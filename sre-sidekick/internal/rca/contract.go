package rca

import (
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
)

// Render assembles the final notify.Diagnosis for an incident, given the
// evidence gathered for it and the reasoner's free-text output. This is
// the only place deterministic fields are set; the model (ModelDiagnosis)
// never sets Grounding, Service, Environment, Window, CorrelationID,
// Evidence, or Reversible, no matter what it returns:
//
//   - Grounding, Service, Environment, Window, CorrelationID are copied
//     from Incident. The reasoner never sees or alters them.
//   - Evidence is exactly the ev slice passed in - the evidence gathered
//     upstream from the telemetry snapshot, with stable IDs and SigNoz
//     links. The reasoner cannot add, remove, or relink evidence.
//   - RootCause, ProposedFix, and Candidates' text is copied from the
//     model, but any EvidenceIDs a candidate (or the top-level RootCause
//     itself) cites that do not resolve to an ev ID are dropped. If every
//     cited id is dropped, the RootCause or candidate is kept with an
//     empty EvidenceIDs list rather than discarded - its text may still be
//     useful even when its citations do not check out; it is simply
//     presented without provenance. The top-level RootCause is not exempt
//     from this check just because it is the single conclusion rather than
//     one of several candidates.
//   - Reversible is NOT taken from the model: it is decided by
//     reversibleForFix (reversible.go), a small deterministic allowlist of
//     clearly-reversible action phrasing (PRD 13.3). The model never sets
//     it, and anything not recognized by the allowlist defaults to false -
//     the conservative choice.
//   - Status is always StatusDiagnosed (the indeterminate path is handled
//     before Render is ever called, by Agent.Diagnose) and Timestamp is
//     set to the current time.
func Render(inc Incident, ev []notify.Evidence, md ModelDiagnosis) notify.Diagnosis {
	known := make(map[string]bool, len(ev))
	for _, e := range ev {
		known[e.ID] = true
	}

	return notify.Diagnosis{
		CorrelationID: inc.CorrelationID,
		Service:       inc.Service,
		Environment:   inc.Environment,
		Window:        inc.Window,
		Status:        notify.StatusDiagnosed,
		Grounding:     inc.Grounding,
		RootCause:     md.RootCause,
		ProposedFix:   md.ProposedFix,
		EvidenceIDs:   filterKnownIDs(md.EvidenceIDs, known),
		Reversible:    reversibleForFix(md.ProposedFix),
		Candidates:    renderCandidates(md.Candidates, known),
		Evidence:      ev,
		Timestamp:     time.Now(),
	}
}

func renderCandidates(candidates []ModelCandidate, known map[string]bool) []notify.Candidate {
	if len(candidates) == 0 {
		return nil
	}
	out := make([]notify.Candidate, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, notify.Candidate{
			RootCause:   c.RootCause,
			ProposedFix: c.ProposedFix,
			Reversible:  reversibleForFix(c.ProposedFix),
			EvidenceIDs: filterKnownIDs(c.EvidenceIDs, known),
		})
	}
	return out
}

func filterKnownIDs(ids []string, known map[string]bool) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if known[id] {
			out = append(out, id)
		}
	}
	return out
}
