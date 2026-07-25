package rca

import (
	"context"
	"fmt"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify/slack"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/source"
)

// SlackAdapter implements slack.RCA over a live *Agent, so the Slack
// coordinator's /diagnose command and alert-driven Announce calls run the
// real Track C pipeline (evidence gate, MCP evidence, LLM reasoner,
// deterministic presentation) instead of the UnavailableRCA stub.
//
// It is declared here, on the rca side, rather than in cmd/reliability-agent,
// so `diagnose`, `watch`, and the alert-driven detect stage can all share
// one construction path and one grounding call (PRD section 7: grounding is
// computed once, the same way, everywhere).
type SlackAdapter struct {
	Agent *Agent
	// Metrics grounds an incident in the SLO config at SLOConfigPath,
	// exactly the way `diagnose` and `watch` do.
	Metrics       source.MetricQuerier
	SLOConfigPath string
	// Window is the lookback window used for evidence gathering when a
	// human triggers a diagnosis with no window of their own (e.g. via
	// /diagnose), e.g. "1h".
	Window string
	// Now returns the current time; overridable in tests. Defaults to
	// time.Now.
	Now func() time.Time
}

var _ slack.RCA = (*SlackAdapter)(nil)

// Diagnose implements slack.RCA. It grounds the incident via
// EvaluateGrounding and runs it through the Agent, the same pipeline
// `diagnose` and the alert-driven detect stage use.
func (a *SlackAdapter) Diagnose(ctx context.Context, req slack.DiagnoseRequest) (notify.Diagnosis, error) {
	grounding, sloName, err := EvaluateGrounding(ctx, a.Metrics, a.SLOConfigPath, req.Service, req.Environment)
	if err != nil {
		return notify.Diagnosis{}, fmt.Errorf("rca: ground incident: %w", err)
	}

	incident := Incident{
		CorrelationID: fmt.Sprintf("slack-%s-%s-%d", req.Service, req.Environment, a.now().UnixNano()),
		Service:       req.Service,
		Environment:   req.Environment,
		Window:        a.window(),
		SLO:           sloName,
		Grounding:     grounding,
	}
	return a.Agent.Diagnose(ctx, incident)
}

// AnswerFollowup implements slack.RCA. Follow-up questions require the live
// LLM reasoner: there is no meaningful conversational answer a stub or a
// bare evidence gate could give, so this refuses cleanly rather than
// fabricating one when the reasoner is not the real thing.
func (a *SlackAdapter) AnswerFollowup(ctx context.Context, req slack.FollowupRequest) (string, error) {
	reasoner, ok := a.Agent.Reasoner.(*LLMReasoner)
	if !ok {
		return "", fmt.Errorf("rca: follow-up questions require the live LLM reasoner")
	}

	history := make([]FollowupTurn, 0, len(req.History))
	for _, turn := range req.History {
		history = append(history, FollowupTurn{Role: turn.Role, Text: turn.Text})
	}

	return reasoner.AnswerFollowup(ctx, FollowupContext{
		Service:       req.Service,
		Environment:   req.Environment,
		Window:        req.Window,
		Grounding:     req.Diagnosis.Grounding,
		RootCause:     req.Diagnosis.RootCause,
		ProposedFix:   req.Diagnosis.ProposedFix,
		Evidence:      req.Diagnosis.Evidence,
		ExtraEvidence: req.ExtraEvidence,
		History:       history,
		Question:      req.Question,
	})
}

func (a *SlackAdapter) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *SlackAdapter) window() string {
	if a.Window != "" {
		return a.Window
	}
	return "1h"
}
