package implslo

import (
	"context"
	"time"

	"github.com/SigNoz/signoz/pkg/errors"
	"github.com/SigNoz/signoz/pkg/factory"
	"github.com/SigNoz/signoz/pkg/modules/dashboard"
	"github.com/SigNoz/signoz/pkg/modules/slo"
	"github.com/SigNoz/signoz/pkg/querier"
	"github.com/SigNoz/signoz/pkg/types/dashboardtypes"
	"github.com/SigNoz/signoz/pkg/types/slotypes"
	"github.com/SigNoz/signoz/pkg/valuer"
)

type module struct {
	scalar        ScalarQuerier
	gate          slo.CompletenessGate
	config        ConfigProvider
	emitter       *emitter
	generator     *generator
	gateThreshold float64
	now           func() time.Time
}

// NewModule constructs the SLO engine.
//
// q executes the SLI queries, gate is the completeness seam to the Telemetry
// Health Auditor (pass NewNoopGate until Track A is available), config supplies
// the SLO definitions, and settings provides the meter used to emit slo.*
// metrics back into SigNoz.
func NewModule(q querier.Querier, gate slo.CompletenessGate, config ConfigProvider, dashboards dashboard.Module, settings factory.ProviderSettings) slo.Module {
	scoped := factory.NewScopedProviderSettings(settings, "github.com/SigNoz/signoz/pkg/modules/slo")
	emit, err := newEmitter(scoped.Meter())
	if err != nil {
		scoped.Logger().Error("failed to create slo metric instruments, emission disabled", "error", err)
		emit = nil
	}
	return &module{
		scalar:        NewScalarQuerier(q),
		gate:          gate,
		config:        config,
		emitter:       emit,
		generator:     newGenerator(dashboards),
		gateThreshold: defaultGateThreshold,
		now:           time.Now,
	}
}

// GenerateDashboard creates or idempotently updates the SLO dashboard.
func (m *module) GenerateDashboard(ctx context.Context, orgID valuer.UUID, createdBy string, creator valuer.UUID) (*dashboardtypes.Dashboard, error) {
	return m.generator.Generate(ctx, orgID, createdBy, creator)
}

// ListSLOs evaluates every configured SLO for an organization.
//
// A single SLO that cannot be evaluated (missing data, query failure) is
// surfaced as an indeterminate report rather than failing the whole list, so one
// bad SLO never hides the others.
func (m *module) ListSLOs(ctx context.Context, orgID valuer.UUID) ([]*slotypes.Report, error) {
	cfg, err := m.config.Load(ctx)
	if err != nil {
		return nil, err
	}

	now := m.now()
	reports := make([]*slotypes.Report, 0, len(cfg.SLOs))
	for _, def := range cfg.SLOs {
		report, err := m.evaluate(ctx, orgID, cfg.Service, def, now)
		if err != nil {
			report = indeterminateReport(cfg.Service, def)
		}
		m.emitter.Emit(ctx, report)
		reports = append(reports, report)
	}
	return reports, nil
}

// evaluate resolves a single SLO into a report.
func (m *module) evaluate(ctx context.Context, orgID valuer.UUID, service string, def slotypes.SLODefinition, now time.Time) (*slotypes.Report, error) {
	dur, err := def.Window.Duration()
	if err != nil {
		return nil, err
	}
	endMs := uint64(now.UnixMilli())
	startMs := uint64(now.Add(-dur).UnixMilli())

	report := &slotypes.Report{
		Name:         def.Name,
		Service:      service,
		Type:         def.Type,
		Target:       def.Target,
		Window:       def.Window,
		Completeness: 1.0,
	}

	// Completeness gate: consult the auditor and short-circuit to indeterminate
	// before spending a query when telemetry cannot be trusted.
	if def.RequiresCompleteness {
		completeness, err := m.gate.Completeness(ctx, orgID, service, def.Window)
		if err != nil {
			return nil, err
		}
		report.Completeness = completeness
		if completeness < m.gateThreshold {
			report.State = slotypes.StateIndeterminate
			return report, nil
		}
	}

	sli, err := m.evaluateSLI(ctx, orgID, def, startMs, endMs)
	if err != nil {
		return nil, err
	}
	report.SLI = sli
	report.State = resolveState(sli, def.Target, report.Completeness, m.gateThreshold, def.RequiresCompleteness)

	// Error budget and full-window burn derived from the ratio SLI. Over the whole
	// window, consumed budget fraction == burn rate, so remaining == 1 - burn.
	errorRate := 1 - sli
	report.BurnRate = BurnRate(errorRate, def.Target)
	remaining := 1 - report.BurnRate
	if remaining > 1 {
		remaining = 1
	}
	report.ErrorBudgetRemaining = remaining

	return report, nil
}

// evaluateSLI dispatches to the evaluator for the definition's type.
func (m *module) evaluateSLI(ctx context.Context, orgID valuer.UUID, def slotypes.SLODefinition, startMs, endMs uint64) (float64, error) {
	switch def.Type {
	case slotypes.SLITypeRatio:
		return evaluateRatio(ctx, m.scalar, orgID, def, startMs, endMs)
	default:
		return 0, errors.Newf(errors.TypeUnsupported, errors.CodeUnsupported, "slo %q: SLI type %q is not implemented yet", def.Name, def.Type)
	}
}

// indeterminateReport is returned when an SLO cannot be evaluated, keeping the
// rest of the list intact.
func indeterminateReport(service string, def slotypes.SLODefinition) *slotypes.Report {
	return &slotypes.Report{
		Name:    def.Name,
		Service: service,
		Type:    def.Type,
		Target:  def.Target,
		Window:  def.Window,
		State:   slotypes.StateIndeterminate,
	}
}
