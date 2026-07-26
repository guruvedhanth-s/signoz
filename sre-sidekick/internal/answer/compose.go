package answer

import (
	"context"
	"fmt"
	"strings"
)

// Wordsmith is the model's entire role in the conversational path: it is
// handed pre-formatted facts and asked for wording. It cannot compute,
// because it receives no raw values; it cannot reinterpret the question,
// because it never sees one; and it cannot be trusted with the result,
// because VerifyNumbers checks the output before anyone reads it.
//
// It is an interface so the composer is fully testable without an API key,
// and so #50's cost controls can wrap it at one choke point rather than
// three.
type Wordsmith interface {
	// Phrase returns a natural-language rendering of the supplied prompt.
	// An error means "no wording available", which is a normal condition -
	// no key configured, rate limited, over budget - and must degrade to
	// the template rather than fail the answer.
	Phrase(ctx context.Context, prompt Prompt) (string, error)
}

// Prompt is the two-part message pair sent to a model: fixed rules, and
// the facts for one answer. They are separate fields rather than one
// concatenated string so the rules land in the system role, where a model
// weights them more heavily than anything in the user turn.
type Prompt struct {
	System string `json:"system"`
	User   string `json:"user"`
}

// AnswerSource records how an answer's wording was produced. It is
// reported rather than hidden: an operator debugging a strange reply
// should be able to see immediately whether the model wrote it, and if
// not, why not.
type AnswerSource string

const (
	// SourceLLM means the model's wording passed verification and was used.
	SourceLLM AnswerSource = "llm"
	// SourceTemplate means the deterministic template produced the answer,
	// either because no Wordsmith was configured or because the model's
	// wording was rejected.
	SourceTemplate AnswerSource = "template"
)

// Answer is a composed reply, ready for the Slack adapter to render.
type Answer struct {
	// Text is the wording to post.
	Text string `json:"text"`
	// Source says who wrote Text.
	Source AnswerSource `json:"source"`
	// FallbackReason explains a SourceTemplate answer that had a Wordsmith
	// available - a rejected number, a failed call. Empty when no model
	// was configured at all, which is not a fallback but a mode.
	FallbackReason string `json:"fallback_reason,omitempty"`
	// Facts is the exact input the wording was produced from, kept for
	// logging, tests and Block Kit rendering.
	Facts Facts `json:"facts"`
}

// Composer turns typed tool results into sentences.
//
// Note what it does not have: a field for the user's question. The
// composer receives values and an intent, never raw question text with
// authority to reinterpret it. Interpretation already happened in intent
// resolution, and re-admitting the question here would hand a model both
// the numbers and a instruction channel pointing at them.
type Composer struct {
	// Wordsmith is optional. Nil means template-only, which is a
	// first-class supported configuration and not a degraded one.
	Wordsmith Wordsmith
}

// Compose renders a typed envelope into an answer.
func Compose[T any](ctx context.Context, c Composer, env Envelope[T]) Answer {
	return c.ComposeFacts(ctx, FactsFrom(env))
}

// ComposeAny renders a result that came back through Registry.Invoke,
// where the static type was lost at the LLM edge. A payload that is not an
// Envelope is refused rather than guessed at.
func (c Composer) ComposeAny(ctx context.Context, result any) (Answer, error) {
	erasable, ok := result.(Erasable)
	if !ok {
		return Answer{}, fmt.Errorf("answer: cannot compose %T; expected an Envelope", result)
	}
	return c.ComposeFacts(ctx, FactsFromAny(erasable.Erase())), nil
}

// ComposeFacts is the composition path everything else funnels into.
//
// The template is rendered first, unconditionally. That ordering is
// deliberate: there is always a correct answer available before the model
// is consulted, so every failure mode - no key, timeout, rate limit,
// hallucinated figure - falls back to something already computed rather
// than to an error path.
func (c Composer) ComposeFacts(ctx context.Context, facts Facts) Answer {
	template := RenderTemplate(facts)
	answer := Answer{Text: template, Source: SourceTemplate, Facts: facts}
	if c.Wordsmith == nil {
		return answer
	}

	phrased, err := c.Wordsmith.Phrase(ctx, BuildPrompt(facts))
	if err != nil {
		answer.FallbackReason = "wording unavailable: " + err.Error()
		return answer
	}
	phrased = strings.TrimSpace(phrased)
	if phrased == "" {
		answer.FallbackReason = "wording was empty"
		return answer
	}
	if err := VerifyNumbers(phrased, facts); err != nil {
		// The model stated a figure nobody computed. This is the failure
		// the whole product exists to prevent, so the wording is discarded
		// outright - not repaired, not retried into acceptance.
		answer.FallbackReason = "rejected model wording: " + err.Error()
		return answer
	}

	answer.Text = phrased
	answer.Source = SourceLLM
	return answer
}

// composerSystemPrompt is intentionally small and boring. The model's only
// job is wording; every fact is in the input, and every rule below exists
// to stop it doing anything else.
const composerSystemPrompt = `You are the wording layer of an SRE reliability assistant. You turn already-computed facts into one short, plain reply.

RULES:
1. Every number in your reply must be copied EXACTLY from the VALUES below, character for character, including the sign, the decimal places and the unit (for example "3.6x", "-264.0%", "95.0%"). Never round, never reformat, never compute, never estimate, and never state any number that is not in VALUES. Your reply is rejected automatically if it contains a number that was not given to you.
2. Lead with the verdict, then the numbers, then the caveat.
3. Always state the window the answer covers. Never omit it.
4. Never suggest, imply or speculate about a cause. You are reporting status, not diagnosing. If asked why, the honest answer is that this reply does not determine causes.
5. If the facts say the answer is indeterminate, say so plainly and give the stated reason. Do not soften it, do not guess, and do not offer a number.
6. Two or three sentences at most. No bullet lists, no headings, no markdown, no emoji.`

// BuildPrompt renders Facts into a system/user pair. Every value is
// already a string; nothing is formatted at prompt-build time, so what the
// verifier allows and what the model is shown are the same set by
// construction.
func BuildPrompt(facts Facts) Prompt {
	var b strings.Builder
	b.WriteString("INTENT: ")
	b.WriteString(facts.Intent)
	if scope := scopeLabel(facts); scope != "" {
		b.WriteString("\nSCOPE: ")
		b.WriteString(scope)
	}
	b.WriteString("\nVERDICT: ")
	b.WriteString(facts.Headline)
	if facts.Indeterminate {
		b.WriteString("\nINDETERMINATE: yes")
		if facts.Reason != "" {
			b.WriteString("\nREASON: ")
			b.WriteString(facts.Reason)
		}
	}
	if facts.Window != "" {
		b.WriteString("\nWINDOW: ")
		b.WriteString(facts.Window)
	}
	if facts.EvaluatedRange != "" {
		b.WriteString("\nEVALUATED: ")
		b.WriteString(facts.EvaluatedRange)
	}
	if facts.Caveat != "" {
		b.WriteString("\nCAVEAT: ")
		b.WriteString(facts.Caveat)
	}
	if len(facts.Values) > 0 {
		b.WriteString("\nVALUES:")
		for _, fact := range facts.Values {
			b.WriteString("\n- ")
			b.WriteString(fact.Label)
			b.WriteString(": ")
			b.WriteString(fact.Value)
		}
	}
	b.WriteString("\n\nWrite the reply now.")
	return Prompt{System: composerSystemPrompt, User: b.String()}
}
