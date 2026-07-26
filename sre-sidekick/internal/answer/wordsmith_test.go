package answer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/rca"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/rca/limits"
)

// llmServer stands in for OpenRouter, replying with whatever wording the
// test wants the model to have produced.
func llmServer(t *testing.T, reply string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, err := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": reply}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server
}

func wordsmithFor(t *testing.T, reply string) Wordsmith {
	t.Helper()
	server := llmServer(t, reply)
	return LLMWordsmith{Reasoner: &rca.LLMReasoner{
		APIKey: "test-key", BaseURL: server.URL, HTTPClient: server.Client(),
	}}
}

func TestLLMWordsmithComposesEndToEnd(t *testing.T) {
	reply := "checkout-api / production is unhealthy: SLI 44.0% against a 95.0% target, burn 11.2x. Telemetry trusted. Window 1h."
	answer := Compose(context.Background(), Composer{Wordsmith: wordsmithFor(t, reply)}, statusEnvelope())
	if answer.Source != SourceLLM {
		t.Fatalf("source = %q, want llm; fallback %q", answer.Source, answer.FallbackReason)
	}
	if answer.Text != reply {
		t.Errorf("text = %q, want the model's verified wording", answer.Text)
	}
}

// The verifier must hold across the real client too, not just the stub:
// this is the path a live model actually takes.
func TestLLMWordsmithHallucinationStillFallsBack(t *testing.T) {
	reply := "checkout-api is unhealthy: SLI 44.0%, burn 11.2x, and p99 latency hit 812ms."
	answer := Compose(context.Background(), Composer{Wordsmith: wordsmithFor(t, reply)}, statusEnvelope())
	if answer.Source != SourceTemplate {
		t.Fatalf("source = %q, want template: 812 was never computed", answer.Source)
	}
	if strings.Contains(answer.Text, "812") {
		t.Error("the invented latency figure survived into the answer")
	}
}

func TestLLMWordsmithWithoutReasonerDegrades(t *testing.T) {
	answer := Compose(context.Background(), Composer{Wordsmith: LLMWordsmith{}}, statusEnvelope())
	if answer.Source != SourceTemplate {
		t.Fatalf("source = %q, want template", answer.Source)
	}
	if !strings.Contains(answer.FallbackReason, "no LLM configured") {
		t.Errorf("fallback reason = %q", answer.FallbackReason)
	}
	if answer.Text == "" {
		t.Error("no answer produced")
	}
}

// A slow provider must not hold up a reply that was already available
// deterministically before the call was made.
func TestLLMWordsmithTimeoutDegrades(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(300 * time.Millisecond):
		case <-r.Context().Done():
		}
	}))
	defer server.Close()

	wordsmith := LLMWordsmith{
		Reasoner: &rca.LLMReasoner{APIKey: "test-key", BaseURL: server.URL, HTTPClient: server.Client()},
		Timeout:  50 * time.Millisecond,
	}
	start := time.Now()
	answer := Compose(context.Background(), Composer{Wordsmith: wordsmith}, statusEnvelope())
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("composition took %v; the timeout did not bound it", elapsed)
	}
	if answer.Source != SourceTemplate {
		t.Fatalf("source = %q, want template", answer.Source)
	}
	if answer.Text == "" {
		t.Error("timeout produced no answer at all")
	}
}

func TestNewWordsmithFromEnvWithoutKey(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	wordsmith, err := NewWordsmithFromEnv(nil)
	if err == nil {
		t.Fatal("expected an error explaining that templates will be used")
	}
	if wordsmith != nil {
		t.Fatal("expected a nil Wordsmith, which Composer treats as template-only")
	}
	// The documented contract: the nil result is directly usable.
	answer := Compose(context.Background(), Composer{Wordsmith: wordsmith}, statusEnvelope())
	if answer.Source != SourceTemplate || answer.Text == "" {
		t.Errorf("nil wordsmith did not yield a template answer: %+v", answer)
	}
}

func TestNewWordsmithFromEnvUsesSharedConfig(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	t.Setenv("RCA_MODEL", "deepseek/deepseek-chat")
	manager := limits.New(limits.Config{})
	wordsmith, err := NewWordsmithFromEnv(manager)
	if err != nil {
		t.Fatalf("NewWordsmithFromEnv: %v", err)
	}
	llm, ok := wordsmith.(LLMWordsmith)
	if !ok {
		t.Fatalf("wordsmith type = %T, want LLMWordsmith", wordsmith)
	}
	if llm.Reasoner.Model != "deepseek/deepseek-chat" {
		t.Errorf("model = %q; the composer must share the reasoner's configuration", llm.Reasoner.Model)
	}
	if llm.Reasoner.Limits != manager {
		t.Error("wordsmith reasoner did not receive shared limits manager")
	}
}
