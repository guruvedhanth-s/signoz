package rca

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify/slack"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/source"
)

const slackAdapterSLOConfig = `
version: "1"
service: support-agent
environment: local

slos:
  - name: grounded-answers
    type: ratio
    target: 0.9
    window: 1h
    good_metric: agent_success_total
    total_metric: agent_requests_total
`

func writeSLOConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "slo.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write SLO config: %v", err)
	}
	return path
}

// fakeMetrics is a minimal source.MetricQuerier, mirroring the one in
// internal/slo/engine_test.go, keyed by metric name.
type fakeMetrics struct {
	Values map[string]float64
}

func (f fakeMetrics) ScalarBuilder(_ context.Context, query source.MetricQuery, _, _ uint64) (float64, error) {
	return f.Values[query.Metric], nil
}

func TestSlackAdapter_Diagnose_GroundsAndDelegatesToTheAgent(t *testing.T) {
	sloPath := writeSLOConfig(t, slackAdapterSLOConfig)
	metrics := fakeMetrics{Values: map[string]float64{"agent_success_total": 95, "agent_requests_total": 100}}

	fake := notify.NewFake()
	adapter := &SlackAdapter{
		Agent: &Agent{
			Gate:           &SourceEvidenceGate{Source: diagnosableGateSource()},
			EvidenceSource: fakeEvidenceSource{ev: concentratedErrorEvidence(5)},
			Reasoner:       StubReasoner{},
			Notifier:       fake,
		},
		Metrics:       metrics,
		SLOConfigPath: sloPath,
		Now:           func() time.Time { return time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC) },
	}

	diagnosis, err := adapter.Diagnose(context.Background(), slack.DiagnoseRequest{
		Service: "support-agent", Environment: "local", RequestedBy: "U1",
	})
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}

	if diagnosis.Service != "support-agent" || diagnosis.Environment != "local" {
		t.Errorf("Diagnosis = %+v, want it scoped to the requested service/environment", diagnosis)
	}
	if diagnosis.Grounding.SLO != "grounded-answers" {
		t.Errorf("Grounding.SLO = %q, want the SLO evaluated from the config", diagnosis.Grounding.SLO)
	}
	if !diagnosis.Grounding.TelemetryTrusted {
		t.Errorf("Grounding.TelemetryTrusted = false, want the completeness gate to trust this window")
	}
	if _, ok := fake.LastDiagnosis(); !ok {
		t.Error("expected the Notifier to receive the diagnosis Diagnose produced")
	}
}

// Diagnose must never invent grounding for the wrong service - a mismatch
// between the incident and the configured SLO's service/environment must
// fall back to an ungrounded (TelemetryTrusted=false) result rather than
// silently attaching another service's facts (PRD section 7).
func TestSlackAdapter_Diagnose_MismatchedServiceSkipsGrounding(t *testing.T) {
	sloPath := writeSLOConfig(t, slackAdapterSLOConfig)
	metrics := fakeMetrics{Values: map[string]float64{"agent_success_total": 95, "agent_requests_total": 100}}

	adapter := &SlackAdapter{
		Agent: &Agent{
			Gate:     &SourceEvidenceGate{Source: diagnosableGateSource()},
			Reasoner: StubReasoner{},
			Notifier: notify.NewFake(),
		},
		Metrics:       metrics,
		SLOConfigPath: sloPath,
	}

	diagnosis, err := adapter.Diagnose(context.Background(), slack.DiagnoseRequest{
		Service: "checkout-api", Environment: "local", RequestedBy: "U1",
	})
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if diagnosis.Grounding.TelemetryTrusted {
		t.Error("Grounding.TelemetryTrusted = true for a service the SLO config does not cover, want false")
	}
	if diagnosis.Status != notify.StatusIndeterminate {
		t.Errorf("Status = %q, want indeterminate when grounding could not be established", diagnosis.Status)
	}
}

func TestSlackAdapter_AnswerFollowup_RefusesWithoutTheLiveReasoner(t *testing.T) {
	adapter := &SlackAdapter{
		Agent: &Agent{Reasoner: StubReasoner{}},
	}
	if _, err := adapter.AnswerFollowup(context.Background(), slack.FollowupRequest{}); err == nil {
		t.Fatal("AnswerFollowup() error = nil, want an error when the reasoner is not the live LLM reasoner")
	}
}

func TestSlackAdapter_AnswerFollowup_WhatChangedUsesGroundedDeployCorrelation(t *testing.T) {
	when := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	adapter := &SlackAdapter{Agent: &Agent{Reasoner: StubReasoner{}}}
	answer, err := adapter.AnswerFollowup(context.Background(), slack.FollowupRequest{
		Question:  "what changed?",
		Diagnosis: notify.Diagnosis{Grounding: notify.Grounding{RecentDeploy: &notify.DeployCorrelation{Candidates: []notify.DeployCandidate{{Version: "v1.4.2", At: when, Gap: 4 * time.Minute}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if answer != "v1.4.2, 4m0s before onset" {
		t.Fatalf("answer = %q, want the deterministic deploy correlation", answer)
	}
}

func TestSlackAdapter_Diagnose_DefaultsWindowTo1h(t *testing.T) {
	sloPath := writeSLOConfig(t, slackAdapterSLOConfig)
	metrics := fakeMetrics{Values: map[string]float64{"agent_success_total": 95, "agent_requests_total": 100}}

	adapter := &SlackAdapter{
		Agent: &Agent{
			Gate:     &SourceEvidenceGate{Source: diagnosableGateSource()},
			Reasoner: StubReasoner{},
			Notifier: notify.NewFake(),
		},
		Metrics:       metrics,
		SLOConfigPath: sloPath,
	}

	diagnosis, err := adapter.Diagnose(context.Background(), slack.DiagnoseRequest{Service: "support-agent", Environment: "local"})
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if diagnosis.Window != "1h" {
		t.Errorf("Window = %q, want the default 1h", diagnosis.Window)
	}
}
