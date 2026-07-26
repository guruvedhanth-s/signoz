// Package rca is the RCA (root cause analysis) agent for Track C. It
// reasons about *why* an SLO alert fired and proposes a fix; it never
// computes a score, SLI, SLO state, error budget, burn rate, or threshold -
// those are deterministic facts computed elsewhere (the SLO engine and the
// completeness gate) and only carried through here (PRD section 7, section
// 13). The agent is advisory and read-only: it never takes an autonomous
// or destructive action.
//
// M1 (this milestone) builds the skeleton and the seams - the evidence
// gate, the reasoner interface, and the renderer that assembles the final
// notify.Diagnosis - with a stubbed Reasoner. The real reasoning agent
// (ADK/DeepSeek, with SigNoz MCP tool access) lands in later milestones
// (M3/M4).
package rca

import "github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"

// Incident is the input to the RCA agent: which service/environment/window
// an alert fired for, and the deterministic grounding facts computed
// upstream by the SLO engine and completeness gate. The RCA agent never
// computes Grounding itself - it is supplied by the caller (the webhook
// handler that received the firing alert, reading from the SLO engine) and
// is copied through to the final diagnosis unchanged.
type Incident struct {
	// CorrelationID ties this incident to the triggering alert and to every
	// diagnosis, message, and action that follows it, for audit.
	CorrelationID string
	Service       string
	Environment   string
	// Window is the lookback window for evidence gathering, e.g. "1h" or
	// "30d" (same format as internal/slo.WindowDuration accepts).
	Window string
	// SLO names the SLO this incident is scoped to.
	SLO string
	// Alert is a free-form description of which alert fired, e.g.
	// "fast-burn" or "slow-burn".
	Alert string
	// Grounding carries the deterministic reliability facts (SLO state,
	// burn rate, error budget, telemetry trust) this incident is based on.
	// The RCA agent only reads and forwards these; it never sets or alters
	// them.
	Grounding notify.Grounding
	// DeployEvents are explicit version/deploy markers discovered from
	// telemetry. They are correlated deterministically before reasoning.
	DeployEvents             []notify.DeployEvent
	DeployCorrelationEnabled bool
}
