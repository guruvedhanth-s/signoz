package act

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify/slack"
)

func TestAdvisoryActuator_Act_NeverExecutesAnythingAndAlwaysRecords(t *testing.T) {
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	actuator := &AdvisoryActuator{Logger: logger}

	result, err := actuator.Act(context.Background(), slack.ActionRequest{
		CorrelationID: "corr-1",
		Service:       "support-agent",
		Environment:   "prod",
		ProposedFix:   "raise the tool timeout",
		Reversible:    true,
	})
	if err != nil {
		t.Fatalf("Act() error = %v", err)
	}
	if result.Outcome != slack.OutcomeRecorded {
		t.Errorf("Outcome = %q, want %q", result.Outcome, slack.OutcomeRecorded)
	}
	if !strings.Contains(result.Detail, "no automated action was taken") {
		t.Errorf("Detail = %q, want it to state nothing was executed", result.Detail)
	}
	if !strings.Contains(logs.String(), "corr-1") {
		t.Error("the proposal was not logged for audit (PRD section 20)")
	}
}

func TestAdvisoryActuator_Act_DefaultsToSlogDefault(t *testing.T) {
	actuator := &AdvisoryActuator{}
	if _, err := actuator.Act(context.Background(), slack.ActionRequest{Service: "x", Environment: "y"}); err != nil {
		t.Fatalf("Act() error = %v, want a nil Logger to fall back cleanly", err)
	}
}
