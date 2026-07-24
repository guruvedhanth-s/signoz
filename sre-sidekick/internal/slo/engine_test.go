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
