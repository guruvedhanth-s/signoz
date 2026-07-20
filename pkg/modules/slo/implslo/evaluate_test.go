package implslo

import (
	"context"
	"testing"
	"time"

	"github.com/SigNoz/signoz/pkg/modules/slo"
	"github.com/SigNoz/signoz/pkg/types/slotypes"
	"github.com/SigNoz/signoz/pkg/valuer"
)

// stubConfigProvider returns a fixed config.
type stubConfigProvider struct{ cfg *slotypes.Config }

func (s stubConfigProvider) Load(context.Context) (*slotypes.Config, error) { return s.cfg, nil }

// stubGate returns a fixed completeness value.
type stubGate struct{ completeness float64 }

func (g stubGate) Completeness(context.Context, valuer.UUID, string, slotypes.Window) (float64, error) {
	return g.completeness, nil
}

func newTestModule(scalar ScalarQuerier, gate slo.CompletenessGate, cfg *slotypes.Config) *module {
	return &module{
		scalar:        scalar,
		gate:          gate,
		config:        stubConfigProvider{cfg: cfg},
		gateThreshold: defaultGateThreshold,
		now:           func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
}

func ratioConfig(requiresCompleteness bool) *slotypes.Config {
	return &slotypes.Config{
		Service: "support-agent",
		SLOs: []slotypes.SLODefinition{{
			Name:                 "successful-agent-runs",
			Type:                 slotypes.SLITypeRatio,
			Target:               0.99,
			Window:               "30d",
			GoodQuery:            "good",
			TotalQuery:           "total",
			RequiresCompleteness: requiresCompleteness,
		}},
	}
}

func TestListSLOsHealthy(t *testing.T) {
	scalar := stubScalarQuerier{values: map[string]float64{"good": 9950, "total": 10000}}
	m := newTestModule(scalar, stubGate{completeness: 1.0}, ratioConfig(true))

	reports, err := m.ListSLOs(context.Background(), valuer.GenerateUUID())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(reports) != 1 {
		t.Fatalf("expected 1 report, got %d", len(reports))
	}
	r := reports[0]
	if r.State != slotypes.StateHealthy {
		t.Fatalf("state = %v, want healthy", r.State)
	}
	if r.SLI != 0.995 {
		t.Fatalf("sli = %v, want 0.995", r.SLI)
	}
	// error rate 0.5% against a 1% budget -> half the budget consumed.
	if r.ErrorBudgetRemaining <= 0.49 || r.ErrorBudgetRemaining >= 0.51 {
		t.Fatalf("remaining budget = %v, want ~0.5", r.ErrorBudgetRemaining)
	}
}

func TestListSLOsIndeterminateWhenTelemetryIncomplete(t *testing.T) {
	// The demo money-shot: the SLI would pass, but incomplete telemetry forces
	// indeterminate instead of a false green.
	scalar := stubScalarQuerier{values: map[string]float64{"good": 9950, "total": 10000}}
	m := newTestModule(scalar, stubGate{completeness: 0.40}, ratioConfig(true))

	reports, err := m.ListSLOs(context.Background(), valuer.GenerateUUID())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reports[0].State != slotypes.StateIndeterminate {
		t.Fatalf("state = %v, want indeterminate", reports[0].State)
	}
	if reports[0].SLI != 0 {
		t.Fatalf("sli = %v, want 0 (not computed when gated)", reports[0].SLI)
	}
}

func TestListSLOsUnhealthy(t *testing.T) {
	scalar := stubScalarQuerier{values: map[string]float64{"good": 9500, "total": 10000}}
	m := newTestModule(scalar, stubGate{completeness: 1.0}, ratioConfig(true))

	reports, _ := m.ListSLOs(context.Background(), valuer.GenerateUUID())
	if reports[0].State != slotypes.StateUnhealthy {
		t.Fatalf("state = %v, want unhealthy", reports[0].State)
	}
}

func TestListSLOsNoDataIsIndeterminate(t *testing.T) {
	// total=0 -> evaluateRatio errors -> the SLO becomes indeterminate, not a pass.
	scalar := stubScalarQuerier{values: map[string]float64{"good": 0, "total": 0}}
	m := newTestModule(scalar, stubGate{completeness: 1.0}, ratioConfig(false))

	reports, _ := m.ListSLOs(context.Background(), valuer.GenerateUUID())
	if reports[0].State != slotypes.StateIndeterminate {
		t.Fatalf("state = %v, want indeterminate on no data", reports[0].State)
	}
}
