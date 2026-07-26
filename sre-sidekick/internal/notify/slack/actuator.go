package slack

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Actuator executes (or, in the advisory MVP, records) an approved
// remediation proposal (PRD sections 15, 18). Declared here, consumer
// side, like RCA and Verifier: the Slack layer depends on this small
// interface, not on whichever adapter (advisory today; KEDA, a config
// patch, or a PR later) implements it.
//
// The LLM never calls this directly and never receives write credentials
// (PRD section 20) - only a human-approved decision reaches an Actuator.
type Actuator interface {
	Act(ctx context.Context, req ActionRequest) (ActionResult, error)
}

// ActionRequest describes one human-approved proposal.
type ActionRequest struct {
	CorrelationID string
	Service       string
	Environment   string
	ProposedFix   string
	// Reversible mirrors Diagnosis.Reversible: an irreversible action is
	// labeled and, in an executing adapter, would demand stronger
	// confirmation before it runs (PRD section 15).
	Reversible bool
	Mutation   *MutationRequest
}

type MutationKind string

const (
	MutationCreateDashboard MutationKind = "create_dashboard"
	MutationUpdateBurn      MutationKind = "update_burn_threshold"
	MutationSilenceAlert    MutationKind = "silence_alert"
	MutationEnableAlert     MutationKind = "enable_alert"
	MutationDisableAlert    MutationKind = "disable_alert"
)

// MutationRequest is the closed, typed allowlist boundary. There is no raw
// HTTP method, URL, or arbitrary SigNoz payload here for an LLM to construct.
type MutationRequest struct {
	Kind          MutationKind
	Name          string
	SLO           string
	Tier          string
	NewMultiplier float64
	Duration      time.Duration
	ManagedBy     string
	Before        string
	After         string
}

func (m MutationRequest) Validate() error {
	if strings.TrimSpace(m.ManagedBy) != "signoz-sre-sidekick" {
		return fmt.Errorf("mutation target is not managed by the sidekick")
	}
	switch m.Kind {
	case MutationCreateDashboard:
		if strings.TrimSpace(m.Name) == "" {
			return fmt.Errorf("mutation %s requires a target name", m.Kind)
		}
	case MutationUpdateBurn:
		if strings.TrimSpace(m.SLO) == "" || strings.TrimSpace(m.Tier) == "" {
			return fmt.Errorf("mutation %s requires slo and tier", m.Kind)
		}
		if m.NewMultiplier <= 0 || m.NewMultiplier > 1000 {
			return fmt.Errorf("burn threshold must be between 0 and 1000")
		}
	case MutationSilenceAlert:
		if strings.TrimSpace(m.Name) == "" {
			return fmt.Errorf("mutation %s requires a target name", m.Kind)
		}
		if m.Duration <= 0 {
			return fmt.Errorf("silence duration must be positive")
		}
	case MutationEnableAlert, MutationDisableAlert:
		if strings.TrimSpace(m.Name) == "" {
			return fmt.Errorf("mutation %s requires a target name", m.Kind)
		}
	default:
		return fmt.Errorf("mutation %q is not allowlisted", m.Kind)
	}
	return nil
}

// ActionResult is what the Actuator did (or recorded) with the proposal.
type ActionResult struct {
	// Outcome is one of the Outcome* constants below.
	Outcome string
	// Detail is a short human-readable note, safe to log or show in Slack.
	Detail string
}

// Outcomes recorded on MetricActions and returned in ActionResult.
const (
	// OutcomeRecorded is the only outcome the advisory MVP ever returns:
	// the proposal was logged for audit, and a human performs the fix by
	// hand. An executing adapter would add OutcomeExecuted/OutcomeFailed.
	OutcomeRecorded = "recorded"
)

// NoopActuator is the default Actuator: it records nothing beyond what the
// caller already logs, and always reports OutcomeRecorded. Coordinators
// that want the proposal logged for audit (PRD section 20) should attach
// act.AdvisoryActuator instead.
type NoopActuator struct{}

var _ Actuator = NoopActuator{}

func (NoopActuator) Act(context.Context, ActionRequest) (ActionResult, error) {
	return ActionResult{Outcome: OutcomeRecorded, Detail: "advisory only - no automated action was taken"}, nil
}
