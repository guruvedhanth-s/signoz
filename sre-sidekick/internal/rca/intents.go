package rca

import "strings"

// Intent is the small, shared surface for deterministic follow-up routing.
// Keeping this outside SlackAdapter prevents provider-specific handlers from
// accumulating competing substring resolvers.
type Intent string

const IntentWhatChanged Intent = "what_changed"

func ResolveIntent(question string) Intent {
	question = strings.ToLower(strings.TrimSpace(question))
	if strings.Contains(question, "what changed") || strings.Contains(question, "what was deployed") {
		return IntentWhatChanged
	}
	return ""
}
