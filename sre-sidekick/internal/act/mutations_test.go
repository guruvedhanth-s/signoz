package act

import (
	"testing"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify/slack"
)

func TestParseMutationCommandAcceptsOnlyTypedAllowlistedMutation(t *testing.T) {
	got, err := ParseMutationCommand("update_burn_threshold slo=checkout tier=fast multiplier=6")
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != slack.MutationUpdateBurn || got.SLO != "checkout" || got.NewMultiplier != 6 {
		t.Fatalf("mutation = %+v", got)
	}
}

func TestParseMutationCommandRejectsArbitraryOrUnmanagedMutation(t *testing.T) {
	for _, command := range []string{
		"delete_dashboard name=checkout",
		"update_burn_threshold name=checkout multiplier=6",
	} {
		if _, err := ParseMutationCommand(command); err == nil {
			t.Errorf("ParseMutationCommand(%q) accepted an unsafe mutation", command)
		}
	}
}
