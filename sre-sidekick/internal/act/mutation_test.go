package act

import (
	"context"
	"testing"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/mutation"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify/slack"
)

type mutationBackend struct {
	preview    mutation.Diff
	applyErr   error
	rolledBack bool
}

func (b *mutationBackend) Preview(context.Context, mutation.Request) (mutation.Diff, error) {
	return b.preview, nil
}
func (b *mutationBackend) Apply(context.Context, mutation.Request, mutation.Diff) (mutation.Diff, error) {
	return b.preview, b.applyErr
}
func (b *mutationBackend) Rollback(context.Context, mutation.Request, mutation.Diff) error {
	b.rolledBack = true
	return nil
}

func TestMutationActuatorIsDisabledByDefault(t *testing.T) {
	a := &MutationActuator{}
	result, err := a.Act(context.Background(), slack.ActionRequest{Mutation: &mutation.Request{Kind: mutation.CreateDashboard, Name: "checkout"}})
	if err != nil || result.Outcome != slack.OutcomeRecorded {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestMutationActuatorRejectsChangedPreviewAndRollsBackApplyFailure(t *testing.T) {
	req := mutation.Request{Kind: mutation.CreateDashboard, Name: "checkout"}
	backend := &mutationBackend{preview: mutation.Diff{Target: "dashboard:1", Before: "old", After: "new"}, applyErr: context.Canceled}
	a := &MutationActuator{Backend: backend}
	result, _ := a.Act(context.Background(), slack.ActionRequest{Mutation: &req, Preview: mutation.Diff{Target: "dashboard:1", Before: "old", After: "different"}, Execute: true})
	if result.Outcome != slack.OutcomeFailed || backend.rolledBack {
		t.Fatalf("changed preview result=%+v rollback=%v", result, backend.rolledBack)
	}
	result, _ = a.Act(context.Background(), slack.ActionRequest{Mutation: &req, Preview: backend.preview, Execute: true})
	if result.Outcome != slack.OutcomeFailed || !backend.rolledBack {
		t.Fatalf("apply failure result=%+v rollback=%v", result, backend.rolledBack)
	}
}
