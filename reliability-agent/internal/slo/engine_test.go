package slo

import (
	"context"
	"testing"
	"time"
)

type stubScalar struct {
	values map[string]float64
	err    error
}

func (s stubScalar) Scalar(_ context.Context, expr string, _, _ uint64) (float64, error) {
	if s.err != nil {
		return 0, s.err
	}
	return s.values[expr], nil
}

type stubGate struct{ completeness float64 }

func (g stubGate) Completeness(context.Context, string, Window) (float64, error) {
	return g.completeness, nil
}

func testEngine(scalar ScalarQuerier, gate CompletenessGate) *Engine {
	return &Engine{
		scalar:        scalar,
		gate:          gate,
		gateThreshold: DefaultGateThreshold,
		now:           func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
}

func ratioCfg(requires bool) *Config {
	return &Config{
		Service: "support-agent",
		SLOs: []SLODefinition{{
			Name: "successful-agent-runs", Type: SLITypeRatio, Target: 0.99, Window: "30d",
			GoodQuery: "good", TotalQuery: "total", RequiresCompleteness: requires,
		}},
	}
}

func TestEvaluateHealthy(t *testing.T) {
	e := testEngine(stubScalar{values: map[string]float64{"good": 9950, "total": 10000}}, stubGate{1.0})
	r := e.Evaluate(context.Background(), ratioCfg(true))[0]
	if r.State != StateHealthy || r.SLI != 0.995 {
		t.Fatalf("got state=%v sli=%v, want healthy/0.995", r.State, r.SLI)
	}
	if r.ErrorBudgetRemaining <= 0.49 || r.ErrorBudgetRemaining >= 0.51 {
		t.Fatalf("budget remaining = %v, want ~0.5", r.ErrorBudgetRemaining)
	}
}

func TestEvaluateIndeterminateWhenIncomplete(t *testing.T) {
	// The SLI would pass, but incomplete telemetry forces indeterminate.
	e := testEngine(stubScalar{values: map[string]float64{"good": 9950, "total": 10000}}, stubGate{0.40})
	r := e.Evaluate(context.Background(), ratioCfg(true))[0]
	if r.State != StateIndeterminate {
		t.Fatalf("state = %v, want indeterminate", r.State)
	}
	if r.SLI != 0 {
		t.Fatalf("sli = %v, want 0 (not computed when gated)", r.SLI)
	}
}

func TestEvaluateUnhealthy(t *testing.T) {
	e := testEngine(stubScalar{values: map[string]float64{"good": 8000, "total": 10000}}, stubGate{1.0})
	r := e.Evaluate(context.Background(), ratioCfg(true))[0]
	if r.State != StateUnhealthy || r.SLI != 0.8 {
		t.Fatalf("got state=%v sli=%v, want unhealthy/0.8", r.State, r.SLI)
	}
}

func TestEvaluateNoDataIsIndeterminate(t *testing.T) {
	e := testEngine(stubScalar{values: map[string]float64{"good": 0, "total": 0}}, NoopGate{})
	r := e.Evaluate(context.Background(), ratioCfg(false))[0]
	if r.State != StateIndeterminate {
		t.Fatalf("state = %v, want indeterminate on no data", r.State)
	}
}
