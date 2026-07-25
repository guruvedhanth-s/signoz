package slack

import (
	"context"
	"errors"
)

// Verifier is the deterministic engine behind the verify stage (PRD
// section 16): after a human approves and applies a fix by hand, it
// re-evaluates the SLO and reports whether it has recovered.
//
// Declared here, on the consumer side, for the same reason RCA is: the
// Slack adapter depends on this small interface, not on the SLO engine's
// own types, and nothing here reasons - Verify must never touch the LLM
// (PRD section 16 says the verify stage is deterministic).
type Verifier interface {
	Verify(ctx context.Context, req VerifyRequest) (VerifyResult, error)
}

// VerifyRequest asks for a fresh SLO evaluation.
type VerifyRequest struct {
	Service     string
	Environment string
	// SLO names the SLO to re-evaluate - the same one the original
	// diagnosis was grounded on (Diagnosis.Grounding.SLO).
	SLO string
}

// VerifyResult is the re-evaluated SLO state. Recovery is judged on the
// short-window burn rate the SLO config itself is set to (PRD sections 11.3,
// 16), not on a long compliance window that stays depressed after
// budget-consuming failures - the verify stage does not average two
// windows itself, it reports whatever window the SLO's own config uses.
type VerifyResult struct {
	SLOState             string
	BurnRate             float64
	ErrorBudgetRemaining float64
	TelemetryTrusted     bool
	// Recovered is true when the re-evaluated SLO is healthy and trusted.
	Recovered bool
}

// ErrVerifierUnavailable is returned when no verify engine is attached.
var ErrVerifierUnavailable = errors.New("verify: SLO re-evaluation is not connected")

// UnavailableVerifier is the stub used until a verify engine is wired in.
// It refuses rather than reporting a fabricated recovery state.
type UnavailableVerifier struct{}

var _ Verifier = UnavailableVerifier{}

func (UnavailableVerifier) Verify(context.Context, VerifyRequest) (VerifyResult, error) {
	return VerifyResult{}, ErrVerifierUnavailable
}
