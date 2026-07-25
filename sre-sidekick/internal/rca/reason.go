package rca

import (
	"context"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
)

// ModelDiagnosis is the ONLY thing a Reasoner (eventually an LLM-backed
// agent) authors: free text describing a root cause and a proposed fix,
// plus optional ranked candidates. Every deterministic field in the final
// notify.Diagnosis - Grounding, Service, Environment, Window,
// CorrelationID, Evidence, Reversible - is injected by Render, never by a
// Reasoner. The RCA agent never produces an SLO verdict, a score, or a
// threshold; it only explains one.
type ModelDiagnosis struct {
	RootCause   string
	ProposedFix string
	// EvidenceIDs cites the evidence (by ID, from the evidence passed to
	// Reason) that supports RootCause - the same citation contract
	// Candidates use. Render drops any id that does not resolve to
	// evidence actually gathered for this diagnosis, exactly as it does
	// for a Candidate's EvidenceIDs: RootCause is not exempt from having
	// its claim checked against real evidence just because it is the
	// top-level answer rather than one of several candidates.
	EvidenceIDs []string
	// Candidates is optional: a Reasoner can return ranked hypotheses
	// instead of (or alongside) a single RootCause/ProposedFix. Render
	// copies these through, dropping any EvidenceIDs that do not resolve
	// to evidence actually gathered for this diagnosis.
	Candidates []ModelCandidate
}

// ModelCandidate is one ranked hypothesis a Reasoner proposes. EvidenceIDs
// should cite ids from the evidence passed to Reason; ids that do not
// resolve are dropped by Render, not by the Reasoner - the model is not
// trusted to police its own citations.
type ModelCandidate struct {
	RootCause   string
	ProposedFix string
	EvidenceIDs []string
}

// Reasoner turns gathered evidence into a free-text diagnosis. In M1 there
// is no LLM and no MCP tool access yet (those land in M3/M4); StubReasoner
// below is a fixed placeholder that lets the rest of the pipeline (gate,
// evidence gathering, rendering) run and be tested end to end before the
// real reasoning agent exists.
type Reasoner interface {
	Reason(ctx context.Context, inc Incident, ev []notify.Evidence) (ModelDiagnosis, error)
}

// StubReasoner is the M1 placeholder Reasoner. It never inspects the
// incident or its evidence: it always returns the same clearly-marked
// placeholder text, citing no evidence, so nobody mistakes its output for a
// real diagnosis.
type StubReasoner struct{}

// Reason implements Reasoner.
func (StubReasoner) Reason(context.Context, Incident, []notify.Evidence) (ModelDiagnosis, error) {
	return ModelDiagnosis{
		RootCause:   "M1 stub: reasoning not yet implemented",
		ProposedFix: "M1 stub: no proposed fix yet - the RCA reasoning agent lands in a later milestone",
	}, nil
}
