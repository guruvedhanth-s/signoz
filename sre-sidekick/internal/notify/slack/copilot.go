package slack

import (
	"context"
	"errors"
)

// CopilotScope is the service and environment a session-less question is
// answered about. Either field may be empty, in which case the copilot
// resolves what it can and asks rather than guessing.
type CopilotScope struct {
	Service     string
	Environment string
}

// Copilot answers a question from live state, with no incident and no
// frozen diagnosis behind it.
//
// The interface is declared here, on the consumer side, and implemented in
// internal/answer - the same arrangement RCA, Verifier and Actuator
// already use. That is what keeps the deterministic tool surface from
// becoming Slack-shaped: this package knows only that a question goes in
// and text comes out.
//
// The contract has one deliberate omission: there is no way for the
// coordinator to influence what the answer claims. It cannot pass a
// template, a tone, or a hint. Every number in the returned text was
// computed by the tool surface and checked by the composer before this
// package ever sees it.
type Copilot interface {
	// Answer resolves the question to one of a closed set of intents,
	// computes the result deterministically, and returns text ready to
	// post. A question that maps to no intent comes back as a plain
	// explanation of what can be asked, not as an error.
	Answer(ctx context.Context, question string, scope CopilotScope) (string, error)
}

// ErrCopilotUnavailable reports that no copilot is wired up, so
// session-less questions cannot be answered at all.
var ErrCopilotUnavailable = errors.New("slack: no copilot is configured")

// UnavailableCopilot is the default until a real one is attached. It
// refuses rather than improvising, which is the same posture
// UnavailableRCA takes: a sidekick with no engine behind it must say so,
// because the one thing it must never do is answer anyway.
type UnavailableCopilot struct{}

// Answer implements Copilot.
func (UnavailableCopilot) Answer(context.Context, string, CopilotScope) (string, error) {
	return "", ErrCopilotUnavailable
}
