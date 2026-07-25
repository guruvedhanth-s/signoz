package slo

import (
	"context"
	"testing"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/source"
)

type captureMetrics struct {
	Queries []source.MetricQuery
}

func (c *captureMetrics) ScalarBuilder(_ context.Context, query source.MetricQuery, _, _ uint64) (float64, error) {
	c.Queries = append(c.Queries, query)
	return 1, nil
}

func TestMetricPresenceGateScopesServiceAndEnvironment(t *testing.T) {
	querier := &captureMetrics{}
	gate := NewMetricPresenceGate(querier, nil)
	gate.Now = func() time.Time { return time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC) }
	result, err := gate.Check(context.Background(), GateRequest{
		Service:      "checkout-api",
		Environment:  "test",
		Window:       time.Hour,
		Dependencies: []string{"requests"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.QueryComplete || result.Coverage != 1 {
		t.Fatalf("unexpected gate result: %+v", result)
	}
	if len(querier.Queries) != 1 {
		t.Fatalf("expected exactly one dependency query, got %d", len(querier.Queries))
	}
	query := querier.Queries[0]
	if query.Metric != "requests" {
		t.Fatalf("unexpected metric: %q", query.Metric)
	}
	if query.Filter != `service_name = 'checkout-api' AND environment = 'test'` {
		t.Fatalf("unexpected filter: %q", query.Filter)
	}
	if query.TimeAggregation != "count" {
		t.Fatalf("presence query must count samples, got aggregation: %+v", query)
	}
}

func TestMetricPresenceGateUsesConfiguredLabels(t *testing.T) {
	querier := &captureMetrics{}
	gate := NewMetricPresenceGate(querier, nil)
	gate.Now = func() time.Time { return time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC) }
	_, err := gate.Check(context.Background(), GateRequest{
		Service:          "checkout-api",
		Environment:      "test",
		Window:           time.Hour,
		Dependencies:     []string{"requests"},
		ServiceLabel:     "svc",
		EnvironmentLabel: "env",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := querier.Queries[0].Filter; got != `svc = 'checkout-api' AND env = 'test'` {
		t.Fatalf("unexpected filter: %q", got)
	}
}

func TestMetricPresenceGateReportsMissingDependency(t *testing.T) {
	gate := NewMetricPresenceGate(missingMetrics{}, nil)
	result, err := gate.Check(context.Background(), GateRequest{
		Service:      "checkout-api",
		Environment:  "test",
		Window:       time.Hour,
		Dependencies: []string{"requests", "errors"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Coverage != 0.5 || !result.QueryComplete {
		t.Fatalf("unexpected partial coverage result: %+v", result)
	}
}

type missingMetrics struct{}

func (missingMetrics) ScalarBuilder(_ context.Context, query source.MetricQuery, _, _ uint64) (float64, error) {
	if query.Metric == "requests" {
		return 12, nil
	}
	return 0, nil
}
