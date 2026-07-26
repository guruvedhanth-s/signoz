package slo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/source"
)

type CompletenessGate interface {
	Check(context.Context, GateRequest) (GateResult, error)
}

type Engine struct {
	Metrics source.MetricQuerier
	Gate    CompletenessGate
	Now     func() time.Time
}

func NewEngine(metrics source.MetricQuerier, gate CompletenessGate) *Engine {
	return &Engine{Metrics: metrics, Gate: gate, Now: time.Now}
}

func (e *Engine) Evaluate(ctx context.Context, cfg Config, now time.Time) ([]Report, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if e.Metrics == nil {
		return nil, fmt.Errorf("SLO metric querier is required")
	}
	if now.IsZero() {
		now = e.Now()
	}
	reports := make([]Report, 0, len(cfg.SLOs))
	for _, definition := range cfg.SLOs {
		reports = append(reports, e.evaluate(ctx, cfg, definition, now))
	}
	return reports, nil
}

func (e *Engine) evaluate(ctx context.Context, cfg Config, definition Definition, now time.Time) Report {
	duration, err := WindowDuration(definition.Window)
	report := Report{
		SchemaVersion: "1.0",
		Name:          definition.Name,
		Service:       cfg.Service,
		Environment:   cfg.Environment,
		Type:          definition.Type,
		Window:        definition.Window,
		Target:        definition.NormalizedTarget(),
		Completeness:  1,
		Gate:          GateResult{Coverage: 1, QueryComplete: true, Trusted: true},
	}
	if err != nil {
		return indeterminate(report, err)
	}
	report.EvaluatedStart = now.Add(-duration)
	report.EvaluatedEnd = now

	if definition.Type == SLITypeCompleteness {
		// A completeness SLI's value *is* the dependency-coverage
		// fraction the gate computes - not a good/total counter ratio
		// (see config.go's Validate: this type takes no good_metric/
		// total_metric). The SLO Engine and the Track A audit engine
		// answer different questions (PRD section 7): this still never
		// judges whether telemetry is "reliable enough for other SLOs",
		// only whether the fraction it measures meets its own target.
		return e.evaluateCompleteness(ctx, cfg, definition, now, duration, report)
	}

	if definition.RequiresCompleteness {
		gateResult, gateErr := e.checkCompleteness(ctx, cfg, definition, now, duration)
		if gateErr != nil {
			return indeterminate(report, gateErr)
		}
		report.Gate = gateResult
		report.Completeness = gateResult.Coverage
		report.Warning = gateResult.Warning
		if !gateResult.Trusted {
			return indeterminate(report, fmt.Errorf("telemetry completeness is below the SLO gate"))
		}
	}

	start := uint64(report.EvaluatedStart.UnixMilli())
	end := uint64(report.EvaluatedEnd.UnixMilli())
	sli, warning, queryErr := evaluateSLI(ctx, e.Metrics, cfg, definition, start, end)
	if queryErr != nil {
		return indeterminate(report, queryErr)
	}
	report.SLI = sli
	if report.Warning == "" {
		report.Warning = warning
	} else if warning != "" {
		report.Warning += "; " + warning
	}
	if sli >= report.Target {
		report.State = StateHealthy
	} else {
		report.State = StateUnhealthy
	}
	report.BurnRate = BurnRate(1-sli, report.Target)
	report.ErrorBudgetRemaining = RemainingBudget(1-sli, report.Target)
	return report
}

// checkCompleteness runs the completeness gate for definition, scoped to
// its own service/environment label overrides and dependencies (falling
// back to cfg.Completeness.ExpectedMetrics), and marks the result Trusted
// against cfg.GateThreshold. Shared by RequiresCompleteness (a trust gate
// in front of a ratio/latency/grounded_answers SLI) and evaluateCompleteness
// (where coverage is the SLI itself).
func (e *Engine) checkCompleteness(ctx context.Context, cfg Config, definition Definition, now time.Time, window time.Duration) (GateResult, error) {
	if e.Gate == nil {
		return GateResult{}, fmt.Errorf("completeness gate is not configured")
	}
	dependencies := definition.Dependencies
	if len(dependencies) == 0 && cfg.Completeness != nil {
		dependencies = cfg.Completeness.ExpectedMetrics
	}
	serviceLabel, environmentLabel := cfg.MetricLabels()
	if trimmed := strings.TrimSpace(definition.ServiceLabel); trimmed != "" {
		serviceLabel = trimmed
	}
	if trimmed := strings.TrimSpace(definition.EnvironmentLabel); trimmed != "" {
		environmentLabel = trimmed
	}
	gateResult, err := e.Gate.Check(ctx, GateRequest{
		Service:          cfg.Service,
		Environment:      cfg.Environment,
		Window:           window,
		Dependencies:     dependencies,
		ServiceLabel:     serviceLabel,
		EnvironmentLabel: environmentLabel,
		Now:              now,
	})
	if err != nil {
		return GateResult{}, err
	}
	gateResult.Trusted = gateResult.QueryComplete && gateResult.Coverage >= cfg.GateThreshold(definition)
	return gateResult, nil
}

// evaluateCompleteness handles SLITypeCompleteness: the gate's coverage
// fraction is the SLI, compared directly against the definition's target
// (e.g. target: 0.99 means "99% of the time, these dependencies have
// data"). A query failure or an incomplete gate query is indeterminate,
// same as any other SLI's query failure - but an merely low (not
// incomplete) coverage is a real, computed unhealthy state, not
// indeterminate: the gate answered the question, the answer was just
// below target.
func (e *Engine) evaluateCompleteness(
	ctx context.Context, cfg Config, definition Definition, now time.Time, window time.Duration, report Report,
) Report {
	gateResult, err := e.checkCompleteness(ctx, cfg, definition, now, window)
	if err != nil {
		return indeterminate(report, err)
	}
	report.Gate = gateResult
	report.Completeness = gateResult.Coverage
	report.Warning = gateResult.Warning
	if !gateResult.QueryComplete {
		return indeterminate(report, fmt.Errorf("completeness dependency query did not complete"))
	}
	report.SLI = gateResult.Coverage
	if report.SLI >= report.Target {
		report.State = StateHealthy
	} else {
		report.State = StateUnhealthy
	}
	report.BurnRate = BurnRate(1-report.SLI, report.Target)
	report.ErrorBudgetRemaining = RemainingBudget(1-report.SLI, report.Target)
	return report
}

func indeterminate(report Report, err error) Report {
	report.State = StateIndeterminate
	report.Error = err.Error()
	report.Gate.Trusted = false
	return report
}
