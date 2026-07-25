package rca

import (
	"testing"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
)

func TestRender_DeterministicFieldsComeFromIncidentAndEvidence(t *testing.T) {
	inc := Incident{
		CorrelationID: "corr-1",
		Service:       "checkout",
		Environment:   "prod",
		Window:        "1h",
		Grounding: notify.Grounding{
			Environment:          "prod",
			SLO:                  "checkout-availability",
			SLOState:             "unhealthy",
			BurnRate:             4.2,
			ErrorBudgetRemaining: -0.1,
			TelemetryTrusted:     true,
		},
	}
	ev := []notify.Evidence{
		{ID: "e1", Kind: notify.EvidenceKindTrace, SignozLink: "https://signoz.example/e1", Note: "trace"},
	}
	md := ModelDiagnosis{
		RootCause:   "payment gateway timeout",
		ProposedFix: "increase gateway timeout",
		Candidates: []ModelCandidate{
			{RootCause: "candidate A", ProposedFix: "fix A", EvidenceIDs: []string{"e1", "e99"}},
			{RootCause: "candidate B", ProposedFix: "fix B", EvidenceIDs: []string{"e99"}},
		},
	}

	got := Render(inc, ev, md)

	if got.Status != notify.StatusDiagnosed {
		t.Errorf("Status = %v, want %v", got.Status, notify.StatusDiagnosed)
	}
	if got.Grounding != inc.Grounding {
		t.Errorf("Grounding = %+v, want %+v (the model must not alter grounding)", got.Grounding, inc.Grounding)
	}
	if got.Service != inc.Service || got.Environment != inc.Environment ||
		got.Window != inc.Window || got.CorrelationID != inc.CorrelationID {
		t.Errorf("identity fields were not copied verbatim from Incident, got %+v", got)
	}
	if len(got.Evidence) != 1 || got.Evidence[0].SignozLink != "https://signoz.example/e1" {
		t.Errorf("Evidence must come only from the ev argument, got %+v", got.Evidence)
	}
	if got.Reversible {
		t.Errorf("Reversible must default to false for a fix not matched by the reversible allowlist, got true")
	}
	if got.RootCause != md.RootCause || got.ProposedFix != md.ProposedFix {
		t.Errorf("model root cause/fix text was not copied through, got %+v", got)
	}

	if len(got.Candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(got.Candidates))
	}
	if want := []string{"e1"}; !equalStrings(got.Candidates[0].EvidenceIDs, want) {
		t.Errorf("candidate A EvidenceIDs = %v, want %v (unknown id e99 must be dropped)", got.Candidates[0].EvidenceIDs, want)
	}
	if len(got.Candidates[1].EvidenceIDs) != 0 {
		t.Errorf("candidate B EvidenceIDs = %v, want empty (all cited ids unknown, candidate kept without provenance)", got.Candidates[1].EvidenceIDs)
	}
	if got.Candidates[1].RootCause != "candidate B" {
		t.Errorf("candidate B must be kept even though all its evidence ids were dropped")
	}

	if got.Timestamp.IsZero() {
		t.Errorf("Timestamp should be set")
	}
	if time.Since(got.Timestamp) > time.Minute {
		t.Errorf("Timestamp should be close to now, got %v", got.Timestamp)
	}
}

// TestRender_TopLevelRootCauseCitesEvidenceLikeACandidate locks in the fix
// for a bug where a Candidate's EvidenceIDs were filtered against known
// evidence (filterKnownIDs) but the top-level RootCause - the single
// stated conclusion - had no citation field at all, so its claim was never
// checked against any evidence. The top-level RootCause must go through
// the identical filterKnownIDs treatment as a Candidate: it is not exempt
// from citing evidence just because it is the conclusion rather than one
// of several hypotheses.
func TestRender_TopLevelRootCauseCitesEvidenceLikeACandidate(t *testing.T) {
	inc := Incident{Service: "checkout", Window: "1h"}
	ev := []notify.Evidence{{ID: "e1", Kind: notify.EvidenceKindTrace, Note: "trace"}}
	md := ModelDiagnosis{
		RootCause:   "payment gateway timeout",
		ProposedFix: "increase gateway timeout",
		EvidenceIDs: []string{"e1", "e99"},
	}

	got := Render(inc, ev, md)

	if want := []string{"e1"}; !equalStrings(got.EvidenceIDs, want) {
		t.Errorf("EvidenceIDs = %v, want %v (unknown id e99 must be dropped, same as a candidate's)", got.EvidenceIDs, want)
	}
}

func TestRender_NoCandidatesLeavesCandidatesNil(t *testing.T) {
	inc := Incident{Service: "checkout", Window: "1h"}
	md := ModelDiagnosis{RootCause: "x", ProposedFix: "y"}

	got := Render(inc, nil, md)

	if got.Candidates != nil {
		t.Errorf("Candidates = %v, want nil when the model returns none", got.Candidates)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
