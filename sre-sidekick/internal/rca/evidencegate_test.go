package rca

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/evidence"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/source"
)

func TestSourceEvidenceGate_Check(t *testing.T) {
	inc := Incident{Service: "checkout", Environment: "prod", Window: "1h"}

	tests := []struct {
		name           string
		snapshot       evidence.Snapshot
		wantSufficient bool
		wantErrorSpans bool
		wantExceptions bool
		wantLogs       bool
		reasonContains string
	}{
		{
			name: "error spans present",
			snapshot: evidence.Snapshot{
				AvailableSignals: map[string]bool{"traces": true},
				Traces: []evidence.Record{
					{Selector: "POST /checkout", Fields: map[string]any{"status_code": float64(500)}},
				},
			},
			wantSufficient: true,
			wantErrorSpans: true,
			reasonContains: "error spans",
		},
		{
			name:           "empty snapshot is insufficient",
			snapshot:       evidence.Snapshot{AvailableSignals: map[string]bool{"traces": true, "logs": true}},
			wantSufficient: false,
			reasonContains: "no error spans",
		},
		{
			name: "traces unavailable is reflected in reason",
			snapshot: evidence.Snapshot{
				AvailableSignals: map[string]bool{"traces": false},
			},
			wantSufficient: false,
			reasonContains: "traces",
		},
		{
			name: "logs alone are sufficient",
			snapshot: evidence.Snapshot{
				AvailableSignals: map[string]bool{"traces": true, "logs": true},
				Logs:             []evidence.Record{{Selector: "app.log", Fields: map[string]any{"message": "boom"}}},
			},
			wantSufficient: true,
			wantLogs:       true,
			reasonContains: "logs",
		},
		{
			name: "exception field is sufficient",
			snapshot: evidence.Snapshot{
				AvailableSignals: map[string]bool{"traces": true},
				Traces: []evidence.Record{
					{Selector: "checkout.charge", Fields: map[string]any{"exception.message": "nil pointer dereference"}},
				},
			},
			wantSufficient: true,
			wantExceptions: true,
			reasonContains: "exceptions",
		},
		{
			name: "ok status is not an error",
			snapshot: evidence.Snapshot{
				AvailableSignals: map[string]bool{"traces": true},
				Traces: []evidence.Record{
					{Selector: "GET /health", Fields: map[string]any{"status_code": float64(200)}},
				},
			},
			wantSufficient: false,
			reasonContains: "no error spans",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gate := &SourceEvidenceGate{
				Source: source.MemorySource{Data: tt.snapshot},
				Now:    func() time.Time { return time.Unix(0, 0) },
			}
			result, err := gate.Check(context.Background(), inc)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if result.Sufficient != tt.wantSufficient {
				t.Errorf("Sufficient = %v, want %v", result.Sufficient, tt.wantSufficient)
			}
			if result.HasErrorSpans != tt.wantErrorSpans {
				t.Errorf("HasErrorSpans = %v, want %v", result.HasErrorSpans, tt.wantErrorSpans)
			}
			if result.HasExceptions != tt.wantExceptions {
				t.Errorf("HasExceptions = %v, want %v", result.HasExceptions, tt.wantExceptions)
			}
			if result.HasLogs != tt.wantLogs {
				t.Errorf("HasLogs = %v, want %v", result.HasLogs, tt.wantLogs)
			}
			if result.Reason == "" {
				t.Errorf("Reason is empty, want a human-readable explanation")
			}
			if tt.reasonContains != "" && !strings.Contains(result.Reason, tt.reasonContains) {
				t.Errorf("Reason = %q, want it to contain %q", result.Reason, tt.reasonContains)
			}
		})
	}
}

func TestSourceEvidenceGate_CheckWithSnapshot_ReturnsFetchedSnapshot(t *testing.T) {
	snapshot := evidence.Snapshot{Logs: []evidence.Record{{Selector: "a"}}}
	gate := &SourceEvidenceGate{Source: source.MemorySource{Data: snapshot}}

	_, got, err := gate.CheckWithSnapshot(context.Background(), Incident{Service: "s", Window: "1h"})
	if err != nil {
		t.Fatalf("CheckWithSnapshot: %v", err)
	}
	if len(got.Logs) != 1 {
		t.Errorf("expected the fetched snapshot to be returned, got %+v", got)
	}
}

func TestSourceEvidenceGate_InvalidWindow(t *testing.T) {
	gate := &SourceEvidenceGate{Source: source.MemorySource{}}

	_, err := gate.Check(context.Background(), Incident{Service: "s", Window: "not-a-duration"})
	if err == nil {
		t.Fatal("expected an error for an invalid window")
	}
}

func TestSourceEvidenceGate_NoSource(t *testing.T) {
	gate := &SourceEvidenceGate{}

	_, err := gate.Check(context.Background(), Incident{Service: "s", Window: "1h"})
	if err == nil {
		t.Fatal("expected an error when no telemetry source is configured")
	}
}
