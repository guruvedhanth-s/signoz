package slo

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/source"
)

// fakeMetrics is keyed by MetricQuery.Metric rather than a full query
// string: since the engine always derives the builder filter from
// cfg.Service/cfg.Environment, the metric name alone is enough to
// distinguish "good" from "total" reads in these tests.
type fakeMetrics struct {
	Values map[string]float64
	Errors map[string]error
}

func (f fakeMetrics) ScalarBuilder(_ context.Context, query source.MetricQuery, _, _ uint64) (float64, error) {
	if err := f.Errors[query.Metric]; err != nil {
		return 0, err
	}
	value, ok := f.Values[query.Metric]
	if !ok {
		return 0, errors.New("missing fake metric: " + query.Metric)
	}
	return value, nil
}

type fakeGate struct {
	Result GateResult
}

func (f fakeGate) Check(context.Context, GateRequest) (GateResult, error) {
	return f.Result, nil
}

func TestEngineHealthyAndUnhealthy(t *testing.T) {
	metrics := fakeMetrics{Values: map[string]float64{"good": 995, "total": 1000}}
	engine := NewEngine(metrics, nil)
	engine.Now = func() time.Time { return time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC) }
	cfg := Config{Service: "checkout-api", Environment: "test", SLOs: []Definition{{
		Name: "success", Type: SLITypeRatio, Target: 0.995, Window: "1h",
		GoodMetric: "good", TotalMetric: "total",
	}}}
	reports, err := engine.Evaluate(context.Background(), cfg, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if reports[0].State != StateHealthy || math.Abs(reports[0].SLI-0.995) > 1e-9 || math.Abs(reports[0].BurnRate-1) > 1e-9 {
		t.Fatalf("unexpected healthy report: %+v", reports[0])
	}

	metrics.Values["good"] = 800
	reports, err = engine.Evaluate(context.Background(), cfg, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if reports[0].State != StateUnhealthy || math.Abs(reports[0].SLI-0.8) > 1e-9 {
		t.Fatalf("unexpected unhealthy report: %+v", reports[0])
	}
}

func TestEngineCompletenessFailureIsIndeterminate(t *testing.T) {
	metrics := fakeMetrics{Values: map[string]float64{"good": 995, "total": 1000}}
	engine := NewEngine(metrics, fakeGate{Result: GateResult{
		Coverage: 0.5, QueryComplete: true, Trusted: false, Reason: "missing dependency",
	}})
	cfg := Config{Service: "checkout-api", Environment: "test", SLOs: []Definition{{
		Name: "success", Type: SLITypeRatio, Target: 0.99, Window: "1h",
		GoodMetric: "good", TotalMetric: "total", RequiresCompleteness: true,
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

// capturingGate records the GateRequest it was called with, for asserting
// what the engine forwarded to it.
type capturingGate struct {
	Result  GateResult
	Request GateRequest
}

func (g *capturingGate) Check(_ context.Context, request GateRequest) (GateResult, error) {
	g.Request = request
	return g.Result, nil
}

// TestEngineForwardsDefinitionLabelOverridesToGate locks in a
// per-Definition ServiceLabel/EnvironmentLabel override reaching the
// completeness gate, not just the SLI query - live-verified necessary
// because a Config commonly mixes custom-instrumented counters
// ("service_name"/"environment") with SigNoz's own spanmetrics-derived
// metrics (OTel resource semantic-convention keys "service.name"/
// "deployment.environment" instead), and a definition requiring
// completeness on the latter needs the gate scoped the same way the SLI
// query is.
func TestEngineForwardsDefinitionLabelOverridesToGate(t *testing.T) {
	gate := &capturingGate{Result: GateResult{Coverage: 1, QueryComplete: true}}
	engine := NewEngine(fakeMetrics{Values: map[string]float64{"good": 5, "total": 10}}, gate)
	cfg := Config{Service: "support-agent", Environment: "local", SLOs: []Definition{{
		Name: "span-latency", Type: SLITypeRatio, Target: 0.5, Window: "1h",
		GoodMetric: "good", TotalMetric: "total", RequiresCompleteness: true,
		Dependencies:     []string{"signoz_latency"},
		ServiceLabel:     "service.name",
		EnvironmentLabel: "deployment.environment",
	}}}
	if _, err := engine.Evaluate(context.Background(), cfg, time.Now()); err != nil {
		t.Fatal(err)
	}
	if gate.Request.ServiceLabel != "service.name" || gate.Request.EnvironmentLabel != "deployment.environment" {
		t.Fatalf("gate request labels = %q/%q, want the per-definition override",
			gate.Request.ServiceLabel, gate.Request.EnvironmentLabel)
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
	gate := &capturingGate{Result: GateResult{Coverage: 1, QueryComplete: true}}
	engine := NewEngine(fakeMetrics{Values: map[string]float64{"good": 995, "total": 1000}}, gate)

	historicalNow := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	cfg := Config{Service: "checkout-api", Environment: "test", SLOs: []Definition{{
		Name: "success", Type: SLITypeRatio, Target: 0.99, Window: "1h",
		GoodMetric: "good", TotalMetric: "total", RequiresCompleteness: true,
		Dependencies: []string{"requests"},
	}}}
	if _, err := engine.Evaluate(context.Background(), cfg, historicalNow); err != nil {
		t.Fatal(err)
	}
	if !gate.Request.Now.Equal(historicalNow) {
		t.Fatalf("gate request Now = %v, want the evaluation's historical now %v", gate.Request.Now, historicalNow)
	}
}

func TestEngineNoDataIsIndeterminate(t *testing.T) {
	engine := NewEngine(fakeMetrics{Values: map[string]float64{"total": 0}}, nil)
	cfg := Config{Service: "checkout-api", Environment: "test", SLOs: []Definition{{
		Name: "success", Type: SLITypeRatio, Target: 0.99, Window: "1h",
		GoodMetric: "good", TotalMetric: "total",
	}}}
	reports, err := engine.Evaluate(context.Background(), cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if reports[0].State != StateIndeterminate || reports[0].Error == "" {
		t.Fatalf("expected no-data indeterminate report: %+v", reports[0])
	}
}

func TestEngineOwnsCompletenessThresholdPolicy(t *testing.T) {
	engine := NewEngine(
		fakeMetrics{Values: map[string]float64{"good": 99, "total": 100}},
		fakeGate{Result: GateResult{Coverage: 0.5, QueryComplete: true}},
	)
	cfg := Config{Service: "checkout-api", Environment: "test", SLOs: []Definition{{
		Name: "success", Type: SLITypeRatio, Target: 0.99, Window: "1h",
		GoodMetric: "good", TotalMetric: "total", RequiresCompleteness: true,
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
