// Package act implements the Act stage's adapters (PRD sections 15, 18):
// the Actuator interface is declared in internal/notify/slack (the
// consumer), and this package provides the advisory-only MVP
// implementation. Executing adapters (KEDA autoscaling, a config patch or
// pull request) are stretch goals and would live here too, behind the
// same interface.
package act

import (
	"context"
	"log/slog"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify/slack"
)

// AdvisoryActuator is the MVP Actuator (PRD section 15): it executes
// nothing at all. It logs the approved proposal for audit (PRD section 20)
// and reports that a human must apply it by hand.
type AdvisoryActuator struct {
	Logger *slog.Logger
}

var _ slack.Actuator = (*AdvisoryActuator)(nil)

// Act implements slack.Actuator. It never returns an error: recording an
// advisory proposal cannot fail in a way that should block the decision
// already made in Slack.
func (a *AdvisoryActuator) Act(_ context.Context, req slack.ActionRequest) (slack.ActionResult, error) {
	if req.Mutation != nil {
		if err := req.Mutation.Validate(); err != nil {
			return slack.ActionResult{Outcome: slack.OutcomeFailed, Detail: err.Error()}, nil
		}
		if !req.Execute {
			return slack.ActionResult{Outcome: slack.OutcomeRecorded, Detail: "mutation preview recorded; execution is disabled by configuration"}, nil
		}
		return slack.ActionResult{Outcome: slack.OutcomeFailed, Detail: "mutation execution backend is not configured"}, nil
	}
	a.logger().Info("advisory action recorded",
		"correlation_id", req.CorrelationID,
		"service", req.Service,
		"environment", req.Environment,
		"reversible", req.Reversible,
	)
	return slack.ActionResult{
		Outcome: slack.OutcomeRecorded,
		Detail:  "advisory only - no automated action was taken; apply the fix by hand, then verify",
	}, nil
}

func (a *AdvisoryActuator) logger() *slog.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return slog.Default()
}
