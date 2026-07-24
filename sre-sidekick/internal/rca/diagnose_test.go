package rca

import (
	"context"
	"testing"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/evidence"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/source"
)

// failingReasoner fails the test if it is ever called. Used to prove the
// Agent does not invoke the reasoner on the indeterminate path.
type failingReasoner struct{ t *testing.T }

func (f failingReasoner) Reason(context.Context, Incident, []notify.Evidence) (ModelDiagnosis, error) {
	f.t.Fatal("Reasoner must not be called when the evidence gate is insufficient")
	return ModelDiagnosis{}, nil
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
	snapshot := evidence.Snapshot{
		AvailableSignals: map[string]bool{"traces": true, "logs": true},
		Traces: []evidence.Record{
			{Selector: "POST /checkout", Fields: map[string]any{"status_code": float64(500)}},
		},
		Logs: []evidence.Record{
			{Selector: "app.log", Fields: map[string]any{"message": "payment gateway timeout"}},
		},
	}
	agent := &Agent{
		Gate:     &SourceEvidenceGate{Source: source.MemorySource{Data: snapshot}},
		Reasoner: StubReasoner{},
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
		t.Errorf("expected a root cause from the stub reasoner")
	}
	if len(got.Evidence) != 2 {
		t.Errorf("expected 2 evidence items attached (1 trace + 1 log), got %d: %+v", len(got.Evidence), got.Evidence)
	}
	if len(got.MissingEvidence) != 0 {
		t.Errorf("MissingEvidence should be empty on the diagnosed path, got %v", got.MissingEvidence)
	}
}

func TestAgent_Diagnose_CapsEvidenceItems(t *testing.T) {
	var traces []evidence.Record
	for i := 0; i < 25; i++ {
		traces = append(traces, evidence.Record{Selector: "x", Fields: map[string]any{"status_code": float64(500)}})
	}
	agent := &Agent{
		Gate:     &SourceEvidenceGate{Source: source.MemorySource{Data: evidence.Snapshot{Traces: traces}}},
		Reasoner: StubReasoner{},
	}

	got, err := agent.Diagnose(context.Background(), Incident{Service: "s", Window: "1h"})
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if len(got.Evidence) != maxEvidenceItems {
		t.Errorf("Evidence count = %d, want capped at %d", len(got.Evidence), maxEvidenceItems)
	}
}
