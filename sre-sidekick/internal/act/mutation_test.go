package act

import (
	"context"
	"testing"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/mutation"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify/slack"
)

func TestMutationActuatorIsDisabledByDefault(t *testing.T) {
	a := &MutationActuator{}
	result, err := a.Act(context.Background(), slack.ActionRequest{Mutation: &mutation.Request{Kind: mutation.CreateDashboard, Name: "checkout"}})
	if err != nil || result.Outcome != slack.OutcomeRecorded {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
