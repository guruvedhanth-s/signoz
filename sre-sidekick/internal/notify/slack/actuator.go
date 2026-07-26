package slack

import (
	"context"
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
