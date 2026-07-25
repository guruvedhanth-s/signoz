package slack

import (
	"context"
	"errors"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/session"
)

// RCA is the reasoning engine behind the adapter: the thing that produces a
// diagnosis and answers follow-up questions about one.
//
// It is declared here, on the consumer side, deliberately. The Slack adapter
// depends on this small interface rather than on the analysis engine's own
// types, so the engine can live in another package or another branch and be
// attached with a thin adapter. Nothing in the Slack layer reasons; it only
// ferries context in and text out.
type RCA interface {
	// Diagnose runs the analysis for a service, used by the /diagnose slash
	// command.
	Diagnose(ctx context.Context, req DiagnoseRequest) (notify.Diagnosis, error)

	// AnswerFollowup answers one human question about an incident that has
	// already been diagnosed.
	AnswerFollowup(ctx context.Context, req FollowupRequest) (string, error)
}

// DiagnoseRequest asks for a fresh diagnosis.
type DiagnoseRequest struct {
	Service     string
	Environment string
	// RequestedBy is the Slack user id that asked, for the audit trail.
	RequestedBy string
}

// FollowupRequest carries everything needed to answer one question in context.
//
// It is a flat value on purpose: the analysis engine must not have to import
// the session package to answer a question, so the coupling runs one way only.
//
// The Diagnosis here is the one **frozen at the session's birth**. Its
// deterministic grounding facts - SLO state, burn rate, error budget,
// telemetry trust - are pinned, and the engine must echo them rather than
// regenerate them (PRD section 7). Everything else in this struct is untrusted
// input: Question comes from a human, and evidence notes come from telemetry
// that an attacker may be able to influence (session design edge cases E12,
// E13).
type FollowupRequest struct {
	CorrelationID string
	Service       string
	Environment   string
	Window        string

	Diagnosis notify.Diagnosis
	// ExtraEvidence is evidence gathered after the diagnosis, during this
	// conversation.
	ExtraEvidence []notify.Evidence
	// History is the conversation so far, oldest first.
	History []FollowupTurn
	// Question is the human's latest message.
	Question string
	// AskedBy is the Slack user id that asked.
	AskedBy string
}

// FollowupTurn is one exchange in the conversation history.
type FollowupTurn struct {
	// Role is "human" or "sidekick".
	Role string
	Text string
	At   time.Time
}

// Roles used in FollowupTurn.
const (
	RoleHuman    = "human"
	RoleSidekick = "sidekick"
)

// ErrRCAUnavailable is returned when no analysis engine is attached.
var ErrRCAUnavailable = errors.New("rca: analysis engine is not connected")

// UnavailableRCA is the stub used until the analysis engine is wired in.
//
// It refuses rather than improvises. A fabricated root cause is the exact
// failure the PRD exists to prevent, and it would be indistinguishable from a
// real one in the thread - so this returns an error, and the adapter says
// plainly that the engine is not connected while still showing the frozen
// deterministic facts, which remain true.
type UnavailableRCA struct{}

var _ RCA = UnavailableRCA{}

func (UnavailableRCA) Diagnose(context.Context, DiagnoseRequest) (notify.Diagnosis, error) {
	return notify.Diagnosis{}, ErrRCAUnavailable
}

func (UnavailableRCA) AnswerFollowup(context.Context, FollowupRequest) (string, error) {
	return "", ErrRCAUnavailable
}

// followupRequest builds the request from a session's frozen state.
func followupRequest(s *session.Session, question, askedBy string) FollowupRequest {
	view := s.View()

	history := make([]FollowupTurn, 0, len(view.History))
	for _, turn := range view.History {
		role := RoleHuman
		if turn.Actor == session.ActorSidekick || turn.Actor == session.ActorSystem {
			role = RoleSidekick
		}
		history = append(history, FollowupTurn{Role: role, Text: turn.Text, At: turn.At})
	}

	return FollowupRequest{
		CorrelationID: view.CorrelationID,
		Service:       view.Service,
		Environment:   view.Environment,
		Window:        view.Window,
		Diagnosis:     view.Diagnosis,
		ExtraEvidence: view.ExtraEvidence,
		History:       history,
		Question:      question,
		AskedBy:       askedBy,
	}
}
