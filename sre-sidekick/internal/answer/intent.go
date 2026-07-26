package answer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// Intent is a resolved question: which tool to call, with which arguments.
// The arguments are JSON because this is the boundary the LLM edge crosses;
// Registry.Invoke decodes and validates them before any tool runs.
type Intent struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

// Resolver turns a natural-language question into one intent from the
// closed set, or fails. Failing is a feature: an unresolvable question
// must produce "I can't answer that yet, here's what I can answer" rather
// than an improvised attempt at an answer.
type Resolver interface {
	Resolve(ctx context.Context, question string, scope ServiceArgs) (Intent, error)
}

// PatternResolver is the deterministic half of intent resolution. It
// matches the phrasings people actually use for the six intents, with no
// model call and no cost.
//
// It exists for three reasons, in order of importance:
//
//  1. It is testable. "what's my burn rate" resolving to burn_rate is an
//     assertion, not a hope about a model's mood.
//  2. It is free. Most questions in an incident channel are the same five
//     questions, and paying for a classification call on each one is the
//     kind of cost that shows up later as a rate limit (#50).
//  3. It cannot leak. No part of the question reaches a model or a query.
//
// It is deliberately not the whole story: real phrasings are unbounded, so
// ChainResolver falls back to an LLM classifier for anything unmatched.
type PatternResolver struct {
	// Registry is the closed set this resolver may return names from. A
	// pattern that matches an unregistered intent (recent_incidents before
	// #48 lands) falls through to unknown, so the capability list stays
	// truthful.
	Registry *Registry
	// KnownServices bounds service-name extraction. Names are matched
	// against this list rather than parsed out of the sentence, which is
	// what keeps rule 4 intact: no substring of the user's text is ever
	// passed to a tool, only an identifier we already knew.
	KnownServices []ServiceEntry
}

// intentPatterns maps keyword groups to intents, most specific first.
// Order matters: "how much error budget is left" contains both "budget"
// and "how", and must not fall through to slo_status.
var intentPatterns = []struct {
	intent   string
	keywords []string
}{
	{"burn_rate", []string{"burn rate", "burn-rate", "burning", "burn"}},
	{"error_budget", []string{"error budget", "budget left", "budget remaining", "budget"}},
	{"telemetry_trust", []string{
		"trust", "trustworthy", "telemetry quality", "instrumentation",
		"audit", "coverage", "why indeterminate", "can i believe",
	}},
	{"recent_incidents", []string{
		"recent incident", "recent incidents", "last incident", "past incidents",
		"what happened", "happened before", "incident history",
	}},
	{"service_inventory", []string{
		"what services", "which services", "list services", "inventory",
		"what do you know", "what can you", "what can i ask",
	}},
	{"slo_status", []string{
		"slo", "how is", "how are", "how's", "status", "doing", "healthy",
		"health", "sli", "objective",
	}},
}

// ErrNoScope reports that a question needs a service and environment but
// none could be determined - neither named in the question nor supplied by
// the caller's session scope.
var ErrNoScope = errors.New("no service/environment scope for this question")

// Resolve matches a question against the pattern table and fills in the
// service/environment scope.
func (r PatternResolver) Resolve(_ context.Context, question string, scope ServiceArgs) (Intent, error) {
	normalized := normalizeQuestion(question)
	for _, pattern := range intentPatterns {
		if !containsAny(normalized, pattern.keywords) {
			continue
		}
		if r.Registry != nil && !r.registered(pattern.intent) {
			// The phrasing was understood but the capability is not built
			// yet. Fall through to unknown so the reply lists what is
			// actually available, rather than naming a tool that will fail.
			continue
		}
		return r.buildIntent(pattern.intent, normalized, scope)
	}
	return Intent{}, r.unknown(question)
}

func (r PatternResolver) registered(name string) bool {
	for _, n := range r.Registry.Names() {
		if n == name {
			return true
		}
	}
	return false
}

func (r PatternResolver) unknown(question string) error {
	var caps []Capability
	if r.Registry != nil {
		caps = r.Registry.Capabilities()
	}
	return &UnknownIntentError{Intent: question, Capabilities: caps}
}

func (r PatternResolver) buildIntent(name, normalized string, scope ServiceArgs) (Intent, error) {
	if name == "service_inventory" {
		raw, err := json.Marshal(EmptyArgs{})
		if err != nil {
			return Intent{}, err
		}
		return Intent{Name: name, Args: raw}, nil
	}

	resolved := r.resolveScope(normalized, scope)
	if err := resolved.Validate(); err != nil {
		return Intent{}, fmt.Errorf("%w: %v", ErrNoScope, err)
	}

	var raw []byte
	var err error
	switch name {
	case "recent_incidents":
		raw, err = json.Marshal(HistoryArgs{Service: resolved.Service, Environment: resolved.Environment})
	case "telemetry_trust":
		raw, err = json.Marshal(resolved)
	default:
		raw, err = json.Marshal(SLOArgs{Service: resolved.Service, Environment: resolved.Environment})
	}
	if err != nil {
		return Intent{}, err
	}
	return Intent{Name: name, Args: raw}, nil
}

// resolveScope picks the service and environment the question is about.
// It only ever returns identifiers drawn from KnownServices or from the
// caller's session scope - never a fragment of the question itself. A
// service named in the question wins over the session default, because
// naming one is an explicit override.
func (r PatternResolver) resolveScope(normalized string, scope ServiceArgs) ServiceArgs {
	best := scope
	for _, entry := range r.KnownServices {
		if entry.Service == "" || !containsWord(normalized, strings.ToLower(entry.Service)) {
			continue
		}
		best.Service = entry.Service
		// Adopt the environment too, unless the question also names a
		// different one for the same service.
		if best.Environment == "" || !r.serviceHasEnvironment(entry.Service, best.Environment) {
			best.Environment = entry.Environment
		}
		if containsWord(normalized, strings.ToLower(entry.Environment)) {
			best.Environment = entry.Environment
			break
		}
	}
	return best
}

func (r PatternResolver) serviceHasEnvironment(service, environment string) bool {
	for _, entry := range r.KnownServices {
		if entry.Service == service && entry.Environment == environment {
			return true
		}
	}
	return false
}

// ChainResolver tries each resolver in order and returns the first
// resolution. It is how the recommended design is assembled: the
// deterministic classifier handles the common phrasings for free, and an
// LLM tool-calling classifier handles the rest.
//
// Whatever the last resolver is, the closed set still holds - a resolver
// may only return a name, and Registry.Invoke rejects any name it does not
// know. The model chooses which question to answer; it never chooses what
// the answer is.
type ChainResolver []Resolver

func (c ChainResolver) Resolve(ctx context.Context, question string, scope ServiceArgs) (Intent, error) {
	var unknown *UnknownIntentError
	for _, resolver := range c {
		intent, err := resolver.Resolve(ctx, question, scope)
		if err == nil {
			return intent, nil
		}
		var unknownErr *UnknownIntentError
		if errors.As(err, &unknownErr) {
			unknown = unknownErr
			continue
		}
		return Intent{}, err
	}
	if unknown != nil {
		return Intent{}, unknown
	}
	return Intent{}, &UnknownIntentError{Intent: question}
}

// Ask resolves a question and invokes the resulting tool. This is the
// whole read-only conversational path minus the phrasing (#45) and the
// Slack plumbing (#43): question in, typed-but-erased result out, and a
// capability list when the question is not answerable.
func Ask(ctx context.Context, registry *Registry, resolver Resolver, question string, scope ServiceArgs) (any, error) {
	intent, err := resolver.Resolve(ctx, question, scope)
	if err != nil {
		return nil, err
	}
	return registry.Invoke(ctx, intent.Name, intent.Args)
}

// normalizeQuestion lowercases and collapses a question for matching. The
// result is used only for keyword comparison and never reaches a tool
// argument or a query.
func normalizeQuestion(question string) string {
	var b strings.Builder
	b.Grow(len(question))
	lastSpace := false
	for _, r := range strings.ToLower(question) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
			lastSpace = false
		default:
			if !lastSpace {
				b.WriteRune(' ')
				lastSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func containsAny(haystack string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(haystack, needle) {
			return true
		}
	}
	return false
}

// containsWord matches a whole token, so a service called "api" does not
// match the word "rapid".
func containsWord(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	for _, field := range strings.Fields(haystack) {
		if field == needle {
			return true
		}
	}
	return false
}
