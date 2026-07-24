package rca

import (
	"os"
	"testing"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
)

func concentratedSignals(samples int, dominantShare float64) Signals {
	return Signals{
		ErrorSampleCount: samples,
		Concentration: Concentration{
			Present:           true,
			TopOperation:      "tool.search_kb",
			TopOperationShare: dominantShare,
			TopException:      "TimeoutError",
			TopExceptionShare: dominantShare,
		},
	}
}

func TestDecide_BelowMinSamples_CouldNotDetermine(t *testing.T) {
	cfg := DefaultPresentationConfig()
	signals := concentratedSignals(2, 1.0) // below the default min of 3
	signals.LatencyShift = Metric{Present: true, Value: 10}

	got := Decide(signals, ModelDiagnosis{RootCause: "x"}, cfg)

	if got != ModeCouldNotDetermine {
		t.Errorf("Decide() = %v, want %v", got, ModeCouldNotDetermine)
	}
}

func TestDecide_NoDominantPattern_CouldNotDetermine(t *testing.T) {
	cfg := DefaultPresentationConfig()
	signals := concentratedSignals(10, 0.3) // below the default 0.6 threshold

	got := Decide(signals, ModelDiagnosis{RootCause: "x"}, cfg)

	if got != ModeCouldNotDetermine {
		t.Errorf("Decide() = %v, want %v", got, ModeCouldNotDetermine)
	}
}

func TestDecide_ZeroErrorSamplesAndNoConcentration_CouldNotDetermine(t *testing.T) {
	cfg := DefaultPresentationConfig()

	got := Decide(Signals{}, ModelDiagnosis{}, cfg)

	if got != ModeCouldNotDetermine {
		t.Errorf("Decide() = %v, want %v", got, ModeCouldNotDetermine)
	}
}

func TestDecide_ConcentratedPlusLatencyShift_Conclusion(t *testing.T) {
	cfg := DefaultPresentationConfig()
	signals := concentratedSignals(10, 1.0)
	signals.LatencyShift = Metric{Present: true, Value: 21.2} // above the default 1.5 threshold

	got := Decide(signals, ModelDiagnosis{RootCause: "x"}, cfg)

	if got != ModeConclusion {
		t.Errorf("Decide() = %v, want %v", got, ModeConclusion)
	}
}

func TestDecide_ConcentratedOnly_OneGoldenSignal_RankedHypotheses(t *testing.T) {
	cfg := DefaultPresentationConfig()
	signals := concentratedSignals(10, 1.0) // concentration alone: 1 golden signal

	got := Decide(signals, ModelDiagnosis{RootCause: "x", Candidates: []ModelCandidate{{RootCause: "a"}, {RootCause: "b"}}}, cfg)

	if got != ModeRankedHypotheses {
		t.Errorf("Decide() = %v, want %v", got, ModeRankedHypotheses)
	}
}

func TestDecide_ConcentratedOnly_NoCandidates_StillRankedHypotheses(t *testing.T) {
	// Even when the model committed to one flat answer, the rules (not the
	// model) decide the structure: one supported golden signal is not
	// enough to call it a Conclusion.
	cfg := DefaultPresentationConfig()
	signals := concentratedSignals(10, 1.0)

	got := Decide(signals, ModelDiagnosis{RootCause: "x"}, cfg)

	if got != ModeRankedHypotheses {
		t.Errorf("Decide() = %v, want %v", got, ModeRankedHypotheses)
	}
}

func TestDecide_ErrorRateDeltaCountsTowardErrorsSignal_NotConcentrationAlone(t *testing.T) {
	cfg := DefaultPresentationConfig()
	signals := Signals{
		ErrorSampleCount: 10,
		Concentration:    Concentration{Present: true, TopOperationShare: 0.3, TopExceptionShare: 0.3}, // below threshold
		ErrorRateDelta:   Metric{Present: true, Value: 5.0},                                            // well above the default 0.5 threshold
	}

	got := Decide(signals, ModelDiagnosis{RootCause: "x"}, cfg)

	// Errors signal is supported via the rate delta even though
	// concentration alone would not clear the bar, but that's still only
	// ONE golden signal (errors) - not enough for a Conclusion.
	if got != ModeRankedHypotheses {
		t.Errorf("Decide() = %v, want %v", got, ModeRankedHypotheses)
	}
}

func TestDecide_MultipleModelCandidates_ForcesRankedHypothesesEvenWithTwoSignals(t *testing.T) {
	cfg := DefaultPresentationConfig()
	signals := concentratedSignals(10, 1.0)
	signals.LatencyShift = Metric{Present: true, Value: 21.2}

	got := Decide(signals, ModelDiagnosis{Candidates: []ModelCandidate{{RootCause: "a"}, {RootCause: "b"}}}, cfg)

	if got != ModeRankedHypotheses {
		t.Errorf("Decide() = %v, want %v (model split its own answer across multiple evidence-grounded candidates)", got, ModeRankedHypotheses)
	}
}

func TestDecide_SaturationCountsAsASeparateGoldenSignal(t *testing.T) {
	cfg := DefaultPresentationConfig()
	signals := concentratedSignals(10, 1.0)
	signals.Saturation = BoolSignal{Present: true, Value: true}

	got := Decide(signals, ModelDiagnosis{RootCause: "x"}, cfg)

	if got != ModeConclusion {
		t.Errorf("Decide() = %v, want %v", got, ModeConclusion)
	}
}

func TestDecide_ConfigTunesThresholds(t *testing.T) {
	cfg := DefaultPresentationConfig()
	cfg.MinErrorSamples = 100 // deliberately raised very high

	signals := concentratedSignals(10, 1.0)
	signals.LatencyShift = Metric{Present: true, Value: 21.2}

	got := Decide(signals, ModelDiagnosis{RootCause: "x"}, cfg)

	if got != ModeCouldNotDetermine {
		t.Errorf("Decide() = %v, want %v (raising min_error_samples must flip a would-be Conclusion to CouldNotDetermine)", got, ModeCouldNotDetermine)
	}
}

func TestSupportingSignals_Count(t *testing.T) {
	cfg := DefaultPresentationConfig()
	signals := concentratedSignals(10, 1.0)
	signals.LatencyShift = Metric{Present: true, Value: 21.2}
	signals.Saturation = BoolSignal{Present: true, Value: true}

	got := supportingSignals(signals, cfg)

	if !got.Errors || !got.Latency || !got.Saturation {
		t.Fatalf("supportingSignals() = %+v, want all true", got)
	}
	if got.Count() != 3 {
		t.Errorf("Count() = %d, want 3", got.Count())
	}
}

func TestPresent_Conclusion_SingleRootCauseNoCandidates(t *testing.T) {
	inc := Incident{CorrelationID: "corr-1", Service: "support-agent", Environment: "local", Window: "1h"}
	ev := []notify.Evidence{{ID: "e1", Kind: notify.EvidenceKindTrace, Note: "trace"}}
	md := ModelDiagnosis{RootCause: "tool.search_kb times out", ProposedFix: "raise the timeout"}

	got := Present(inc, ev, md, Signals{}, ModeConclusion, DefaultPresentationConfig())

	if got.Status != notify.StatusDiagnosed {
		t.Errorf("Status = %v, want %v", got.Status, notify.StatusDiagnosed)
	}
	if got.RootCause != md.RootCause || got.ProposedFix != md.ProposedFix {
		t.Errorf("RootCause/ProposedFix = %q/%q, want the model's text copied through", got.RootCause, got.ProposedFix)
	}
	if got.Candidates != nil {
		t.Errorf("Candidates = %v, want nil for a Conclusion", got.Candidates)
	}
	if !got.Reversible {
		t.Errorf("Reversible = false, want true (\"raise the timeout\" matches the allowlist)")
	}
	if len(got.MissingEvidence) != 0 {
		t.Errorf("MissingEvidence = %v, want empty for a Conclusion", got.MissingEvidence)
	}
}

func TestPresent_RankedHypotheses_CandidatesSetRootCauseEmpty(t *testing.T) {
	inc := Incident{Service: "checkout", Window: "1h"}
	ev := []notify.Evidence{{ID: "e1", Kind: notify.EvidenceKindTrace, Note: "trace"}}
	md := ModelDiagnosis{
		Candidates: []ModelCandidate{
			{RootCause: "candidate A", ProposedFix: "restart the pod", EvidenceIDs: []string{"e1"}},
			{RootCause: "candidate B", ProposedFix: "fix B"},
		},
	}

	got := Present(inc, ev, md, Signals{}, ModeRankedHypotheses, DefaultPresentationConfig())

	if got.Status != notify.StatusDiagnosed {
		t.Errorf("Status = %v, want %v", got.Status, notify.StatusDiagnosed)
	}
	if got.RootCause != "" || got.ProposedFix != "" {
		t.Errorf("RootCause/ProposedFix = %q/%q, want empty (Candidates set instead, per notify contract)", got.RootCause, got.ProposedFix)
	}
	if len(got.Candidates) != 2 {
		t.Fatalf("Candidates = %+v, want 2", got.Candidates)
	}
	if !got.Candidates[0].Reversible {
		t.Errorf("Candidates[0].Reversible = false, want true (\"restart the pod\" matches the allowlist)")
	}
}

func TestPresent_RankedHypotheses_SingleModelAnswerWrappedAsOneCandidate(t *testing.T) {
	// The rules decided RankedHypotheses even though the model returned one
	// flat RootCause/ProposedFix (no Candidates) - Present must still not
	// leak that text into the top-level RootCause field.
	inc := Incident{Service: "checkout", Window: "1h"}
	md := ModelDiagnosis{RootCause: "maybe the database", ProposedFix: "investigate further"}

	got := Present(inc, nil, md, Signals{}, ModeRankedHypotheses, DefaultPresentationConfig())

	if got.RootCause != "" {
		t.Errorf("RootCause = %q, want empty", got.RootCause)
	}
	if len(got.Candidates) != 1 || got.Candidates[0].RootCause != md.RootCause {
		t.Fatalf("Candidates = %+v, want the model's flat answer wrapped as one candidate", got.Candidates)
	}
}

func TestPresent_CouldNotDetermine_SuppressesModelRootCause(t *testing.T) {
	inc := Incident{CorrelationID: "corr-9", Service: "checkout", Window: "1h"}
	ev := []notify.Evidence{{ID: "e1", Kind: notify.EvidenceKindTrace, Note: "trace"}}
	md := ModelDiagnosis{RootCause: "the model is very sure it is the database", ProposedFix: "restart the database"}
	signals := Signals{ErrorSampleCount: 1}

	got := Present(inc, ev, md, signals, ModeCouldNotDetermine, DefaultPresentationConfig())

	if got.Status != notify.StatusIndeterminate {
		t.Errorf("Status = %v, want %v", got.Status, notify.StatusIndeterminate)
	}
	if got.RootCause != "" || got.ProposedFix != "" {
		t.Errorf("RootCause/ProposedFix = %q/%q, want empty - the model's asserted root cause must be suppressed", got.RootCause, got.ProposedFix)
	}
	if got.Candidates != nil {
		t.Errorf("Candidates = %v, want nil", got.Candidates)
	}
	if len(got.MissingEvidence) == 0 {
		t.Errorf("MissingEvidence must be populated explaining why no cause was presented")
	}
	// Distinct from the gate's "telemetry not trusted" wording.
	for _, reason := range got.MissingEvidence {
		if reason == "" {
			t.Errorf("MissingEvidence entries must not be empty")
		}
	}
	if got.CorrelationID != inc.CorrelationID {
		t.Errorf("CorrelationID = %q, want %q", got.CorrelationID, inc.CorrelationID)
	}
}

func TestLoadPresentationConfig_MissingFile_ReturnsDefaults(t *testing.T) {
	got, err := LoadPresentationConfig("/nonexistent/sidekick.yaml")
	if err != nil {
		t.Fatalf("LoadPresentationConfig: %v", err)
	}
	if got != DefaultPresentationConfig() {
		t.Errorf("got %+v, want defaults %+v", got, DefaultPresentationConfig())
	}
}

func TestLoadPresentationConfig_EmptyPath_ReturnsDefaults(t *testing.T) {
	got, err := LoadPresentationConfig("")
	if err != nil {
		t.Fatalf("LoadPresentationConfig: %v", err)
	}
	if got != DefaultPresentationConfig() {
		t.Errorf("got %+v, want defaults %+v", got, DefaultPresentationConfig())
	}
}

func TestLoadPresentationConfig_PartialOverride_FillsRestFromDefaults(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/sidekick.yaml"
	if err := os.WriteFile(path, []byte("min_error_samples: 10\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadPresentationConfig(path)
	if err != nil {
		t.Fatalf("LoadPresentationConfig: %v", err)
	}
	if got.MinErrorSamples != 10 {
		t.Errorf("MinErrorSamples = %d, want 10", got.MinErrorSamples)
	}
	if got.ConcentrationThreshold != DefaultPresentationConfig().ConcentrationThreshold {
		t.Errorf("ConcentrationThreshold = %v, want the default (unset in the file)", got.ConcentrationThreshold)
	}
}

func TestLoadPresentationConfig_InvalidYAML_Errors(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/sidekick.yaml"
	if err := os.WriteFile(path, []byte("min_error_samples: [this is not a number\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadPresentationConfig(path); err == nil {
		t.Fatal("expected an error for invalid YAML, got nil")
	}
}
