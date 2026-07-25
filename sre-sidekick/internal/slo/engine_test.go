package slo

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

type fakeScalar struct {
	Values map[string]float64
	Errors map[string]error
}

func (f fakeScalar) Scalar(_ context.Context, expression string, _, _ uint64) (float64, error) {
	if err := f.Errors[expression]; err != nil {
		return 0, err
	}
	value, ok := f.Values[expression]
	if !ok {
		return 0, errors.New("missing fake query: " + expression)
	}
	return value, nil
}

type fakeGate struct {
	Result   GateResult
	Requests []GateRequest
}

func (f *fakeGate) Check(_ context.Context, request GateRequest) (GateResult, error) {
	f.Requests = append(f.Requests, request)
	return f.Result, nil
}

func TestEngineHealthyAndUnhealthy(t *testing.T) {
	goodQuery := `increase(good_total{service_name="checkout-api",environment="test"}[1h])`
	totalQuery := `increase(request_total{service_name="checkout-api",environment="test"}[1h])`
	query := fakeScalar{Values: map[string]float64{goodQuery: 995, totalQuery: 1000}}
	engine := NewEngine(query, nil)
	engine.Now = func() time.Time { return time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC) }
	cfg := Config{Service: "checkout-api", Environment: "test", SLOs: []Definition{{
		Name: "success", Type: SLITypeRatio, Target: 0.995, Window: "1h",
		GoodQuery: goodQuery, TotalQuery: totalQuery,
	}}}
	reports, err := engine.Evaluate(context.Background(), cfg, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if reports[0].State != StateHealthy || math.Abs(reports[0].SLI-0.995) > 1e-9 || math.Abs(reports[0].BurnRate-1) > 1e-9 {
		t.Fatalf("unexpected healthy report: %+v", reports[0])
	}

	query.Values[goodQuery] = 800
	reports, err = engine.Evaluate(context.Background(), cfg, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if reports[0].State != StateUnhealthy || math.Abs(reports[0].SLI-0.8) > 1e-9 {
		t.Fatalf("unexpected unhealthy report: %+v", reports[0])
	}
}

func TestEngineCompletenessFailureIsIndeterminate(t *testing.T) {
	goodQuery := `increase(good_total{service_name="checkout-api",environment="test"}[1h])`
	totalQuery := `increase(request_total{service_name="checkout-api",environment="test"}[1h])`
	query := fakeScalar{Values: map[string]float64{goodQuery: 995, totalQuery: 1000}}
	engine := NewEngine(query, &fakeGate{Result: GateResult{
		Coverage: 0.5, QueryComplete: true, Trusted: false, Reason: "missing dependency",
	}})
	cfg := Config{Service: "checkout-api", Environment: "test", SLOs: []Definition{{
		Name: "success", Type: SLITypeRatio, Target: 0.99, Window: "1h",
		GoodQuery: goodQuery, TotalQuery: totalQuery, RequiresCompleteness: true,
		Dependencies: []string{"requests"},
	}}}
	reports, err := engine.Evaluate(context.Background(), cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if reports[0].State != StateIndeterminate || reports[0].SLI != 0 {
		t.Fatalf("expected indeterminate report: %+v", reports[0])
	}
}

func TestEngineNoDataIsIndeterminate(t *testing.T) {
	goodQuery := `increase(good_total{service_name="checkout-api",environment="test"}[1h])`
	totalQuery := `increase(request_total{service_name="checkout-api",environment="test"}[1h])`
	engine := NewEngine(fakeScalar{Values: map[string]float64{totalQuery: 0}}, nil)
	cfg := Config{Service: "checkout-api", Environment: "test", SLOs: []Definition{{
		Name: "success", Type: SLITypeRatio, Target: 0.99, Window: "1h",
		GoodQuery: goodQuery, TotalQuery: totalQuery,
	}}}
	reports, err := engine.Evaluate(context.Background(), cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if reports[0].State != StateIndeterminate || reports[0].Error == "" {
		t.Fatalf("expected no-data indeterminate report: %+v", reports[0])
	}
}

// TestEngineForwardsEvaluationNowToGate locks in the fix for a bug where
// the completeness gate always scoped its presence check to its own wall
// clock, ignoring the `now` Engine.Evaluate was actually scoring the SLI
// against - so a historical evaluation (e.g. server.go's SLORequest.Now)
// got a completeness verdict for the current moment instead of the window
// under evaluation. Evaluate must pass the same `now` it uses for the SLI
// query into GateRequest.Now.
func TestEngineForwardsEvaluationNowToGate(t *testing.T) {
	goodQuery := `increase(good_total{service_name="checkout-api",environment="test"}[1h])`
	totalQuery := `increase(request_total{service_name="checkout-api",environment="test"}[1h])`
	query := fakeScalar{Values: map[string]float64{goodQuery: 995, totalQuery: 1000}}
	gate := &fakeGate{Result: GateResult{Coverage: 1, QueryComplete: true}}
	engine := NewEngine(query, gate)

	historicalNow := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	cfg := Config{Service: "checkout-api", Environment: "test", SLOs: []Definition{{
		Name: "success", Type: SLITypeRatio, Target: 0.99, Window: "1h",
		GoodQuery: goodQuery, TotalQuery: totalQuery, RequiresCompleteness: true,
		Dependencies: []string{"requests"},
	}}}
	if _, err := engine.Evaluate(context.Background(), cfg, historicalNow); err != nil {
		t.Fatal(err)
	}
	if len(gate.Requests) != 1 {
		t.Fatalf("expected exactly one gate check, got %d", len(gate.Requests))
	}
	if !gate.Requests[0].Now.Equal(historicalNow) {
		t.Fatalf("gate request Now = %v, want the evaluation's historical now %v", gate.Requests[0].Now, historicalNow)
	}
}

func TestEngineOwnsCompletenessThresholdPolicy(t *testing.T) {
	goodQuery := `increase(good_total{service_name="checkout-api",environment="test"}[1h])`
	totalQuery := `increase(request_total{service_name="checkout-api",environment="test"}[1h])`
	engine := NewEngine(
		fakeScalar{Values: map[string]float64{goodQuery: 99, totalQuery: 100}},
		&fakeGate{Result: GateResult{Coverage: 0.5, QueryComplete: true}},
	)
	cfg := Config{Service: "checkout-api", Environment: "test", SLOs: []Definition{{
		Name: "success", Type: SLITypeRatio, Target: 0.99, Window: "1h",
		GoodQuery: goodQuery, TotalQuery: totalQuery, RequiresCompleteness: true,
		CompletenessThreshold: 0.5, Dependencies: []string{"requests"},
	}}}

	reports, err := engine.Evaluate(context.Background(), cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if reports[0].State != StateHealthy || !reports[0].Gate.Trusted {
		t.Fatalf("threshold policy was not applied by engine: %+v", reports[0])
	}
}
