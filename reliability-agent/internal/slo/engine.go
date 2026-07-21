package slo

import (
	"context"
	"fmt"
	"time"
)

// CompletenessGate is the single integration seam to the Telemetry Health
// Auditor. It returns 0..1 for a service and window, where 1.0 means fully
// trustworthy. Below an SLO's gate, the SLO short-circuits to indeterminate.
type CompletenessGate interface {
	Completeness(ctx context.Context, service string, window Window) (float64, error)
}

// NoopGate reports full completeness and is used until the auditor is wired in.
type NoopGate struct{}

func (NoopGate) Completeness(context.Context, string, Window) (float64, error) { return 1.0, nil }

// Engine evaluates SLO definitions against a stock SigNoz.
type Engine struct {
	scalar        ScalarQuerier
	gate          CompletenessGate
	gateThreshold float64
	now           func() time.Time
}

// NewEngine constructs the SLO engine. Pass NoopGate{} until the auditor exists.
func NewEngine(scalar ScalarQuerier, gate CompletenessGate) *Engine {
	return &Engine{
		scalar:        scalar,
		gate:          gate,
		gateThreshold: DefaultGateThreshold,
		now:           time.Now,
	}
}

// Evaluate resolves every SLO in the config into a report. A single SLO that
// cannot be evaluated becomes indeterminate rather than failing the whole set.
func (e *Engine) Evaluate(ctx context.Context, cfg *Config) []*Report {
	now := e.now()
	reports := make([]*Report, 0, len(cfg.SLOs))
	for _, def := range cfg.SLOs {
		report, err := e.evaluate(ctx, cfg.Service, def, now)
		if err != nil {
			report = indeterminateReport(cfg.Service, def)
		}
		reports = append(reports, report)
	}
	return reports
}

func (e *Engine) evaluate(ctx context.Context, service string, def SLODefinition, now time.Time) (*Report, error) {
	dur, err := def.Window.Duration()
	if err != nil {
		return nil, err
	}
	endMs := uint64(now.UnixMilli())
	startMs := uint64(now.Add(-dur).UnixMilli())

	report := &Report{
		Name:         def.Name,
		Service:      service,
		Type:         def.Type,
		Target:       def.Target,
		Window:       def.Window,
		Completeness: 1.0,
	}

	if def.RequiresCompleteness {
		completeness, err := e.gate.Completeness(ctx, service, def.Window)
		if err != nil {
			return nil, err
		}
		report.Completeness = completeness
		if completeness < e.gateThreshold {
			report.State = StateIndeterminate
			return report, nil
		}
	}

	sli, err := e.evaluateSLI(ctx, def, startMs, endMs)
	if err != nil {
		return nil, err
	}
	report.SLI = sli
	report.State = resolveState(sli, def.Target, report.Completeness, e.gateThreshold, def.RequiresCompleteness)

	errorRate := 1 - sli
	report.BurnRate = BurnRate(errorRate, def.Target)
	remaining := 1 - report.BurnRate
	if remaining > 1 {
		remaining = 1
	}
	report.ErrorBudgetRemaining = remaining
	return report, nil
}

func (e *Engine) evaluateSLI(ctx context.Context, def SLODefinition, startMs, endMs uint64) (float64, error) {
	switch def.Type {
	case SLITypeRatio:
		return evaluateRatio(ctx, e.scalar, def, startMs, endMs)
	default:
		return 0, fmt.Errorf("slo %q: SLI type %q is not implemented yet", def.Name, def.Type)
	}
}

func indeterminateReport(service string, def SLODefinition) *Report {
	return &Report{
		Name:    def.Name,
		Service: service,
		Type:    def.Type,
		Target:  def.Target,
		Window:  def.Window,
		State:   StateIndeterminate,
	}
}
