package slo

import (
	"context"
	"testing"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/source"
)

type captureMetrics struct {
	Queries    []source.MetricQuery
	Start, End uint64
}

func (c *captureMetrics) ScalarBuilder(_ context.Context, query source.MetricQuery, start, end uint64) (float64, error) {
	c.Queries = append(c.Queries, query)
	c.Start, c.End = start, end
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

// TestMetricPresenceGateUsesRequestNowForHistoricalWindows locks in the fix
// for a bug where the gate always scoped its presence query to the gate's
// own wall clock (g.Now), ignoring the evaluation instant Engine.Evaluate
// (and, via it, a historical SLORequest.Now from the API) was actually
// scoring against. A completeness check for a past window must query that
// window's telemetry, not whatever the gate's clock reads when it happens
// to run - otherwise a historical evaluation gets a completeness verdict
// for today's data instead of the window being evaluated.
func TestMetricPresenceGateUsesRequestNowForHistoricalWindows(t *testing.T) {
	querier := &captureMetrics{}
	gate := NewMetricPresenceGate(querier, nil)
	// The gate's own clock says "now" is 2026-07-25, far from the window
	// actually being evaluated.
	gate.Now = func() time.Time { return time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC) }

	historicalNow := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	window := time.Hour
	_, err := gate.Check(context.Background(), GateRequest{
		Service:      "checkout-api",
		Environment:  "test",
		Window:       window,
		Dependencies: []string{"requests"},
		Now:          historicalNow,
	})
	if err != nil {
		t.Fatal(err)
	}

	wantEnd := uint64(historicalNow.UnixMilli())
	wantStart := uint64(historicalNow.Add(-window).UnixMilli())
	if querier.End != wantEnd || querier.Start != wantStart {
		t.Fatalf("gate queried [%d,%d], want the request's historical window [%d,%d] (got the gate's own clock instead)",
			querier.Start, querier.End, wantStart, wantEnd)
	}
}

// TestMetricPresenceGateFallsBackToOwnClockWhenNowUnset preserves the
// existing behavior for callers (including tests) that never set
// GateRequest.Now: the gate falls back to its own clock exactly as before.
func TestMetricPresenceGateFallsBackToOwnClockWhenNowUnset(t *testing.T) {
	querier := &captureMetrics{}
	gate := NewMetricPresenceGate(querier, nil)
	fixedNow := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	gate.Now = func() time.Time { return fixedNow }

	window := time.Hour
	_, err := gate.Check(context.Background(), GateRequest{
		Service:      "checkout-api",
		Environment:  "test",
		Window:       window,
		Dependencies: []string{"requests"},
	})
	if err != nil {
		t.Fatal(err)
	}

	wantEnd := uint64(fixedNow.UnixMilli())
	wantStart := uint64(fixedNow.Add(-window).UnixMilli())
	if querier.End != wantEnd || querier.Start != wantStart {
		t.Fatalf("gate queried [%d,%d], want the gate's own clock's window [%d,%d]",
			querier.Start, querier.End, wantStart, wantEnd)
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
