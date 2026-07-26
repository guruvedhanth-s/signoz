package rca

import "testing"

func TestResolveIntentOnlyRoutesDirectWhatChangedQuestions(t *testing.T) {
	for _, question := range []string{"what changed?", "What was deployed in prod?", "  what changed since the alert"} {
		if got := ResolveIntent(question); got != IntentWhatChanged {
			t.Errorf("ResolveIntent(%q) = %q, want %q", question, got, IntentWhatChanged)
		}
	}
	for _, question := range []string{"I know what changed, but why did it break?", "why did it break?"} {
		if got := ResolveIntent(question); got == IntentWhatChanged {
			t.Errorf("ResolveIntent(%q) routed an embedded phrase to what_changed", question)
		}
	}
}
