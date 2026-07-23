// Package otlp emits the reliability agent's results back into SigNoz as OTLP
// metrics, so an SLO renders on a normal SigNoz dashboard and can be alerted on.
package otlp

import (
	"context"
	"time"

	"github.com/guruvedhanth-s/signoz/reliability-agent/internal/slo"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// slo.state encoding.
const (
	stateUnhealthy     = 0
	stateHealthy       = 1
	stateIndeterminate = 2
)

// Emitter pushes slo.* metrics to the SigNoz collector over OTLP HTTP.
type Emitter struct {
	provider *sdkmetric.MeterProvider

	compliance metric.Float64Gauge
	state      metric.Float64Gauge
	budget     metric.Float64Gauge
	burn       metric.Float64Gauge
}

// NewEmitter builds an OTLP emitter targeting the collector's OTLP HTTP endpoint
// (for example localhost:4318).
func NewEmitter(ctx context.Context, endpoint string) (*Emitter, error) {
	exporter, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpoint(endpoint),
		otlpmetrichttp.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}
	reader := sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(1*time.Second))
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	meter := provider.Meter("reliability-agent")

	compliance, err := meter.Float64Gauge("slo_compliance",
		metric.WithDescription("Measured SLI as a fraction 0..1."), metric.WithUnit("1"))
	if err != nil {
		return nil, err
	}
	state, err := meter.Float64Gauge("slo_state",
		metric.WithDescription("SLO trust state: 0 unhealthy, 1 healthy, 2 indeterminate."))
	if err != nil {
		return nil, err
	}
	budget, err := meter.Float64Gauge("slo_error_budget_remaining",
		metric.WithDescription("Remaining error budget as a fraction of the total budget."), metric.WithUnit("1"))
	if err != nil {
		return nil, err
	}
	burn, err := meter.Float64Gauge("slo_burn_rate",
		metric.WithDescription("Error-budget burn rate; 1.0 exhausts the budget by the end of the window."), metric.WithUnit("1"))
	if err != nil {
		return nil, err
	}

	return &Emitter{provider: provider, compliance: compliance, state: state, budget: budget, burn: burn}, nil
}

// Emit records one SLO report. slo.state is always emitted; compliance, budget,
// and burn are only meaningful when the SLO was computed (not indeterminate).
func (e *Emitter) Emit(ctx context.Context, r *slo.Report) {
	attrs := metric.WithAttributes(
		attribute.String("service", r.Service),
		attribute.String("slo", r.Name),
		attribute.String("window", string(r.Window)),
	)
	e.state.Record(ctx, stateValue(r.State), attrs)
	if r.State == slo.StateIndeterminate {
		return
	}
	e.compliance.Record(ctx, r.SLI, attrs)
	e.budget.Record(ctx, r.ErrorBudgetRemaining, attrs)
	e.burn.Record(ctx, r.BurnRate, attrs)
}

// Shutdown flushes pending metrics and stops the provider.
func (e *Emitter) Shutdown(ctx context.Context) error {
	return e.provider.Shutdown(ctx)
}

func stateValue(s slo.State) float64 {
	switch s {
	case slo.StateHealthy:
		return stateHealthy
	case slo.StateIndeterminate:
		return stateIndeterminate
	default:
		return stateUnhealthy
	}
}
