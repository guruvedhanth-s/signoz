package answer

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// VerifyNumbers is the structural half of the guarantee. Prompting a model
// not to invent numbers is a request; checking afterwards that it did not
// is an assertion. This function is the assertion.
//
// It extracts every ASCII numeric token from the candidate answer and
// requires each one to appear in the set of values the composer supplied.
// A token that was never supplied means the model produced a figure from
// nowhere, which is a bug, not a stylistic choice - the caller must discard
// the answer and fall back to the deterministic template.
//
// This is a token-membership check, not a proof that every claim is true:
// it cannot prove that a supplied value is attached to the right label, and
// it does not understand numbers written as words. It therefore pairs with
// deterministic templates and conservative fallbacks rather than replacing
// them.
//
// The check is deliberately strict: there is no whitelist of "obviously
// safe" small integers. "All 3 tiers are firing" is rejected unless 3 was
// supplied as a value, and that is why every Facts builder emits explicit
// count facts ("tiers evaluated", "SLOs exhausted", "services known").
// Supplying the count is cheap; deciding case by case which invented
// numbers are harmless is exactly the judgement this design refuses to
// make.
func VerifyNumbers(candidate string, facts Facts) error {
	candidate = withoutOpaqueProvenance(candidate, facts)
	if bad := nonASCIINumerals(candidate); len(bad) > 0 {
		sort.Strings(bad)
		return fmt.Errorf("answer contains unsupported non-ASCII numeral(s): %s", strings.Join(bad, ", "))
	}
	allowed := allowedNumbers(facts)
	var unknown []string
	for _, token := range numericTokens(candidate) {
		if _, ok := allowed[normalizeNumber(token)]; !ok {
			unknown = append(unknown, token)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	if len(unknown) > 20 {
		unknown = append(unknown[:20], "...")
	}
	return fmt.Errorf("answer states %d number(s) that were never computed: %s",
		len(unknown), strings.Join(unknown, ", "))
}

// allowedNumbers is the set of numeric tokens the answer may contain. It
// intentionally excludes provenance strings such as evaluated ranges: the
// deterministic template may print the timestamp, but clock digits must
// not become values the model can reuse in unrelated claims.
func allowedNumbers(facts Facts) map[string]struct{} {
	allowed := map[string]struct{}{}
	add := func(text string) {
		for _, token := range numericTokens(text) {
			allowed[normalizeNumber(token)] = struct{}{}
		}
	}
	add(facts.Headline)
	add(facts.Window)
	add(facts.Service)
	add(facts.Environment)
	add(facts.Caveat)
	add(facts.Reason)
	for _, fact := range facts.Values {
		add(fact.Label)
		add(fact.Value)
	}
	return allowed
}

// numericTokens extracts maximal numeric runs, including a leading sign
// and a decimal part. The sign is captured because "-264%" and "264%" are
// different claims: one says the budget is overspent, the other says it is
// nearly intact.
func numericTokens(text string) []string {
	var tokens []string
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		if !isDigit(runes[i]) {
			continue
		}
		start := i
		// Take a preceding minus sign only when it is not part of a range
		// like "12:04-12:09": a digit immediately before the sign means
		// the sign is a separator, not a negation.
		if start > 0 && runes[start-1] == '-' && (start < 2 || !isDigit(runes[start-2])) {
			start--
		}
		j := i
		for j < len(runes) {
			if isDigit(runes[j]) {
				j++
				continue
			}
			if runes[j] == ',' && j > i && j < len(runes)-1 && isDigit(runes[j+1]) {
				j++
				continue
			}
			break
		}
		if j < len(runes)-1 && runes[j] == '.' && isDigit(runes[j+1]) {
			j++
			for j < len(runes) && isDigit(runes[j]) {
				j++
			}
		}
		tokens = append(tokens, string(runes[start:j]))
		i = j - 1
	}
	return tokens
}

// normalizeNumber collapses the harmless ways of writing the same figure -
// "3.60" and "3.6", "04" and "4", "-0" and "0" - so a model that writes a
// supplied number in a slightly different style is not treated as having
// invented one. It does not collapse anything that changes the value.
func normalizeNumber(token string) string {
	negative := strings.HasPrefix(token, "-")
	token = strings.TrimPrefix(token, "-")
	token = strings.ReplaceAll(token, ",", "")
	integer, fraction, hasFraction := strings.Cut(token, ".")
	integer = strings.TrimLeft(integer, "0")
	if integer == "" {
		integer = "0"
	}
	if hasFraction {
		fraction = strings.TrimRight(fraction, "0")
	}
	normalized := integer
	if fraction != "" {
		normalized += "." + fraction
	}
	if negative && normalized != "0" {
		normalized = "-" + normalized
	}
	return normalized
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

func withoutOpaqueProvenance(candidate string, facts Facts) string {
	if facts.EvaluatedRange == "" {
		return candidate
	}
	return strings.ReplaceAll(candidate, facts.EvaluatedRange, "")
}

func nonASCIINumerals(text string) []string {
	seen := map[string]struct{}{}
	for _, r := range text {
		if isDigit(r) {
			continue
		}
		if unicode.IsDigit(r) || unicode.IsNumber(r) {
			seen[string(r)] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	return out
}
