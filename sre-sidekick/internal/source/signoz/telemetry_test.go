package signoz

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/profile"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/source"
)

func TestTelemetrySourceCollectsTraceAndMetricSignals(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			CompositeQuery struct {
				Queries []struct {
					Spec struct {
						Signal string `json:"signal"`
					} `json:"spec"`
				} `json:"queries"`
			} `json:"compositeQuery"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		signal := request.CompositeQuery.Queries[0].Spec.Signal
		// The two signals have genuinely different response shapes, and the
		// stub has to reflect that: metrics come back as labelled time
		// series, not as rows. Serving rows for metrics is what let the
		// invalid metrics query go unnoticed here in the first place.
		if signal == "metrics" {
			_ = json.NewEncoder(w).Encode(metricsSeriesResponse(
				map[string]string{"service.name": "checkout"}, time.Now(), 1))
			return
		}
		data := map[string]any{"service.name": "checkout", "name": signal + ".span", "value": 1}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"data": map[string]any{"results": []any{map[string]any{"rows": []any{map[string]any{"timestamp": time.Now(), "data": data}}}}}}})
	}))
	defer server.Close()
	p := profile.Profile{Metadata: profile.Metadata{Service: "checkout", Environment: "test"}, Spec: profile.Spec{
		AuditRules: []profile.RuleSpec{{ID: "span", Type: "required_span", Signal: "traces", SpanName: "traces.span", Severity: "blocker"}, {ID: "metric", Type: "freshness", Signal: "metrics", Field: "value", MaxAge: "1h", Severity: "warning"}},
	}}
	snapshot, err := NewTelemetrySource(NewClient(server.URL, "key"), 10).Snapshot(context.Background(), p, source.Target{Service: "checkout", Environment: "test", Start: time.Now().Add(-time.Minute), End: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.SignalAvailable("traces") || !snapshot.SignalAvailable("metrics") || len(snapshot.Traces) != 1 || len(snapshot.Metrics) != 1 {
		t.Fatalf("signals were not collected: %+v", snapshot)
	}
}

// The v5 API rejects the whole query with "field `id` not found" when the
// traces signal is ordered by id: unlike logs, traces have no id field. That
// failed the traces query on every call, which cleared Snapshot.QueryComplete
// and made the RCA evidence gate return indeterminate for every incident.
// Confirmed live against SigNoz; asserted here so the tiebreaker cannot be
// reinstated for traces without failing.
func TestQuerySignalOrdersByIDForLogsOnly(t *testing.T) {
	ordersBySignal := map[string][]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			CompositeQuery struct {
				Queries []struct {
					Spec struct {
						Signal string `json:"signal"`
						Order  []struct {
							Key struct {
								Name string `json:"name"`
							} `json:"key"`
						} `json:"order"`
					} `json:"spec"`
				} `json:"queries"`
			} `json:"compositeQuery"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		spec := request.CompositeQuery.Queries[0].Spec
		var keys []string
		for _, order := range spec.Order {
			keys = append(keys, order.Key.Name)
		}
		ordersBySignal[spec.Signal] = keys
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data":   map[string]any{"data": map[string]any{"results": []any{}}},
		})
	}))
	defer server.Close()

	source := NewTelemetrySource(NewClient(server.URL, "key"), 10)
	target := targetFor("checkout", "test")
	for _, signal := range []string{"logs", "traces"} {
		if _, err := source.querySignal(context.Background(), signal, target); err != nil {
			t.Fatalf("%s query: %v", signal, err)
		}
	}

	if want := []string{"timestamp", "id"}; !equalStrings(ordersBySignal["logs"], want) {
		t.Errorf("logs order = %v, want %v", ordersBySignal["logs"], want)
	}
	if want := []string{"timestamp"}; !equalStrings(ordersBySignal["traces"], want) {
		t.Errorf("traces order = %v, want %v (traces have no id field)", ordersBySignal["traces"], want)
	}
}

func targetFor(service, environment string) source.Target {
	return source.Target{
		Service:     service,
		Environment: environment,
		Start:       time.Now().Add(-time.Minute),
		End:         time.Now(),
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
