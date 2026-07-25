package rca

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/mcp"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
)

func testIncidentAndEvidence() (Incident, []notify.Evidence) {
	inc := Incident{
		CorrelationID: "corr-1",
		Service:       "support-agent",
		Environment:   "local",
		Window:        "1h",
		SLO:           "grounded-answers",
		Grounding: notify.Grounding{
			Environment:      "local",
			SLO:              "grounded-answers",
			SLOState:         "unhealthy",
			BurnRate:         14.4,
			TelemetryTrusted: true,
		},
	}
	ev := []notify.Evidence{
		{ID: "e1", Kind: notify.EvidenceKindTrace, SignozLink: "http://localhost:8080/trace/1", Note: "operation tool.search_kb, errored: timed out after 5s"},
		{ID: "e2", Kind: notify.EvidenceKindLog, SignozLink: "http://localhost:8080/logs", Note: "[ERROR] agent run failed: tool.search_kb timed out"},
	}
	return inc, ev
}

// decodeRequestBody decodes the raw chat-completions request body an
// httptest handler received, for assertions on what was sent.
func decodeRequestBody(t *testing.T, r *http.Request) chatCompletionRequest {
	t.Helper()
	var req chatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return req
}

func TestLLMReasoner_Reason_ParsesValidJSONResponse(t *testing.T) {
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
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"root_cause\":\"tool.search_kb times out after 5s, causing the agent run to fail\",\"proposed_fix\":\"increase the tool.search_kb timeout or add a circuit breaker\",\"candidates\":[{\"root_cause\":\"downstream KB search latency\",\"proposed_fix\":\"scale the KB search backend\",\"evidence_ids\":[\"e1\",\"e2\"]}]}"}}]}`))
	}))
	defer server.Close()

	reasoner := &LLMReasoner{APIKey: "test-key", Model: "deepseek/deepseek-chat", BaseURL: server.URL, HTTPClient: server.Client()}
	inc, ev := testIncidentAndEvidence()

	md, err := reasoner.Reason(context.Background(), inc, ev)
	if err != nil {
		t.Fatalf("Reason: %v", err)
	}
	if !strings.Contains(md.RootCause, "tool.search_kb") {
		t.Errorf("RootCause = %q, want it to mention tool.search_kb", md.RootCause)
	}
	if md.ProposedFix == "" {
		t.Errorf("ProposedFix is empty")
	}
	if len(md.Candidates) != 1 || len(md.Candidates[0].EvidenceIDs) != 2 {
		t.Errorf("Candidates = %+v, want one candidate citing 2 evidence ids", md.Candidates)
	}

	if !strings.Contains(gotSystemPrompt, untrustedBeginMarker) || !strings.Contains(gotSystemPrompt, untrustedEndMarker) {
		t.Errorf("system prompt does not reference the untrusted-evidence delimiters: %q", gotSystemPrompt)
	}
	if !strings.Contains(gotUserPrompt, untrustedBeginMarker) || !strings.Contains(gotUserPrompt, untrustedEndMarker) {
		t.Errorf("user prompt does not wrap evidence in the untrusted-evidence delimiters: %q", gotUserPrompt)
	}
	if !strings.Contains(gotUserPrompt, "e1") || !strings.Contains(gotUserPrompt, "e2") {
		t.Errorf("user prompt does not include the given evidence ids: %q", gotUserPrompt)
	}
}

func TestLLMReasoner_Reason_RetriesOnceOnMalformedJSON(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"not json at all"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"root_cause\":\"recovered on retry\",\"proposed_fix\":\"fix it\"}"}}]}`))
	}))
	defer server.Close()

	reasoner := &LLMReasoner{APIKey: "test-key", BaseURL: server.URL, HTTPClient: server.Client()}
	inc, ev := testIncidentAndEvidence()

	md, err := reasoner.Reason(context.Background(), inc, ev)
	if err != nil {
		t.Fatalf("Reason: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want exactly 2 (one retry)", calls)
	}
	if md.RootCause != "recovered on retry" {
		t.Errorf("RootCause = %q, want %q", md.RootCause, "recovered on retry")
	}
}

// TestLLMReasoner_Reason_ErrorsWhenJSONNeverParses locks in the fix for a
// bug where a model response that never became parseable JSON (even after
// one corrective retry) was swallowed into a fabricated ModelDiagnosis
// with nil error - a fallback whose RootCause happened to say "unable to
// determine a root cause" but whose Status, once rendered, was
// StatusDiagnosed like any other successful reasoning result. Reason must
// return an error here so Diagnose (diagnose.go) can render and deliver an
// honest indeterminate result instead.
func TestLLMReasoner_Reason_ErrorsWhenJSONNeverParses(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"still not json"}}]}`))
	}))
	defer server.Close()

	reasoner := &LLMReasoner{APIKey: "test-key", BaseURL: server.URL, HTTPClient: server.Client()}
	inc, ev := testIncidentAndEvidence()

	_, err := reasoner.Reason(context.Background(), inc, ev)
	if err == nil {
		t.Fatal("Reason returned no error for a response that never became parseable JSON, want an error")
	}
	if calls != 2 {
		t.Errorf("calls = %d, want exactly 2 (one retry, then an error without a third call)", calls)
	}
}

func TestLLMReasoner_Reason_StripsMarkdownCodeFence(t *testing.T) {
	fenced := "```json\n{\"root_cause\": \"fenced\", \"proposed_fix\": \"fix\"}\n```"
	response := chatCompletionResponse{
		Choices: []chatChoice{{Message: chatMessage{Role: "assistant", Content: fenced}}},
	}
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal test response: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer server.Close()

	reasoner := &LLMReasoner{APIKey: "test-key", BaseURL: server.URL, HTTPClient: server.Client()}
	inc, ev := testIncidentAndEvidence()

	md, err := reasoner.Reason(context.Background(), inc, ev)
	if err != nil {
		t.Fatalf("Reason: %v", err)
	}
	if md.RootCause != "fenced" {
		t.Errorf("RootCause = %q, want %q", md.RootCause, "fenced")
	}
}

func TestLLMReasoner_Reason_NoAPIKeyReturnsError(t *testing.T) {
	reasoner := &LLMReasoner{}
	inc, ev := testIncidentAndEvidence()
	if _, err := reasoner.Reason(context.Background(), inc, ev); err == nil {
		t.Fatal("expected an error when APIKey is empty")
	}
}

// fakeToolCallerCounting wraps fakeToolCaller-like behavior with a fixed
// canned response for every tool call, for the bounded tool-loop test.
type fakeToolCallerCounting struct {
	calls   int
	results mcp.ToolResult
}

func (f *fakeToolCallerCounting) CallTool(_ context.Context, _ string, _ map[string]any) (mcp.ToolResult, error) {
	f.calls++
	return f.results, nil
}

func TestLLMReasoner_Reason_BoundsToolCallLoop(t *testing.T) {
	const maxToolCalls = 3
	requestsWithTools := 0
	requestsWithoutTools := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeRequestBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		if len(req.Tools) > 0 {
			requestsWithTools++
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"signoz_search_traces","arguments":"{}"}}]}}]}`))
			return
		}
		requestsWithoutTools++
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"root_cause\":\"bounded\",\"proposed_fix\":\"fix\"}"}}]}`))
	}))
	defer server.Close()

	fakeTools := &fakeToolCallerCounting{results: mcp.ToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: `{"status":"success"}`}}}}
	reasoner := &LLMReasoner{
		APIKey:       "test-key",
		BaseURL:      server.URL,
		HTTPClient:   server.Client(),
		Tools:        fakeTools,
		ToolSchemas:  []ToolSchema{{Name: "signoz_search_traces", Description: "search traces"}},
		MaxToolCalls: maxToolCalls,
	}
	inc, ev := testIncidentAndEvidence()

	md, err := reasoner.Reason(context.Background(), inc, ev)
	if err != nil {
		t.Fatalf("Reason: %v", err)
	}
	if md.RootCause != "bounded" {
		t.Errorf("RootCause = %q, want %q", md.RootCause, "bounded")
	}
	if fakeTools.calls != maxToolCalls {
		t.Errorf("tool calls made = %d, want exactly %d (bounded by MaxToolCalls)", fakeTools.calls, maxToolCalls)
	}
	if requestsWithTools != maxToolCalls {
		t.Errorf("requests with tools enabled = %d, want %d", requestsWithTools, maxToolCalls)
	}
	if requestsWithoutTools != 1 {
		t.Errorf("requests with tools disabled (forced final answer) = %d, want 1", requestsWithoutTools)
	}
}

func TestToolSchemasFromMCPTools_FiltersToAllowlist(t *testing.T) {
	tools := []mcp.Tool{
		{Name: "signoz_search_traces", Description: "search traces", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "signoz_delete_dashboard", Description: "delete a dashboard"},
		{Name: "signoz_search_logs", Description: "search logs"},
	}
	got := ToolSchemasFromMCPTools(tools)
	if len(got) != 2 {
		t.Fatalf("got %d schemas, want 2 (allowlisted only): %+v", len(got), got)
	}
	names := map[string]bool{}
	for _, s := range got {
		names[s.Name] = true
	}
	if !names["signoz_search_traces"] || !names["signoz_search_logs"] {
		t.Errorf("got %+v, want signoz_search_traces and signoz_search_logs", got)
	}
	if names["signoz_delete_dashboard"] {
		t.Errorf("signoz_delete_dashboard must not be exposed to the model")
	}
}

func TestNewLLMReasonerFromEnv_RequiresAPIKey(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("RCA_MODEL", "")
	t.Setenv("OPENROUTER_BASE_URL", "")
	if _, err := NewLLMReasonerFromEnv(nil, nil); err == nil {
		t.Fatal("expected an error when OPENROUTER_API_KEY is unset")
	}
}

func TestNewLLMReasonerFromEnv_AppliesDefaults(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	t.Setenv("RCA_MODEL", "")
	t.Setenv("OPENROUTER_BASE_URL", "")

	reasoner, err := NewLLMReasonerFromEnv(nil, nil)
	if err != nil {
		t.Fatalf("NewLLMReasonerFromEnv: %v", err)
	}
	if reasoner.Model != defaultModel {
		t.Errorf("Model = %q, want default %q", reasoner.Model, defaultModel)
	}
	if reasoner.BaseURL != defaultOpenRouterBaseURL {
		t.Errorf("BaseURL = %q, want default %q", reasoner.BaseURL, defaultOpenRouterBaseURL)
	}
}

func TestNewLLMReasonerFromEnv_HonorsOverrides(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	t.Setenv("RCA_MODEL", "some/other-model")
	t.Setenv("OPENROUTER_BASE_URL", "https://example.test/v1/chat/completions")

	reasoner, err := NewLLMReasonerFromEnv(nil, nil)
	if err != nil {
		t.Fatalf("NewLLMReasonerFromEnv: %v", err)
	}
	if reasoner.Model != "some/other-model" {
		t.Errorf("Model = %q, want %q", reasoner.Model, "some/other-model")
	}
	if reasoner.BaseURL != "https://example.test/v1/chat/completions" {
		t.Errorf("BaseURL = %q, want the overridden URL", reasoner.BaseURL)
	}
}
