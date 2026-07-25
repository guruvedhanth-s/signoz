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
	Values   map[string]float64
	Errors   map[string]error
	Warnings map[string]string
}

func (f fakeMetrics) ScalarBuilder(ctx context.Context, query source.MetricQuery, start, end uint64) (float64, error) {
	value, _, err := f.ScalarBuilderWarning(ctx, query, start, end)
	return value, err
}

// ScalarBuilderWarning makes fakeMetrics satisfy source.WarningQuerier
// unconditionally, so every existing test (which never sets Warnings)
// exercises the same type-assertion path scalarWithWarning takes against
// the real *signoz.Client - with an always-empty warning, identical to
// before this method existed.
func (f fakeMetrics) ScalarBuilderWarning(_ context.Context, query source.MetricQuery, _, _ uint64) (float64, string, error) {
	if err := f.Errors[query.Metric]; err != nil {
		return 0, "", err
	}
	value, ok := f.Values[query.Metric]
	if !ok {
		return 0, "", errors.New("missing fake metric: " + query.Metric)
	}
	return value, f.Warnings[query.Metric], nil
}

var _ source.WarningQuerier = fakeMetrics{}

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

func completenessConfig(target float64) Config {
	return Config{Service: "checkout-api", Environment: "test", SLOs: []Definition{{
		Name: "telemetry-coverage", Type: SLITypeCompleteness, Target: target, Window: "1h",
		Dependencies: []string{"requests_total", "errors_total"},
	}}}
}

// A completeness SLI's value is the gate's own coverage fraction, not a
// good/total counter ratio - deriveMetricQueries is never called for it
// (TestDeriveMetricQueries_CompletenessIsUnsupported in sli_test.go).
func TestEvaluateCompleteness_Healthy(t *testing.T) {
	engine := NewEngine(fakeMetrics{}, fakeGate{Result: GateResult{Coverage: 0.99, QueryComplete: true}})
	reports, err := engine.Evaluate(context.Background(), completenessConfig(0.95), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if reports[0].State != StateHealthy {
		t.Fatalf("state = %q, want healthy: %+v", reports[0].State, reports[0])
	}
	if reports[0].SLI != 0.99 {
		t.Errorf("SLI = %v, want the gate's coverage (0.99), not a counter ratio", reports[0].SLI)
	}
	if reports[0].Completeness != 0.99 {
		t.Errorf("Completeness = %v, want 0.99", reports[0].Completeness)
	}
}

func TestEvaluateCompleteness_BelowTargetIsUnhealthyNotIndeterminate(t *testing.T) {
	engine := NewEngine(fakeMetrics{}, fakeGate{Result: GateResult{Coverage: 0.5, QueryComplete: true}})
	reports, err := engine.Evaluate(context.Background(), completenessConfig(0.95), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// The gate answered the question (QueryComplete) - a low coverage is a
	// real, computed unhealthy state, not "we don't know".
	if reports[0].State != StateUnhealthy {
		t.Fatalf("state = %q, want unhealthy (the gate query succeeded, coverage was just low): %+v", reports[0].State, reports[0])
	}
	if reports[0].SLI != 0.5 {
		t.Errorf("SLI = %v, want 0.5", reports[0].SLI)
	}
}

func TestEvaluateCompleteness_IncompleteGateQueryIsIndeterminate(t *testing.T) {
	engine := NewEngine(fakeMetrics{}, fakeGate{Result: GateResult{Coverage: 0.99, QueryComplete: false}})
	reports, err := engine.Evaluate(context.Background(), completenessConfig(0.95), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if reports[0].State != StateIndeterminate || reports[0].Error == "" {
		t.Fatalf("expected an indeterminate report when the gate query itself did not complete: %+v", reports[0])
	}
}

func TestEvaluateCompleteness_NoGateConfiguredIsIndeterminate(t *testing.T) {
	engine := NewEngine(fakeMetrics{}, nil)
	reports, err := engine.Evaluate(context.Background(), completenessConfig(0.95), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if reports[0].State != StateIndeterminate || reports[0].Error == "" {
		t.Fatalf("expected an indeterminate report with no gate configured: %+v", reports[0])
	}
}

func TestEvaluateCompleteness_GateErrorIsIndeterminate(t *testing.T) {
	gateErr := errors.New("dependency query failed")
	engine := NewEngine(fakeMetrics{}, fakeErrGate{err: gateErr})
	reports, err := engine.Evaluate(context.Background(), completenessConfig(0.95), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if reports[0].State != StateIndeterminate || reports[0].Error != gateErr.Error() {
		t.Fatalf("expected the gate error surfaced as indeterminate: %+v", reports[0])
	}
}

type fakeErrGate struct{ err error }

func (f fakeErrGate) Check(context.Context, GateRequest) (GateResult, error) {
	return GateResult{}, f.err
}

// PRD section 11.2: "return the evaluated start and end timestamps".
func TestEngineReportsTheEvaluatedWindow(t *testing.T) {
	metrics := fakeMetrics{Values: map[string]float64{"good": 995, "total": 1000}}
	engine := NewEngine(metrics, nil)
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	engine.Now = func() time.Time { return now }
	cfg := Config{Service: "checkout-api", Environment: "test", SLOs: []Definition{{
		Name: "success", Type: SLITypeRatio, Target: 0.99, Window: "1h",
		GoodMetric: "good", TotalMetric: "total",
	}}}

	reports, err := engine.Evaluate(context.Background(), cfg, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if !reports[0].EvaluatedEnd.Equal(now) {
		t.Errorf("EvaluatedEnd = %v, want %v", reports[0].EvaluatedEnd, now)
	}
	if want := now.Add(-time.Hour); !reports[0].EvaluatedStart.Equal(want) {
		t.Errorf("EvaluatedStart = %v, want %v (window=1h before EvaluatedEnd)", reports[0].EvaluatedStart, want)
	}
}

// An invalid window fails before any query runs. Config.Validate already
// rejects this before Evaluate ever calls evaluate (so this path is not
// reachable through the public API), but evaluate itself must still be
// honest that no window was ever evaluated if it is ever called this way.
func TestEngineReportsNoEvaluatedWindowOnAnInvalidWindow(t *testing.T) {
	engine := NewEngine(fakeMetrics{}, nil)
	cfg := Config{Service: "checkout-api", Environment: "test"}
	definition := Definition{
		Name: "success", Type: SLITypeRatio, Target: 0.99, Window: "not-a-duration",
		GoodMetric: "good", TotalMetric: "total",
	}
	report := engine.evaluate(context.Background(), cfg, definition, time.Now())
	if !report.EvaluatedStart.IsZero() || !report.EvaluatedEnd.IsZero() {
		t.Errorf("EvaluatedStart/End = %v/%v, want zero when the window itself was invalid", report.EvaluatedStart, report.EvaluatedEnd)
	}
	if report.State != StateIndeterminate {
		t.Errorf("State = %q, want indeterminate", report.State)
	}
}

// PRD section 11.2: "preserve SigNoz query-completeness metadata" - a
// warning from either the good or total query surfaces on the report.
func TestEngineSurfacesASigNozWarningFromTheSLIQuery(t *testing.T) {
	metrics := fakeMetrics{
		Values:   map[string]float64{"good": 995, "total": 1000},
		Warnings: map[string]string{"total": "metric total has gone dormant"},
	}
	engine := NewEngine(metrics, nil)
	cfg := Config{Service: "checkout-api", Environment: "test", SLOs: []Definition{{
		Name: "success", Type: SLITypeRatio, Target: 0.99, Window: "1h",
		GoodMetric: "good", TotalMetric: "total",
	}}}
	reports, err := engine.Evaluate(context.Background(), cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if reports[0].Warning != "metric total has gone dormant" {
		t.Errorf("Warning = %q, want the SLI query's warning surfaced", reports[0].Warning)
	}
	// A warning is informational, not fatal - the SLO still computes.
	if reports[0].State != StateHealthy {
		t.Errorf("State = %q, want healthy - a warning must not force indeterminate", reports[0].State)
	}
}

func TestEngineSurfacesASigNozWarningFromTheCompletenessGate(t *testing.T) {
	engine := NewEngine(
		fakeMetrics{Values: map[string]float64{"good": 995, "total": 1000}},
		fakeGate{Result: GateResult{Coverage: 0.99, QueryComplete: true, Warning: "dependency requests_total has gone dormant"}},
	)
	cfg := Config{Service: "checkout-api", Environment: "test", SLOs: []Definition{{
		Name: "success", Type: SLITypeRatio, Target: 0.99, Window: "1h",
		GoodMetric: "good", TotalMetric: "total", RequiresCompleteness: true,
		Dependencies: []string{"requests_total"},
	}}}
	reports, err := engine.Evaluate(context.Background(), cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if reports[0].Warning != "dependency requests_total has gone dormant" {
		t.Errorf("Warning = %q, want the gate's warning surfaced", reports[0].Warning)
	}
}
