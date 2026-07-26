package act

import (
	"context"
	"fmt"
	"reflect"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/mutation"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify/slack"
)

// MutationActuator is the guarded execution adapter. A backend is required
// for ownership checks, preview generation, apply, and rollback. Without an
// explicitly enabled backend it records the request but cannot change SigNoz.
type MutationActuator struct {
	Backend mutation.Backend
}

var _ slack.Actuator = (*MutationActuator)(nil)

func (a *MutationActuator) Act(ctx context.Context, req slack.ActionRequest) (slack.ActionResult, error) {
	if req.Mutation == nil {
		return slack.ActionResult{Outcome: slack.OutcomeRecorded, Detail: "no typed mutation requested"}, nil
	}
	if err := req.Mutation.Validate(); err != nil {
		return slack.ActionResult{Outcome: slack.OutcomeFailed, Detail: err.Error()}, nil
	}
	if !req.Execute {
		return slack.ActionResult{Outcome: slack.OutcomeRecorded, Detail: "mutation preview recorded; execution is disabled by configuration"}, nil
	}
	if a.Backend == nil {
		return slack.ActionResult{Outcome: slack.OutcomeFailed, Detail: "mutation execution backend is not configured"}, nil
	}
	diff, err := a.Backend.Preview(ctx, *req.Mutation)
	if err != nil {
		return slack.ActionResult{Outcome: slack.OutcomeFailed, Detail: fmt.Sprintf("mutation preview failed: %v", err)}, nil
	}
	if diff.Target == "" || diff.Before == "" || diff.After == "" {
		return slack.ActionResult{Outcome: slack.OutcomeFailed, Detail: "mutation preview is incomplete"}, nil
	}
	if req.Preview.Target == "" || !reflect.DeepEqual(req.Preview, diff) {
		return slack.ActionResult{Outcome: slack.OutcomeFailed, Detail: "mutation state changed since preview; refresh the preview before approving"}, nil
	}
	if _, err := a.Backend.Apply(ctx, *req.Mutation, diff); err != nil {
		rollbackErr := a.Backend.Rollback(ctx, *req.Mutation, diff)
		if rollbackErr != nil {
			return slack.ActionResult{Outcome: slack.OutcomeFailed, Detail: fmt.Sprintf("mutation apply failed: %v; rollback failed: %v", err, rollbackErr)}, nil
		}
		return slack.ActionResult{Outcome: slack.OutcomeFailed, Detail: fmt.Sprintf("mutation apply failed: %v; rollback completed", err)}, nil
	}
	return slack.ActionResult{Outcome: slack.OutcomeExecuted, Detail: fmt.Sprintf("mutation applied to %s; rollback is available", diff.Target)}, nil
}
