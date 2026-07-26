package signoz

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/profile"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/source"
)

// metricsRequest is the part of a v5 metrics query these tests assert on.
type metricsRequest struct {
	RequestType    string `json:"requestType"`
	CompositeQuery struct {
		Queries []struct {
			Spec struct {
				Signal       string `json:"signal"`
				Aggregations []struct {
					MetricName       string `json:"metricName"`
					TimeAggregation  string `json:"timeAggregation"`
					SpaceAggregation string `json:"spaceAggregation"`
					Expression       string `json:"expression"`
				} `json:"aggregations"`
				Filter struct {
					Expression string `json:"expression"`
				} `json:"filter"`
				GroupBy []struct {
					Name string `json:"name"`
				} `json:"groupBy"`
			} `json:"spec"`
		} `json:"queries"`
	} `json:"compositeQuery"`
}

func decodeMetricsRequest(t *testing.T, r *http.Request) metricsRequest {
	t.Helper()
	var request metricsRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		t.Errorf("decode request: %v", err)
	}
	return request
}

// metricsSeriesResponse renders one labelled series with one sample.
func metricsSeriesResponse(labels map[string]string, at time.Time, value float64) map[string]any {
	labelList := make([]any, 0, len(labels))
	for key, val := range labels {
		labelList = append(labelList, map[string]any{"key": map[string]any{"name": key}, "value": val})
	}
	return map[string]any{"status": "success", "data": map[string]any{"data": map[string]any{
		"results": []any{map[string]any{"aggregations": []any{map[string]any{
			"series": []any{map[string]any{
				"labels": labelList,
				"values": []any{map[string]any{"timestamp": at.UnixMilli(), "value": value}},
			}},
		}}}},
	}}}
}

func emptyMetricsResponse() map[string]any {
	return map[string]any{"status": "success", "data": map[string]any{"data": map[string]any{
		"results": []any{map[string]any{"aggregations": []any{map[string]any{"series": []any{}}}}},
	}}}
}

func metricsProfile(fields []profile.FieldSpec, rules []profile.RuleSpec) profile.Profile {
	return profile.Profile{
		Metadata: profile.Metadata{Service: "checkout", Environment: "prod"},
		Spec: profile.Spec{
			Signals:    profile.Signals{Metrics: profile.SignalSpec{Fields: fields}},
			AuditRules: rules,
		},
	}
}

func metricsTarget() source.Target {
	return source.Target{
		Service: "checkout", Environment: "prod",
		Start: time.Now().Add(-15 * time.Minute), End: time.Now(),
	}
}

// The v5 API rejects the record-shaped {"expression": "count()"} aggregation
// for the metrics signal ("unknown field expression in query spec for
// MetricAggregation"), because a metrics query aggregates a named series
// rather than scanning rows. That failure cleared Snapshot.QueryComplete and
// made every profile declaring a metrics signal undiagnosable.
func TestQueryMetricsSendsATimeSeriesMetricAggregation(t *testing.T) {
	var request metricsRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request = decodeMetricsRequest(t, r)
		_ = json.NewEncoder(w).Encode(metricsSeriesResponse(
			map[string]string{"http.request.method": "GET"}, time.Now(), 1.5))
	}))
	defer server.Close()

	p := metricsProfile(
		[]profile.FieldSpec{{Path: "http.server.request.count", Type: "counter"}},
		[]profile.RuleSpec{{ID: "c", Type: "cardinality", Signal: "metrics", Field: "http.request.method", MaxDistinctValues: 20}},
	)
	snapshot, err := NewTelemetrySource(NewClient(server.URL, "key"), 200).
		queryMetrics(context.Background(), p, metricsTarget())
	if err != nil {
		t.Fatal(err)
	}

	if request.RequestType != "time_series" {
		t.Errorf("requestType = %q, want time_series", request.RequestType)
	}
	spec := request.CompositeQuery.Queries[0].Spec
	if spec.Signal != "metrics" {
		t.Errorf("signal = %q, want metrics", spec.Signal)
	}
	aggregation := spec.Aggregations[0]
	if aggregation.Expression != "" {
		t.Errorf("aggregation must not carry a count() expression for metrics, got %q", aggregation.Expression)
	}
	if aggregation.MetricName != "http.server.request.count" {
		t.Errorf("metricName = %q", aggregation.MetricName)
	}
	// SigNoz rejects "sum" as a time aggregation on a monotonic counter.
	if aggregation.TimeAggregation != "rate" {
		t.Errorf("counter timeAggregation = %q, want rate", aggregation.TimeAggregation)
	}
	if len(spec.GroupBy) != 1 || spec.GroupBy[0].Name != "http.request.method" {
		t.Errorf("cardinality rules must be grouped by, got %+v", spec.GroupBy)
	}
	if !snapshot.QueryComplete {
		t.Error("a successful metrics query must leave QueryComplete true")
	}
	if snapshot.DistinctValues["http.request.method"] != 1 {
		t.Errorf("DistinctValues = %v, want the label counted", snapshot.DistinctValues)
	}
	if snapshot.LastSeen["http.server.request.count"].IsZero() {
		t.Error("freshness needs a LastSeen entry keyed by the declared metric name")
	}
}

// A histogram's base name returns zero series; the data lives in its .count
// child. Confirmed live against signoz_latency.
func TestQueryMetricsReadsHistogramCountChild(t *testing.T) {
	var names []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeMetricsRequest(t, r)
		names = append(names, request.CompositeQuery.Queries[0].Spec.Aggregations[0].MetricName)
		_ = json.NewEncoder(w).Encode(metricsSeriesResponse(nil, time.Now(), 2))
	}))
	defer server.Close()

	p := metricsProfile([]profile.FieldSpec{
		{Path: "http.server.request.duration", Type: "histogram"},
	}, nil)
	snapshot, err := NewTelemetrySource(NewClient(server.URL, "key"), 200).
		queryMetrics(context.Background(), p, metricsTarget())
	if err != nil {
		t.Fatal(err)
	}

	if len(names) == 0 || names[0] != "http.server.request.duration.count" {
		t.Errorf("histogram must be queried via its .count child, got %v", names)
	}
	// LastSeen stays keyed by the *declared* name so the freshness rule,
	// which knows nothing about child series, still finds it.
	if snapshot.LastSeen["http.server.request.duration"].IsZero() {
		t.Errorf("LastSeen must be keyed by the declared metric name, got %v", snapshot.LastSeen)
	}
}

// Metric label spellings differ by family: SDK counters resolve under
// resource.*, spanmetrics series carry the bare semantic-convention keys, and
// sending a key a metric lacks makes SigNoz reject the query outright rather
// than return nothing. The query tries the known spellings and keeps the first
// that parses and has data - never an unscoped query.
func TestQueryMetricsFallsBackThroughScopeLabelSpellings(t *testing.T) {
	var filters []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeMetricsRequest(t, r)
		filter := request.CompositeQuery.Queries[0].Spec.Filter.Expression
		filters = append(filters, filter)
		// Mimic SigNoz rejecting the resource.* spelling for this metric.
		if len(filters) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "error",
				"error": map[string]any{"message": "Found 2 errors while parsing the search expression."}})
			return
		}
		_ = json.NewEncoder(w).Encode(metricsSeriesResponse(nil, time.Now(), 1))
	}))
	defer server.Close()

	p := metricsProfile([]profile.FieldSpec{{Path: "signoz_latency", Type: "histogram"}}, nil)
	snapshot, err := NewTelemetrySource(NewClient(server.URL, "key"), 200).
		queryMetrics(context.Background(), p, metricsTarget())
	if err != nil {
		t.Fatal(err)
	}

	if len(filters) < 2 {
		t.Fatalf("a rejected filter spelling must be retried with the next, got %v", filters)
	}
	if !strings.Contains(filters[0], "resource.service.name") {
		t.Errorf("first attempt should use the resource.* spelling, got %q", filters[0])
	}
	if !strings.Contains(filters[1], "service.name = 'checkout'") {
		t.Errorf("second attempt should use the bare spelling, got %q", filters[1])
	}
	for _, filter := range filters {
		if !strings.Contains(filter, "checkout") {
			t.Errorf("every attempt must stay scoped to the service, got %q", filter)
		}
	}
	if !snapshot.QueryComplete {
		t.Error("recovering on a later spelling is a complete query, not a failed one")
	}
}

// A metric that is simply not emitted is not a query failure: SigNoz answers
// success with zero series, and the freshness rule reports "no last-seen
// timestamp" on its own terms. Clearing QueryComplete here would let one
// absent metric veto an entire audit.
func TestQueryMetricsTreatsAnAbsentMetricAsNoDataNotAFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeMetricsRequest(t, r)
		_ = json.NewEncoder(w).Encode(emptyMetricsResponse())
	}))
	defer server.Close()

	p := metricsProfile([]profile.FieldSpec{{Path: "never_emitted_total", Type: "counter"}}, nil)
	snapshot, err := NewTelemetrySource(NewClient(server.URL, "key"), 200).
		queryMetrics(context.Background(), p, metricsTarget())
	if err != nil {
		t.Fatal(err)
	}

	if !snapshot.QueryComplete {
		t.Error("an absent metric must not clear QueryComplete")
	}
	if _, ok := snapshot.LastSeen["never_emitted_total"]; ok {
		t.Error("an absent metric must not get a LastSeen entry")
	}
	if len(snapshot.Metrics) != 0 {
		t.Errorf("no series means no records, got %d", len(snapshot.Metrics))
	}
}
