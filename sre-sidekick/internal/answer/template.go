package answer

import (
	"strings"
)

// RenderTemplate is the deterministic answer. No model is involved, and
// none is required: this is a genuinely usable reply, not a degraded stub.
// It is what gets posted when the LLM is unconfigured, unavailable, over
// budget, or caught stating a number nobody computed.
//
// The structure is fixed by the style rules, in this order:
//
//  1. Verdict first. Someone scanning an incident channel reads the first
//     few words and nothing else; those words must be the answer.
//  2. Then the numbers, already formatted.
//  3. Then the caveat - trust, completeness.
//  4. Then the window. Never omitted: "burn rate 3.6x" without "over the
//     last hour" is a misleading sentence even when the figure is right.
//
// It states no cause. "How is it doing" is answered with how it is doing;
// inventing a cause from an SLO number is precisely the hallucination the
// product refuses, and root-causing is what diagnose is for.
func RenderTemplate(facts Facts) string {
	var b strings.Builder

	scope := scopeLabel(facts)
	if scope != "" {
		b.WriteString(scope)
		b.WriteString(" — ")
	}

	if facts.Indeterminate {
		b.WriteString("indeterminate. ")
		reason := strings.TrimSpace(facts.Reason)
		if reason == "" {
			reason = "the telemetry behind this answer was not trusted"
		}
		b.WriteString("I can't answer that: ")
		b.WriteString(strings.TrimSuffix(reason, "."))
		b.WriteString(".")
		if provenance := provenanceSentence(facts); provenance != "" {
			b.WriteString(" ")
			b.WriteString(provenance)
		}
		return b.String()
	}

	b.WriteString(facts.Headline)
	if values := joinValues(facts.Values); values != "" {
		b.WriteString(": ")
		b.WriteString(values)
	}
	b.WriteString(".")

	if facts.Caveat != "" {
		b.WriteString(" ")
		b.WriteString(facts.Caveat)
	}
	if provenance := provenanceSentence(facts); provenance != "" {
		b.WriteString(" ")
		b.WriteString(provenance)
	}
	return b.String()
}

func scopeLabel(facts Facts) string {
	switch {
	case facts.Service != "" && facts.Environment != "":
		return facts.Service + " / " + facts.Environment
	case facts.Service != "":
		return facts.Service
	default:
		return ""
	}
}

func joinValues(values []Fact) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		if value.Value == "" {
			continue
		}
		if value.Label == "" {
			parts = append(parts, value.Value)
			continue
		}
		parts = append(parts, value.Label+" "+value.Value)
	}
	return strings.Join(parts, ", ")
}

// provenanceSentence renders the window and evaluated range. It is a
// separate function because every path through RenderTemplate must call
// it - including the refusal path, where "I couldn't tell you for the last
// hour" and "I couldn't tell you, ever" are different statements.
func provenanceSentence(facts Facts) string {
	var parts []string
	if facts.Window != "" {
		parts = append(parts, "Window "+facts.Window)
	}
	if facts.EvaluatedRange != "" {
		parts = append(parts, "evaluated "+facts.EvaluatedRange)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ", ") + "."
}
