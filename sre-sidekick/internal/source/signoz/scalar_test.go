package signoz

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/source"
)

// decodedBuilderRequest is a minimal view of the request body ScalarBuilder
// sends, enough to assert it is a builder_query with the right metric,
// filter, and aggregation shape.
type decodedBuilderRequest struct {
	RequestType    string `json:"requestType"`
	CompositeQuery struct {
		Queries []struct {
			Type string `json:"type"`
			Spec struct {
				Signal       string `json:"signal"`
				Aggregations []struct {
					MetricName       string `json:"metricName"`
					Temporality      string `json:"temporality"`
					TimeAggregation  string `json:"timeAggregation"`
					SpaceAggregation string `json:"spaceAggregation"`
					ReduceTo         string `json:"reduceTo"`
				} `json:"aggregations"`
				Filter struct {
					Expression string `json:"expression"`
				} `json:"filter"`
			} `json:"spec"`
		} `json:"queries"`
	} `json:"compositeQuery"`
}

func TestScalarBuilderSendsBuilderQueryAndExtractsScalar(t *testing.T) {
	var captured decodedBuilderRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatal(err)
		}
		writeScalarBuilderResponse(t, w, 720)
	}))
	defer server.Close()

	client := NewClient(server.URL, "key")
	value, err := client.ScalarBuilder(context.Background(), source.MetricQuery{
		Metric: "agent_evaluated_answers_total",
		Filter: "service_name = 'support-agent' AND environment = 'local'",
	}, 1000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if value != 720 {
		t.Fatalf("expected scalar 720, got %v", value)
	}

	if captured.RequestType != "scalar" {
		t.Fatalf("expected scalar request type, got %q", captured.RequestType)
	}
	if len(captured.CompositeQuery.Queries) != 1 {
		t.Fatalf("expected exactly one query, got %d", len(captured.CompositeQuery.Queries))
	}
	query := captured.CompositeQuery.Queries[0]
	if query.Type != "builder_query" {
		t.Fatalf("expected builder_query type, got %q", query.Type)
	}
	if query.Spec.Signal != "metrics" {
		t.Fatalf("expected metrics signal, got %q", query.Spec.Signal)
	}
	if len(query.Spec.Aggregations) != 1 {
		t.Fatalf("expected exactly one aggregation, got %d", len(query.Spec.Aggregations))
	}
	aggregation := query.Spec.Aggregations[0]
	if aggregation.MetricName != "agent_evaluated_answers_total" {
		t.Fatalf("unexpected metric name: %q", aggregation.MetricName)
	}
	// Defaults applied when the MetricQuery leaves aggregation fields unset.
	if aggregation.TimeAggregation != "increase" || aggregation.SpaceAggregation != "sum" || aggregation.Temporality != "Cumulative" {
		t.Fatalf("unexpected default aggregation shape: %+v", aggregation)
	}
	if aggregation.ReduceTo != "sum" {
		t.Fatalf("expected reduceTo sum so the builder response collapses to one scalar, got %q", aggregation.ReduceTo)
	}
	if query.Spec.Filter.Expression != "service_name = 'support-agent' AND environment = 'local'" {
		t.Fatalf("unexpected filter expression: %q", query.Spec.Filter.Expression)
	}
}

func TestScalarBuilderHonorsExplicitAggregationFields(t *testing.T) {
	var captured decodedBuilderRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatal(err)
		}
		writeScalarBuilderResponse(t, w, 3)
	}))
	defer server.Close()

	client := NewClient(server.URL, "key")
	value, err := client.ScalarBuilder(context.Background(), source.MetricQuery{
		Metric:           "agent_grounded_answers_total",
		Filter:           "service_name = 'support-agent' AND environment = 'local'",
		TimeAggregation:  "count",
		SpaceAggregation: "sum",
		Temporality:      "Delta",
	}, 1000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if value != 3 {
		t.Fatalf("expected scalar 3, got %v", value)
	}
	aggregation := captured.CompositeQuery.Queries[0].Spec.Aggregations[0]
	if aggregation.TimeAggregation != "count" || aggregation.Temporality != "Delta" {
		t.Fatalf("explicit aggregation fields were not sent: %+v", aggregation)
	}
}

func TestScalarBuilderReturnsZeroForEmptyResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"data":{"results":[{"queryName":"A","columns":null,"data":[]}]}}}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "key")
	value, err := client.ScalarBuilder(context.Background(), source.MetricQuery{
		Metric: "agent_grounded_answers_total",
		Filter: "service_name = 'support-agent' AND environment = 'local'",
	}, 1000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if value != 0 {
		t.Fatalf("expected zero scalar for an empty result, got %v", value)
	}
}

// TestScalarBuilderHandlesGroupColumnAlongsideAggregation locks in the fix
// for a bug where a data row was decoded as [][]float64 unconditionally,
// but a query filtered on a group dimension (e.g. a latency_threshold
// SLI's "le" bucket filter) still comes back with that dimension as its
// own "group"-typed column alongside the "aggregation" column - confirmed
// live against a running SigNoz instance: filtering signoz_latency.bucket
// by le='1000' still returned a row shaped ["1000", <count>], not just
// [<count>]. The old [][]float64 decode failed outright the moment any row
// contained that string value; scalar() must skip non-"aggregation"
// columns and only decode the ones it actually sums.
func TestScalarBuilderHandlesGroupColumnAlongsideAggregation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := map[string]any{
			"status": "success",
			"data": map[string]any{
				"data": map[string]any{
					"results": []any{
						map[string]any{
							"queryName": "A",
							"columns": []any{
								map[string]any{"name": "le", "columnType": "group"},
								map[string]any{"name": "__result_0", "columnType": "aggregation"},
							},
							"data": []any{[]any{"1000", 11721}},
						},
					},
				},
			},
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Fatal(err)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, "key")
	value, err := client.ScalarBuilder(context.Background(), source.MetricQuery{
		Metric: "signoz_latency.bucket",
		Filter: "service.name = 'support-agent' AND le = '1000'",
	}, 1000, 2000)
	if err != nil {
		t.Fatalf("ScalarBuilder: %v", err)
	}
	if value != 11721 {
		t.Fatalf("expected scalar 11721, got %v", value)
	}
}

// TestScalarBuilderErrorsOnNonSuccessStatus locks in the fix for a bug
// where scalarBuilderResponse.scalar() never looked at the response's
// application-level Status field: a malformed query or an internal
// query-engine failure can come back as HTTP 200 with status "error" and
// empty results, which parsed as a silent scalar of 0 - indistinguishable
// from a metric that genuinely has no samples in the window. ScalarBuilder
// must return an error here instead.
func TestScalarBuilderErrorsOnNonSuccessStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"error","error":"query execution failed","data":{"data":{"results":[]}}}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "key")
	_, err := client.ScalarBuilder(context.Background(), source.MetricQuery{
		Metric: "agent_grounded_answers_total",
		Filter: "service_name = 'support-agent' AND environment = 'local'",
	}, 1000, 2000)
	if err == nil {
		t.Fatal("expected an error for a response with status \"error\", got nil (silently parsed as scalar 0)")
	}
}

// writeScalarBuilderResponse writes a response in SigNoz's ScalarData shape
// (aggregation columns plus a single data row), matching what SigNoz
// returns for a builder_query scalar request with reduceTo set.
func writeScalarBuilderResponse(t *testing.T, w http.ResponseWriter, value float64) {
	t.Helper()
	writeScalarBuilderResponseWithWarning(t, w, value, "")
}

func writeScalarBuilderResponseWithWarning(t *testing.T, w http.ResponseWriter, value float64, warning string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	data := map[string]any{
		"data": map[string]any{
			"results": []any{
				map[string]any{
					"queryName": "A",
					"columns":   []any{map[string]any{"name": "__result_0", "columnType": "aggregation"}},
					"data":      []any{[]any{value}},
				},
			},
		},
	}
	if warning != "" {
		data["warning"] = map[string]any{"message": warning}
	}
	body := map[string]any{"status": "success", "data": data}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatal(err)
	}
}

// TestScalarBuilderWarning_ParsesTheTopLevelWarning locks in PRD section
// 11.2 ("preserve SigNoz query-completeness metadata"): the v5 response's
// top-level data.warning.message - the only completeness-adjacent field
// SigNoz's ScalarData response shape actually carries (confirmed against
// querybuildertypesv5.QueryRangeResponse.Warning; there is no per-row/
// per-series partial flag on the scalar shape) - must round-trip through
// unchanged.
func TestScalarBuilderWarning_ParsesTheTopLevelWarning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeScalarBuilderResponseWithWarning(t, w, 42, "metric agent_requests_total has gone dormant")
	}))
	defer server.Close()

	client := NewClient(server.URL, "key")
	value, warning, err := client.ScalarBuilderWarning(context.Background(), source.MetricQuery{Metric: "agent_requests_total"}, 1000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if value != 42 {
		t.Errorf("value = %v, want 42", value)
	}
	if warning != "metric agent_requests_total has gone dormant" {
		t.Errorf("warning = %q, want SigNoz's dormant-metric message", warning)
	}
}

func TestScalarBuilderWarning_NoWarningIsEmptyString(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeScalarBuilderResponse(t, w, 42)
	}))
	defer server.Close()

	client := NewClient(server.URL, "key")
	_, warning, err := client.ScalarBuilderWarning(context.Background(), source.MetricQuery{Metric: "x"}, 1000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if warning != "" {
		t.Errorf("warning = %q, want empty when SigNoz raised none", warning)
	}
}

// ScalarBuilder itself (the plain MetricQuerier method) must behave
// identically to before this change - it just discards the warning.
func TestScalarBuilder_StillWorksUnchangedWhenAWarningIsPresent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeScalarBuilderResponseWithWarning(t, w, 42, "some warning")
	}))
	defer server.Close()

	client := NewClient(server.URL, "key")
	value, err := client.ScalarBuilder(context.Background(), source.MetricQuery{Metric: "x"}, 1000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if value != 42 {
		t.Errorf("value = %v, want 42", value)
	}
}
