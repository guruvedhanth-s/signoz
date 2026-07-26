package answer

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/rca"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/rca/limits"
)

// defaultWordsmithTimeout bounds one wording call. It is much shorter than
// the reasoner's own timeout because the stakes are different: a diagnosis
// is worth waiting a minute for, while a status reply that takes a minute
// has already lost to the deterministic template someone could have read
// immediately.
const defaultWordsmithTimeout = 15 * time.Second

// LLMWordsmith adapts rca.LLMReasoner to the composer's Wordsmith
// interface. It adds no HTTP handling of its own - the retry behaviour,
// error decoding, token cap and timeout all belong to the existing client,
// so there is exactly one place to fix when the provider changes.
type LLMWordsmith struct {
	// Reasoner is the shared OpenRouter client.
	Reasoner *rca.LLMReasoner
	// Timeout bounds one wording call. Zero selects
	// defaultWordsmithTimeout.
	Timeout time.Duration
}

// Phrase implements Wordsmith.
//
// Note what this method does not do: inspect, repair or retry the model's
// output. Verification happens in the composer, and a wording that states
// an uncomputed number is discarded rather than negotiated with. Keeping
// that decision out of here means the adapter cannot quietly weaken it.
func (w LLMWordsmith) Phrase(ctx context.Context, prompt Prompt) (string, error) {
	if w.Reasoner == nil {
		return "", fmt.Errorf("answer: no LLM configured")
	}
	timeout := w.Timeout
	if timeout <= 0 {
		timeout = defaultWordsmithTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return w.Reasoner.CompleteText(ctx, prompt.System, prompt.User)
}

// NewWordsmithFromReasoner adapts the already-built RCA reasoner for answer
// wording. Production callers should prefer this path so budget, rate
// limits and circuit breakers remain shared across every OpenRouter call.
func NewWordsmithFromReasoner(reasoner *rca.LLMReasoner) Wordsmith {
	if reasoner == nil {
		return nil
	}
	return LLMWordsmith{Reasoner: reasoner}
}

// NewWordsmithFromEnv builds the LLM wording layer from the same
// environment the RCA reasoner uses, and attaches the supplied limits
// manager. A missing key returns (nil, error); callers can pass nil into
// Composer for template-only mode.
func NewWordsmithFromEnv(manager *limits.Manager) (Wordsmith, error) {
	apiKey := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
	if apiKey == "" {
		return nil, fmt.Errorf("answer: OPENROUTER_API_KEY is not set; answers will use deterministic templates")
	}
	reasoner, err := rca.NewLLMReasonerFromEnv(nil, nil)
	if err != nil {
		return nil, err
	}
	reasoner.Limits = manager
	return LLMWordsmith{Reasoner: reasoner}, nil
}
