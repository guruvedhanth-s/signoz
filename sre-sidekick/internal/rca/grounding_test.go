package rca

import (
	"testing"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/slo"
)

func TestGroundingFromReport_CopiesFieldsVerbatim(t *testing.T) {
	report := slo.Report{
		Name:                 "grounded-answers",
		State:                slo.StateUnhealthy,
		BurnRate:             4.2,
		ErrorBudgetRemaining: -0.15,
		Gate:                 slo.GateResult{Trusted: true},
	}

	got := GroundingFromReport("local", report)

	want := notify.Grounding{
		Environment:          "local",
		SLO:                  "grounded-answers",
		SLOState:             "unhealthy",
		BurnRate:             4.2,
		ErrorBudgetRemaining: -0.15,
		TelemetryTrusted:     true,
	}
	if got != want {
		t.Errorf("GroundingFromReport = %+v, want %+v", got, want)
	}
}

func TestGroundingFromReport_HealthyState(t *testing.T) {
	report := slo.Report{
		Name:                 "grounded-answers",
		State:                slo.StateHealthy,
		BurnRate:             0.2,
		ErrorBudgetRemaining: 0.9,
		Gate:                 slo.GateResult{Trusted: true},
	}
	got := GroundingFromReport("prod", report)
	if got.SLOState != "healthy" || !got.TelemetryTrusted {
		t.Errorf("GroundingFromReport = %+v, want healthy/trusted", got)
	}
}

func TestGroundingFromReport_IndeterminateMeansTelemetryNotTrusted(t *testing.T) {
	// Mirrors what Engine.evaluate actually does on the indeterminate path
	// (internal/slo/engine.go's indeterminate helper): State is
	// StateIndeterminate and Gate.Trusted is forced to false.
	report := slo.Report{
		Name:  "grounded-answers",
		State: slo.StateIndeterminate,
		Gate:  slo.GateResult{Trusted: false},
		Error: "telemetry completeness is below the SLO gate",
	}

	got := GroundingFromReport("local", report)

	if got.SLOState != "indeterminate" {
		t.Errorf("SLOState = %q, want %q", got.SLOState, "indeterminate")
	}
	if got.TelemetryTrusted {
		t.Errorf("TelemetryTrusted = true, want false for an indeterminate report")
	}
}

func TestSelectReport_MatchesByName(t *testing.T) {
	reports := []slo.Report{
		{Name: "a"},
		{Name: "b"},
	}
	got, ok := SelectReport(reports, "b")
	if !ok || got.Name != "b" {
		t.Errorf("SelectReport(reports, %q) = %+v, %v, want name %q", "b", got, ok, "b")
	}
}

func TestSelectReport_FallsBackToFirstWhenNameEmptyOrUnmatched(t *testing.T) {
	reports := []slo.Report{
		{Name: "a"},
		{Name: "b"},
	}
	if got, ok := SelectReport(reports, ""); !ok || got.Name != "a" {
		t.Errorf("SelectReport(reports, \"\") = %+v, %v, want first report %q", got, ok, "a")
	}
	if got, ok := SelectReport(reports, "missing"); !ok || got.Name != "a" {
		t.Errorf("SelectReport(reports, %q) = %+v, %v, want fallback to first report %q", "missing", got, ok, "a")
	}
}

func TestSelectReport_EmptyReportsReturnsFalse(t *testing.T) {
	if _, ok := SelectReport(nil, "anything"); ok {
		t.Errorf("SelectReport(nil, ...) ok = true, want false")
	}
}
