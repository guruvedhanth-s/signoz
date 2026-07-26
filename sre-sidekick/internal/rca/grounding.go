package rca

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/slo"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/source"
)

// GroundingFromReport maps a deterministic slo.Report - computed entirely
// by the SLO engine and completeness gate, never by the RCA agent or its
// reasoner - into the notify.Grounding an Incident carries and Render
// copies verbatim into the final Diagnosis. This is a field-by-field copy,
// not a re-derivation: the RCA agent and its reasoner must never recompute
// or alter any of these facts (PRD section 7, section 13).
//
// report.Gate.Trusted is the authoritative telemetry-trust signal. It is
// false exactly when the SLO engine could not evaluate the SLO with
// confidence, including the indeterminate case: Engine.evaluate sets
// Gate.Trusted to false whenever it returns a slo.StateIndeterminate
// report (see internal/slo/engine.go), so GroundingFromReport does not
// need to special-case State itself to get TelemetryTrusted right.
func GroundingFromReport(environment string, report slo.Report) notify.Grounding {
	return notify.Grounding{
		Environment:          environment,
		SLO:                  report.Name,
		SLOState:             string(report.State),
		BurnRate:             report.BurnRate,
		ErrorBudgetRemaining: report.ErrorBudgetRemaining,
		TelemetryTrusted:     report.Gate.Trusted,
		EvaluatedStart:       report.EvaluatedStart,
		EvaluatedEnd:         report.EvaluatedEnd,
	}
}

// SelectReport picks the report named name from reports. If name is empty,
// or no report matches it, the first report is returned instead - most SLO
// config files scope one incident to a single primary SLO, so falling back
// to "the only one" (or "the first one") is a reasonable default rather
// than an error. Returns false only when reports is empty.
func SelectReport(reports []slo.Report, name string) (slo.Report, bool) {
	if len(reports) == 0 {
		return slo.Report{}, false
	}
	if name != "" {
		for _, report := range reports {
			if report.Name == name {
				return report, true
			}
		}
	}
	return reports[0], true
}

// ErrConfigScopeMismatch reports that the SLO config loaded from disk
// describes a different service/environment than the caller asked about.
// EvaluateGrounding treats this as "diagnose without grounding" (a
// warning, not a failure) to preserve the conservative default every RCA
// entry point already relies on; callers that need to tell the difference
// - notably the conversational tool surface, which must answer
// "indeterminate, and here is why" rather than silently returning zeros -
// call EvaluateReports and match on this error instead.
var ErrConfigScopeMismatch = errors.New("SLO config service/environment does not match the requested service/environment")

// EvaluateReports evaluates the SLO config at configPath and returns every
// slo.Report it produced, unflattened: the full deterministic record
// including EvaluatedStart/EvaluatedEnd, per-SLO state, SLI, target, burn
// rate, error budget and the completeness gate verdict.
//
// This is the shared engine-invocation path. EvaluateGrounding narrows the
// result down to the single notify.Grounding an Incident carries, which is
// all the RCA flow needs; a caller that must report on every SLO, or that
// needs the provenance metadata the Grounding drops, uses this directly.
// Neither function recomputes anything: the SLO engine and the
// completeness gate are the only things that produce these numbers (PRD
// section 7, section 13).
//
// Returns ErrConfigScopeMismatch (wrapped) when the config is scoped to a
// different service/environment, so the caller decides whether that is a
// warning or an answer.
func EvaluateReports(ctx context.Context, metrics source.MetricQuerier, configPath, service, environment string) ([]slo.Report, error) {
	cfg, err := slo.LoadConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("load SLO config for grounding: %w", err)
	}
	if cfg.Service != service || cfg.Environment != environment {
		return nil, fmt.Errorf("%w: config is %s/%s, requested %s/%s",
			ErrConfigScopeMismatch, cfg.Service, cfg.Environment, service, environment)
	}
	reports, err := EvaluateConfig(ctx, metrics, cfg, time.Now())
	if err != nil {
		return nil, err
	}
	return reports, nil
}

// EvaluateConfig runs the SLO engine over an already-loaded config at a
// caller-supplied instant. Split out from EvaluateReports so callers that
// hold a config in memory (or that need a fixed `now` for a deterministic
// test) do not have to round-trip through the filesystem.
func EvaluateConfig(ctx context.Context, metrics source.MetricQuerier, cfg slo.Config, now time.Time) ([]slo.Report, error) {
	gate := slo.NewMetricPresenceGate(metrics, nil)
	engine := slo.NewEngine(metrics, gate)
	reports, err := engine.Evaluate(ctx, cfg, now)
	if err != nil {
		return nil, fmt.Errorf("evaluate SLO for grounding: %w", err)
	}
	return reports, nil
}

// EvaluateGrounding evaluates the SLO config at configPath and maps the
// report for the SLO scoped to service/environment into a notify.Grounding
// via GroundingFromReport, along with the SLO's name. This is the single
// shared path every RCA entry point (the `diagnose` CLI command, the
// alert-driven detect stage, and the Slack /diagnose command) uses to
// ground an incident, so grounding is computed exactly once, the same way,
// regardless of who triggered the diagnosis (PRD section 7).
//
// If the config's service/environment do not match the incident being
// diagnosed, grounding is skipped (a zero-value Grounding is returned,
// matching the already-conservative TelemetryTrusted=false default) with a
// warning logged, rather than silently attaching facts about the wrong
// service.
func EvaluateGrounding(ctx context.Context, metrics source.MetricQuerier, configPath, service, environment string) (notify.Grounding, string, error) {
	reports, err := EvaluateReports(ctx, metrics, configPath, service, environment)
	if err != nil {
		if errors.Is(err, ErrConfigScopeMismatch) {
			slog.Warn("rca: SLO config service/environment does not match the incident; diagnosing without grounding",
				"service", service, "environment", environment, "error", err)
			return notify.Grounding{}, "", nil
		}
		return notify.Grounding{}, "", err
	}
	report, ok := SelectReport(reports, "")
	if !ok {
		return notify.Grounding{}, "", nil
	}
	return GroundingFromReport(environment, report), report.Name, nil
}
