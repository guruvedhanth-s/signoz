package slo

import (
	"context"
	"strings"
	"testing"
	"time"
)

type captureScalar struct {
	Expression string
	Start, End uint64
}

func (c *captureScalar) Scalar(_ context.Context, expression string, start, end uint64) (float64, error) {
	c.Expression = expression
	c.Start, c.End = start, end
	return 1, nil
}

func TestMetricPresenceGateScopesServiceAndEnvironment(t *testing.T) {
	querier := &captureScalar{}
	gate := NewMetricPresenceGate(querier, nil)
	gate.Now = func() time.Time { return time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC) }
	result, err := gate.Check(context.Background(), GateRequest{
		Service:      "checkout-api",
		Environment:  "test",
		Window:       time.Hour,
		Dependencies: []string{"requests{region=\"local\"}"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.QueryComplete || result.Coverage != 1 {
		t.Fatalf("unexpected gate result: %+v", result)
	}
	for _, expected := range []string{"service_name=\"checkout-api\"", "environment=\"test\"", "region=\"local\""} {
		if !strings.Contains(querier.Expression, expected) {
			t.Fatalf("scoped expression %q does not contain %q", querier.Expression, expected)
		}
	}
	if !strings.Contains(querier.Expression, "count_over_time(") ||
		!strings.Contains(querier.Expression, "[3600s]") {
		t.Fatalf("presence query does not cover the requested window: %q", querier.Expression)
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
	querier := &captureScalar{}
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
	querier := &captureScalar{}
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
