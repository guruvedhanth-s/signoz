package rca

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
)

func testFollowupContext() FollowupContext {
	return FollowupContext{
		Service:     "support-agent",
		Environment: "local",
		Window:      "1h",
		Grounding: notify.Grounding{
			Environment:      "local",
			SLO:              "grounded-answers",
			SLOState:         "unhealthy",
			BurnRate:         14.4,
			TelemetryTrusted: true,
		},
		RootCause:   "tool.search_kb times out after 5s",
		ProposedFix: "increase the tool.search_kb timeout",
		Evidence: []notify.Evidence{
			{ID: "e1", Kind: notify.EvidenceKindTrace, SignozLink: "http://localhost:8080/trace/1", Note: "operation tool.search_kb, errored: timed out after 5s"},
		},
		History: []FollowupTurn{
			{Role: RoleHumanForTest, Text: "is this the same as last week?"},
		},
		Question: "did this start after the last deploy?",
	}
}

// RoleHumanForTest mirrors slack.RoleHuman without importing the slack
// package into a test that only needs a role label.
const RoleHumanForTest = "human"

func TestLLMReasoner_AnswerFollowup_ReturnsTheModelsText(t *testing.T) {
	var gotSystemPrompt, gotUserPrompt string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeRequestBody(t, r)
		for _, m := range req.Messages {
			if m.Role == "system" {
				gotSystemPrompt = m.Content
			}
			if m.Role == "user" {
				gotUserPrompt = m.Content
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Yes, the timeout started right after the deploy that changed the tool.search_kb timeout value."}}]}`))
	}))
	defer server.Close()

	reasoner := &LLMReasoner{APIKey: "test-key", Model: "deepseek/deepseek-chat", BaseURL: server.URL, HTTPClient: server.Client()}

	answer, err := reasoner.AnswerFollowup(context.Background(), testFollowupContext())
	if err != nil {
		t.Fatalf("AnswerFollowup: %v", err)
	}
	if !strings.Contains(answer, "deploy") {
		t.Errorf("answer = %q, want it to reflect the model's text", answer)
	}

	if !strings.Contains(gotSystemPrompt, untrustedBeginMarker) || !strings.Contains(gotSystemPrompt, untrustedEndMarker) {
		t.Errorf("system prompt does not reference the untrusted-data delimiters: %q", gotSystemPrompt)
	}
	if !strings.Contains(gotUserPrompt, untrustedBeginMarker) || !strings.Contains(gotUserPrompt, untrustedEndMarker) {
		t.Errorf("user prompt does not wrap the question/history in the untrusted-data delimiters: %q", gotUserPrompt)
	}
	if !strings.Contains(gotUserPrompt, "did this start after the last deploy?") {
		t.Errorf("user prompt does not include the question: %q", gotUserPrompt)
	}
	if !strings.Contains(gotUserPrompt, "tool.search_kb times out after 5s") {
		t.Errorf("user prompt does not echo the frozen root cause: %q", gotUserPrompt)
	}
	if !strings.Contains(gotUserPrompt, "unhealthy") {
		t.Errorf("user prompt does not echo the frozen grounding: %q", gotUserPrompt)
	}
}

func TestLLMReasoner_AnswerFollowup_NoAPIKeyReturnsError(t *testing.T) {
	reasoner := &LLMReasoner{APIKey: ""}
	if _, err := reasoner.AnswerFollowup(context.Background(), testFollowupContext()); err == nil {
		t.Fatal("AnswerFollowup() error = nil, want an error for a missing API key")
	}
}

func TestLLMReasoner_AnswerFollowup_EmptyModelAnswerIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"   "}}]}`))
	}))
	defer server.Close()

	reasoner := &LLMReasoner{APIKey: "test-key", BaseURL: server.URL, HTTPClient: server.Client()}
	if _, err := reasoner.AnswerFollowup(context.Background(), testFollowupContext()); err == nil {
		t.Fatal("AnswerFollowup() error = nil, want an error for an empty answer")
	}
}
