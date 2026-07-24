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

// writeScalarBuilderResponse writes a response in SigNoz's ScalarData shape
// (aggregation columns plus a single data row), matching what SigNoz
// returns for a builder_query scalar request with reduceTo set.
func writeScalarBuilderResponse(t *testing.T, w http.ResponseWriter, value float64) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	body := map[string]any{
		"status": "success",
		"data": map[string]any{
			"data": map[string]any{
				"results": []any{
					map[string]any{
						"queryName": "A",
						"columns":   []any{map[string]any{"name": "__result_0", "columnType": "aggregation"}},
						"data":      []any{[]any{value}},
					},
				},
			},
		},
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatal(err)
	}
}
