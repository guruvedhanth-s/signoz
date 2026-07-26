package rca

import "strings"

// Intent is the small, shared surface for deterministic follow-up routing.
// Keeping this outside SlackAdapter prevents provider-specific handlers from
// accumulating competing substring resolvers.
type Intent string

const IntentWhatChanged Intent = "what_changed"

func ResolveIntent(question string) Intent {
	question = strings.ToLower(strings.TrimSpace(question))
	question = strings.TrimLeft(question, "!?.,:; ")
	if strings.HasPrefix(question, "what changed") || strings.HasPrefix(question, "what was deployed") {
		return IntentWhatChanged
	}
	return ""
}
