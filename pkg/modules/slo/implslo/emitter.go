package implslo

import (
	"context"

	"github.com/SigNoz/signoz/pkg/errors"
	"github.com/SigNoz/signoz/pkg/types/slotypes"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// State encoding for the slo.state gauge.
const (
	stateValueUnhealthy     = 0
	stateValueHealthy       = 1
	stateValueIndeterminate = 2
)

// emitter records the results of SLO evaluation back into SigNoz as OTLP
// metrics, so an SLO renders on a normal dashboard and can be alerted on.
type emitter struct {
	compliance      metric.Float64Gauge
	state           metric.Float64Gauge
	budgetRemaining metric.Float64Gauge
	burnRate        metric.Float64Gauge
}

// newEmitter creates the slo.* instruments on the given meter.
func newEmitter(meter metric.Meter) (*emitter, error) {
	var errs error

	compliance, err := meter.Float64Gauge("slo.compliance",
		metric.WithDescription("Measured SLI as a fraction in the range 0..1."), metric.WithUnit("1"))
	errs = errors.Join(errs, err)

	state, err := meter.Float64Gauge("slo.state",
		metric.WithDescription("SLO trust state: 0 unhealthy, 1 healthy, 2 indeterminate."))
	errs = errors.Join(errs, err)

	budgetRemaining, err := meter.Float64Gauge("slo.error_budget_remaining",
		metric.WithDescription("Remaining error budget as a fraction of the total budget."), metric.WithUnit("1"))
	errs = errors.Join(errs, err)

	burnRate, err := meter.Float64Gauge("slo.burn_rate",
		metric.WithDescription("Error-budget burn rate; 1.0 exhausts the budget by the end of the window."), metric.WithUnit("1"))
	errs = errors.Join(errs, err)

	if errs != nil {
		return nil, errs
	}
	return &emitter{compliance: compliance, state: state, budgetRemaining: budgetRemaining, burnRate: burnRate}, nil
}

// Emit records one SLO report. It is safe to call on a nil emitter, which makes
// emission optional (for example in tests).
func (e *emitter) Emit(ctx context.Context, r *slotypes.Report) {
	if e == nil || r == nil {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String("service", r.Service),
		attribute.String("slo", r.Name),
		attribute.String("window", string(r.Window)),
	)
	e.state.Record(ctx, stateToValue(r.State), attrs)

	// Compliance, budget, and burn are only meaningful when the SLO was actually
	// computed (not indeterminate).
	if r.State == slotypes.StateIndeterminate {
		return
	}
	e.compliance.Record(ctx, r.SLI, attrs)
	e.budgetRemaining.Record(ctx, r.ErrorBudgetRemaining, attrs)
	e.burnRate.Record(ctx, r.BurnRate, attrs)
}

func stateToValue(s slotypes.State) float64 {
	switch s {
	case slotypes.StateHealthy:
		return stateValueHealthy
	case slotypes.StateIndeterminate:
		return stateValueIndeterminate
	default:
		return stateValueUnhealthy
	}
}
