package rca

import "strings"

// This file implements PRD 13.3's reversible-action allowlist: whether a
// proposed fix is reversible (and therefore safe to present as lower-risk)
// is decided by a small, deterministic allowlist of clearly-reversible
// action phrasing, never by the model asserting it about its own
// suggestion. Anything not matched defaults to false, the conservative
// choice - an unrecognized fix is treated as not proven reversible, which
// is what PRD 13.3's "irreversible actions... demand stronger
// confirmation" rule needs to fail safe.

// reversibleActionPatterns lists action phrases considered clearly
// reversible: an operator (or an automated system, in a future milestone)
// can undo them cheaply and without side effects on data. Deliberately
// small and specific - it is not NLP and is not meant to catch every
// reversible phrasing, only the unambiguous ones. Matching is
// case-insensitive substring matching (see reversibleForFix); a pattern
// added here should not be a plausible substring of a common irreversible
// action.
var reversibleActionPatterns = []string{
	"roll back",
	"rollback",
	"revert",
	"restart",
	"reboot",
	"scale up",
	"scale down",
	"scale out",
	"scale in",
	"raise the timeout",
	"raise timeout",
	"increase the timeout",
	"lower the timeout",
	"reduce the timeout",
	"toggle the feature flag",
	"disable the feature flag",
	"enable the feature flag",
}

// reversibleForFix reports whether text (a proposed fix's free text)
// matches a known reversible action pattern. This is the only function
// that ever sets notify.Diagnosis.Reversible / notify.Candidate.Reversible
// (see Render, contract.go) - the model itself never sets it.
func reversibleForFix(text string) bool {
	lower := strings.ToLower(text)
	for _, pattern := range reversibleActionPatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}
