package rca

import (
	"context"
	"fmt"
	"testing"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/evidence"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/mcp"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/source"
)

func TestAgent_Diagnose_DeliversDiagnosedResultToNotifier(t *testing.T) {
	// Presentation rules (PRD 13.4) require enough concentrated error
	// evidence before a diagnosis counts as more than CouldNotDetermine -
	// see TestAgent_Diagnose_Presentation_* below for the rule boundaries
	// themselves. This test only cares about the delivery routing (a
	// diagnosed - here, ranked-hypotheses - result goes to NotifyDiagnosis,
	// never NotifyIndeterminate).
	fake := notify.NewFake()
	agent := &Agent{
		Gate:           &SourceEvidenceGate{Source: diagnosableGateSource()},
		EvidenceSource: fakeEvidenceSource{ev: concentratedErrorEvidence(5)},
		Reasoner:       StubReasoner{},
		Notifier:       fake,
	}
	inc := Incident{CorrelationID: "corr-3", Service: "checkout", Environment: "prod", Window: "1h", Grounding: notify.Grounding{TelemetryTrusted: true}}

	got, err := agent.Diagnose(context.Background(), inc)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}

	last, ok := fake.LastDiagnosis()
	if !ok {
		t.Fatal("expected the Notifier to receive a diagnosis, got none")
	}
	if last.CorrelationID != got.CorrelationID || last.RootCause != got.RootCause {
		t.Errorf("Notifier received %+v, want it to match the returned diagnosis %+v", last, got)
	}
	if len(fake.Indeterminates) != 0 {
		t.Errorf("expected no indeterminate notifications on the diagnosed path, got %+v", fake.Indeterminates)
	}
}

func TestAgent_Diagnose_DeliversIndeterminateResultToNotifier(t *testing.T) {
	fake := notify.NewFake()
	agent := &Agent{
		Gate:     &SourceEvidenceGate{Source: source.MemorySource{Data: evidence.Snapshot{}}},
		Reasoner: failingReasoner{t: t},
		Notifier: fake,
	}
	inc := Incident{CorrelationID: "corr-4", Service: "checkout", Environment: "prod", Window: "1h"}

	got, err := agent.Diagnose(context.Background(), inc)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if got.Status != notify.StatusIndeterminate {
		t.Fatalf("Status = %v, want %v", got.Status, notify.StatusIndeterminate)
	}

	last, ok := fake.LastIndeterminate()
	if !ok {
		t.Fatal("expected the Notifier to receive NotifyIndeterminate, got none")
	}
	if last.CorrelationID != got.CorrelationID {
		t.Errorf("Notifier received CorrelationID %q, want %q", last.CorrelationID, got.CorrelationID)
	}
	if len(fake.Diagnoses) != 0 {
		t.Errorf("expected no NotifyDiagnosis calls on the indeterminate path, got %+v", fake.Diagnoses)
	}
}

func TestAgent_Diagnose_NilNotifierIsANoOp(t *testing.T) {
	agent := &Agent{
		Gate:     &SourceEvidenceGate{Source: source.MemorySource{Data: evidence.Snapshot{}}},
		Reasoner: failingReasoner{t: t},
	}
	if _, err := agent.Diagnose(context.Background(), Incident{Service: "s", Window: "1h"}); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
}

// failingReasoner fails the test if it is ever called. Used to prove the
// Agent does not invoke the reasoner on the indeterminate path.
type failingReasoner struct{ t *testing.T }

func (f failingReasoner) Reason(context.Context, Incident, []notify.Evidence) (ModelDiagnosis, error) {
	f.t.Fatal("Reasoner must not be called when the evidence gate is insufficient")
	return ModelDiagnosis{}, nil
}

// TestAgent_Diagnose_UntrustedTelemetry_NeverReachesReasoner locks in PRD
// 13.4 rule 1 (also the documented contract on notify.Grounding.
// TelemetryTrusted itself: "when false, diagnosis must be
// StatusIndeterminate"). An incident whose SLO completeness gate marked the
// telemetry untrusted must never reach the evidence gate or the reasoner,
// however much error evidence happens to be present - abundant evidence
// does not make untrustworthy telemetry trustworthy.
func TestAgent_Diagnose_UntrustedTelemetry_NeverReachesReasoner(t *testing.T) {
	agent := &Agent{
		Gate:           &SourceEvidenceGate{Source: diagnosableGateSource()},
		EvidenceSource: fakeEvidenceSource{ev: concentratedErrorEvidence(50)},
		Reasoner:       failingReasoner{t: t},
	}
	inc := Incident{
		CorrelationID: "corr-untrusted",
		Service:       "checkout",
		Environment:   "prod",
		Window:        "1h",
		Grounding:     notify.Grounding{SLOState: "unhealthy", TelemetryTrusted: false},
	}

	got, err := agent.Diagnose(context.Background(), inc)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if got.Status != notify.StatusIndeterminate {
		t.Fatalf("Status = %v, want %v (untrusted telemetry must never yield a diagnosis)", got.Status, notify.StatusIndeterminate)
	}
	if got.RootCause != "" {
		t.Errorf("RootCause = %q, want empty - a root cause must never be stated on untrusted telemetry", got.RootCause)
	}
	if got.Grounding != inc.Grounding {
		t.Errorf("Grounding = %+v, want %+v", got.Grounding, inc.Grounding)
	}
	if len(got.MissingEvidence) == 0 {
		t.Errorf("expected MissingEvidence to explain the refusal")
	}
}

// erroringReasoner always fails, simulating a transport error, an LLM
// outage, or (via reasoner_llm.go's loop) a response that never became
// parseable JSON.
type erroringReasoner struct{ err error }

func (r erroringReasoner) Reason(context.Context, Incident, []notify.Evidence) (ModelDiagnosis, error) {
	return ModelDiagnosis{}, r.err
}

// TestAgent_Diagnose_ReasonerFailure_DeliversIndeterminateNotSilence locks
// in two things: a reasoner failure must render and deliver an honest
// indeterminate diagnosis (never a fabricated "diagnosed" result whose text
// happens to admit uncertainty), and it must never be silently dropped -
// on-call must still hear about the incident even when the LLM call itself
// failed.
func TestAgent_Diagnose_ReasonerFailure_DeliversIndeterminateNotSilence(t *testing.T) {
	fake := notify.NewFake()
	agent := &Agent{
		Gate:           &SourceEvidenceGate{Source: diagnosableGateSource()},
		EvidenceSource: fakeEvidenceSource{ev: concentratedErrorEvidence(5)},
		Reasoner:       erroringReasoner{err: fmt.Errorf("openrouter: connection refused")},
		Notifier:       fake,
	}
	inc := Incident{
		CorrelationID: "corr-reasoner-fail",
		Service:       "checkout",
		Environment:   "prod",
		Window:        "1h",
		Grounding:     notify.Grounding{TelemetryTrusted: true},
	}

	got, err := agent.Diagnose(context.Background(), inc)
	if err != nil {
		t.Fatalf("Diagnose returned an error instead of an indeterminate diagnosis: %v", err)
	}
	if got.Status != notify.StatusIndeterminate {
		t.Fatalf("Status = %v, want %v", got.Status, notify.StatusIndeterminate)
	}
	if got.RootCause != "" {
		t.Errorf("RootCause = %q, want empty", got.RootCause)
	}
	if len(got.MissingEvidence) == 0 {
		t.Errorf("expected MissingEvidence to explain the reasoner failure")
	}
	if len(got.Evidence) != 5 {
		t.Errorf("Evidence = %+v, want the 5 items already gathered to still be attached", got.Evidence)
	}

	last, ok := fake.LastIndeterminate()
	if !ok {
		t.Fatal("expected the Notifier to receive NotifyIndeterminate - a reasoner failure must not be silent")
	}
	if last.CorrelationID != got.CorrelationID {
		t.Errorf("Notifier received CorrelationID %q, want %q", last.CorrelationID, got.CorrelationID)
	}
}

func TestAgent_Diagnose_IndeterminateWhenEvidenceInsufficient(t *testing.T) {
	agent := &Agent{
		Gate:     &SourceEvidenceGate{Source: source.MemorySource{Data: evidence.Snapshot{}}},
		Reasoner: failingReasoner{t: t},
	}
	inc := Incident{
		CorrelationID: "corr-1",
		Service:       "checkout",
		Environment:   "prod",
		Window:        "1h",
		Grounding:     notify.Grounding{SLOState: "unhealthy", TelemetryTrusted: true},
	}

	got, err := agent.Diagnose(context.Background(), inc)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if got.Status != notify.StatusIndeterminate {
		t.Errorf("Status = %v, want %v", got.Status, notify.StatusIndeterminate)
	}
	if got.Grounding != inc.Grounding {
		t.Errorf("Grounding = %+v, want %+v", got.Grounding, inc.Grounding)
	}
	if got.Service != inc.Service || got.Environment != inc.Environment ||
		got.Window != inc.Window || got.CorrelationID != inc.CorrelationID {
		t.Errorf("identity fields were not preserved, got %+v", got)
	}
	if len(got.MissingEvidence) == 0 {
		t.Errorf("expected MissingEvidence to be populated")
	}
	if got.RootCause != "" || got.ProposedFix != "" {
		t.Errorf("RootCause/ProposedFix must be empty on the indeterminate path, got %+v", got)
	}
}

func TestAgent_Diagnose_HappyPath(t *testing.T) {
	// Concentrated evidence plus a latency-shift signal (see
	// TestAgent_Diagnose_Presentation_Conclusion_WhenConcentratedAndLatencyShifted
	// for the rule engine's reasoning) - enough for the presentation rules
	// to reach a Conclusion, so this remains a true "happy path" through
	// the whole pipeline including Decide/Present, not just Render.
	latencyCaller := &recordingCaller{
		fakeToolCaller: &fakeToolCaller{},
		onCall: func(args map[string]any) mcp.ToolResult {
			if args["error"] == true {
				return scalarResult(5310000000)
			}
			return scalarResult(250000000)
		},
	}
	agent := &Agent{
		Gate:           &SourceEvidenceGate{Source: diagnosableGateSource()},
		EvidenceSource: fakeEvidenceSource{ev: concentratedErrorEvidence(5)},
		Reasoner:       StubReasoner{},
		SignalsCaller:  latencyCaller,
	}
	inc := Incident{
		CorrelationID: "corr-2",
		Service:       "checkout",
		Environment:   "prod",
		Window:        "1h",
		Grounding:     notify.Grounding{SLOState: "unhealthy", TelemetryTrusted: true, BurnRate: 5},
	}

	got, err := agent.Diagnose(context.Background(), inc)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if got.Status != notify.StatusDiagnosed {
		t.Errorf("Status = %v, want %v", got.Status, notify.StatusDiagnosed)
	}
	if got.Grounding != inc.Grounding {
		t.Errorf("Grounding = %+v, want %+v", got.Grounding, inc.Grounding)
	}
	if got.RootCause == "" {
		t.Errorf("expected a root cause: the rules found a dominant, corroborated cause")
	}
	if len(got.Evidence) != 5 {
		t.Errorf("expected 5 evidence items attached, got %d: %+v", len(got.Evidence), got.Evidence)
	}
	if len(got.MissingEvidence) != 0 {
		t.Errorf("MissingEvidence should be empty on the diagnosed path, got %v", got.MissingEvidence)
	}
}

// canonicalReasoner returns a fixed ModelDiagnosis, ignoring the incident
// and evidence it is given - used by the presentation wiring tests below,
// which only care about how Agent.Diagnose *shapes* the reasoner's output,
// not about reasoning itself.
type canonicalReasoner struct {
	md ModelDiagnosis
}

func (r canonicalReasoner) Reason(context.Context, Incident, []notify.Evidence) (ModelDiagnosis, error) {
	return r.md, nil
}

// fakeEvidenceSource returns a fixed evidence list, standing in for
// MCPEvidenceSource in the presentation wiring tests below.
type fakeEvidenceSource struct {
	ev []notify.Evidence
}

func (f fakeEvidenceSource) Gather(context.Context, Incident) ([]notify.Evidence, error) {
	return f.ev, nil
}

// concentratedErrorEvidence builds evidence shaped like the live
// support-agent demo: every error shares the same operation and a
// classifiable TimeoutError signature.
func concentratedErrorEvidence(n int) []notify.Evidence {
	ev := make([]notify.Evidence, 0, n)
	for i := 0; i < n; i++ {
		ev = append(ev, notify.Evidence{
			ID:   fmt.Sprintf("e%d", i+1),
			Kind: notify.EvidenceKindTrace,
			Note: "operation tool.search_kb, errored: tool.search_kb timed out after 5s, duration=5s",
		})
	}
	return ev
}

// diagnosableGateSource is a telemetry-source snapshot with just enough
// (one error trace) to make SourceEvidenceGate consider the incident
// worth reasoning about at all - the presentation-rule tests below use a
// separate, richer EvidenceSource for the evidence actually reasoned
// over, exactly as MCPEvidenceSource does against a live SigNoz gate.
func diagnosableGateSource() source.MemorySource {
	return source.MemorySource{Data: evidence.Snapshot{
		QueryComplete: true,
		Traces:        []evidence.Record{{Selector: "seed", Fields: map[string]any{"status_code": float64(500)}}},
	}}
}

func TestAgent_Diagnose_Presentation_Conclusion_WhenConcentratedAndLatencyShifted(t *testing.T) {
	md := ModelDiagnosis{RootCause: "tool.search_kb consistently times out", ProposedFix: "raise the timeout"}
	latencyCaller := &recordingCaller{
		fakeToolCaller: &fakeToolCaller{},
		onCall: func(args map[string]any) mcp.ToolResult {
			if args["error"] == true {
				return scalarResult(5310000000)
			}
			return scalarResult(250000000)
		},
	}
	agent := &Agent{
		Gate:           &SourceEvidenceGate{Source: diagnosableGateSource()},
		EvidenceSource: fakeEvidenceSource{ev: concentratedErrorEvidence(5)},
		Reasoner:       canonicalReasoner{md: md},
		SignalsCaller:  latencyCaller,
	}
	inc := Incident{CorrelationID: "corr-conclusion", Service: "support-agent", Environment: "local", Window: "1h", Grounding: notify.Grounding{TelemetryTrusted: true}}

	got, err := agent.Diagnose(context.Background(), inc)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}

	if got.Status != notify.StatusDiagnosed {
		t.Fatalf("Status = %v, want %v", got.Status, notify.StatusDiagnosed)
	}
	if got.RootCause != md.RootCause || got.ProposedFix != md.ProposedFix {
		t.Errorf("RootCause/ProposedFix = %q/%q, want the model's text (%q/%q)", got.RootCause, got.ProposedFix, md.RootCause, md.ProposedFix)
	}
	if got.Candidates != nil {
		t.Errorf("Candidates = %+v, want nil for a Conclusion", got.Candidates)
	}
	if !got.Reversible {
		t.Errorf("Reversible = false, want true (allowlisted fix text)")
	}
}

func TestAgent_Diagnose_Presentation_RankedHypotheses_WhenOnlyOneGoldenSignal(t *testing.T) {
	md := ModelDiagnosis{
		Candidates: []ModelCandidate{
			{RootCause: "tool.search_kb dependency is down", ProposedFix: "page the search team"},
			{RootCause: "network partition to the search cluster", ProposedFix: "check network policies"},
		},
	}
	agent := &Agent{
		Gate:           &SourceEvidenceGate{Source: diagnosableGateSource()},
		EvidenceSource: fakeEvidenceSource{ev: concentratedErrorEvidence(5)},
		Reasoner:       canonicalReasoner{md: md},
		// No SignalsCaller: only the concentration (errors) golden signal
		// is available, which is not enough for a Conclusion by default.
	}
	inc := Incident{CorrelationID: "corr-ranked", Service: "support-agent", Environment: "local", Window: "1h", Grounding: notify.Grounding{TelemetryTrusted: true}}

	got, err := agent.Diagnose(context.Background(), inc)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}

	if got.Status != notify.StatusDiagnosed {
		t.Fatalf("Status = %v, want %v", got.Status, notify.StatusDiagnosed)
	}
	if got.RootCause != "" || got.ProposedFix != "" {
		t.Errorf("RootCause/ProposedFix = %q/%q, want empty (Candidates set instead)", got.RootCause, got.ProposedFix)
	}
	if len(got.Candidates) != 2 {
		t.Fatalf("Candidates = %+v, want 2", got.Candidates)
	}
}

func TestAgent_Diagnose_Presentation_CouldNotDetermine_SuppressesModelRootCause(t *testing.T) {
	md := ModelDiagnosis{RootCause: "the model is confident it's tool.search_kb", ProposedFix: "raise the timeout"}
	agent := &Agent{
		Gate:           &SourceEvidenceGate{Source: diagnosableGateSource()},
		EvidenceSource: fakeEvidenceSource{ev: concentratedErrorEvidence(1)}, // below the default min of 3
		Reasoner:       canonicalReasoner{md: md},
	}
	inc := Incident{CorrelationID: "corr-cnd", Service: "support-agent", Environment: "local", Window: "1h", Grounding: notify.Grounding{TelemetryTrusted: true}}

	got, err := agent.Diagnose(context.Background(), inc)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}

	if got.Status != notify.StatusIndeterminate {
		t.Fatalf("Status = %v, want %v", got.Status, notify.StatusIndeterminate)
	}
	if got.RootCause != "" || got.ProposedFix != "" {
		t.Errorf("RootCause/ProposedFix = %q/%q, want empty - the model's asserted root cause must never surface here", got.RootCause, got.ProposedFix)
	}
	if len(got.MissingEvidence) == 0 {
		t.Errorf("expected MissingEvidence to explain the refusal")
	}
}

// TestAgent_Diagnose_RaisingMinErrorSamples_FlipsConclusionToCouldNotDetermine
// proves the presentation decision is driven by config, not by the
// reasoner: identical evidence and identical model output, and only
// Agent.Presentation.MinErrorSamples changes.
func TestAgent_Diagnose_RaisingMinErrorSamples_FlipsConclusionToCouldNotDetermine(t *testing.T) {
	md := ModelDiagnosis{RootCause: "tool.search_kb consistently times out", ProposedFix: "raise the timeout"}
	latencyCaller := &recordingCaller{
		fakeToolCaller: &fakeToolCaller{},
		onCall: func(args map[string]any) mcp.ToolResult {
			if args["error"] == true {
				return scalarResult(5310000000)
			}
			return scalarResult(250000000)
		},
	}
	newAgent := func(cfg PresentationConfig) *Agent {
		return &Agent{
			Gate:           &SourceEvidenceGate{Source: diagnosableGateSource()},
			EvidenceSource: fakeEvidenceSource{ev: concentratedErrorEvidence(5)},
			Reasoner:       canonicalReasoner{md: md},
			SignalsCaller:  latencyCaller,
			Presentation:   cfg,
		}
	}
	inc := Incident{CorrelationID: "corr-flip", Service: "support-agent", Environment: "local", Window: "1h", Grounding: notify.Grounding{TelemetryTrusted: true}}

	before, err := newAgent(DefaultPresentationConfig()).Diagnose(context.Background(), inc)
	if err != nil {
		t.Fatalf("Diagnose (default config): %v", err)
	}
	if before.Status != notify.StatusDiagnosed || before.RootCause == "" {
		t.Fatalf("with default thresholds, expected a Conclusion, got %+v", before)
	}

	raised := DefaultPresentationConfig()
	raised.MinErrorSamples = 1000
	after, err := newAgent(raised).Diagnose(context.Background(), inc)
	if err != nil {
		t.Fatalf("Diagnose (raised min_error_samples): %v", err)
	}
	if after.Status != notify.StatusIndeterminate {
		t.Fatalf("with min_error_samples=1000, expected CouldNotDetermine (indeterminate), got %+v", after)
	}
	if after.RootCause != "" {
		t.Errorf("RootCause = %q, want empty once the rules refuse to conclude", after.RootCause)
	}
}

func TestAgent_Diagnose_CapsEvidenceItems(t *testing.T) {
	var traces []evidence.Record
	for i := 0; i < 25; i++ {
		traces = append(traces, evidence.Record{Selector: "x", Fields: map[string]any{"status_code": float64(500)}})
	}
	agent := &Agent{
		Gate:     &SourceEvidenceGate{Source: source.MemorySource{Data: evidence.Snapshot{QueryComplete: true, Traces: traces}}},
		Reasoner: StubReasoner{},
	}

	got, err := agent.Diagnose(context.Background(), Incident{Service: "s", Window: "1h", Grounding: notify.Grounding{TelemetryTrusted: true}})
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if len(got.Evidence) != maxEvidenceItems {
		t.Errorf("Evidence count = %d, want capped at %d", len(got.Evidence), maxEvidenceItems)
	}
}
