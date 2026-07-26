package rca

import (
	"context"
	"fmt"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify/slack"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/slo"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/source"
)

// SLOVerifier implements slack.Verifier by re-running the deterministic SLO
// engine (PRD section 16) - the verify stage never touches the LLM or the
// RCA agent, so this deliberately does not embed or call *Agent.
type SLOVerifier struct {
	Metrics       source.MetricQuerier
	SLOConfigPath string
	// Now returns the current time; overridable in tests. Defaults to
	// time.Now.
	Now func() time.Time
}

var _ slack.Verifier = (*SLOVerifier)(nil)

// Verify implements slack.Verifier. Recovery is judged on whatever window
// the named SLO's own config uses - the demo config uses a short window
// precisely so this responds within minutes rather than staying depressed
// on a long compliance window (PRD sections 11.3, 16).
func (v *SLOVerifier) Verify(ctx context.Context, req slack.VerifyRequest) (slack.VerifyResult, error) {
	cfg, err := slo.LoadConfig(v.SLOConfigPath)
	if err != nil {
		return slack.VerifyResult{}, fmt.Errorf("rca: load SLO config for verify: %w", err)
	}
	if cfg.Service != req.Service || cfg.Environment != req.Environment {
		return slack.VerifyResult{}, fmt.Errorf(
			"rca: SLO config service/environment (%s/%s) does not match the incident (%s/%s)",
			cfg.Service, cfg.Environment, req.Service, req.Environment)
	}

	gate := slo.NewMetricPresenceGate(v.Metrics, nil)
	engine := slo.NewEngine(v.Metrics, gate)
	reports, err := engine.Evaluate(ctx, cfg, v.now())
	if err != nil {
		return slack.VerifyResult{}, fmt.Errorf("rca: evaluate SLO for verify: %w", err)
	}
	report, ok := SelectReport(reports, req.SLO)
	if !ok {
		return slack.VerifyResult{}, fmt.Errorf("rca: no SLO report named %q", req.SLO)
	}

	return slack.VerifyResult{
		SLOState:             string(report.State),
		BurnRate:             report.BurnRate,
		ErrorBudgetRemaining: report.ErrorBudgetRemaining,
		TelemetryTrusted:     report.Gate.Trusted,
		Recovered:            report.Gate.Trusted && report.State == slo.StateHealthy,
	}, nil
}

func (v *SLOVerifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}
