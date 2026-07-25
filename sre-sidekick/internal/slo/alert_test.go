package slo

import (
	"strings"
	"testing"
)

// TestBuildBurnRateRule_AlertsOnMWMBFiring locks in the fix for the bug
// where the generated rule filtered slo_burn_rate by tier - a label that
// metric never carries (see EmitSLO in internal/otlp/emitter.go, which
// emits slo_burn_rate with only service/environment/slo/window). The rule
// must alert on the precomputed slo_mwmb_firing gauge instead, per PRD
// section 11.3.
func TestBuildBurnRateRule_AlertsOnMWMBFiring(t *testing.T) {
	tier := BurnTier{Name: "fast", LongWindow: "1h", ShortWindow: "5m", Threshold: 14.4, Severity: "page"}
	rule := BuildBurnRateRule("successful-agent-runs", tier, "sre-sidekick-default", "support-agent", "local")

	condition := rule["condition"].(map[string]any)
	if target := condition["target"]; target != 0 {
		t.Fatalf("target = %v, want 0 (the mwmb gauge is a 0/1 boolean, not the old fixed 0.5 burn-rate cutoff)", target)
	}
	if op := condition["op"]; op != "1" {
		t.Fatalf("op = %v, want \"1\" (ValueIsAbove)", op)
	}

	query := condition["compositeQuery"].(map[string]any)["queries"].([]any)[0].(map[string]any)
	spec := query["spec"].(map[string]any)
	aggregations := spec["aggregations"].([]any)[0].(map[string]any)
	if metric := aggregations["metricName"]; metric != "slo_mwmb_firing" {
		t.Fatalf("metricName = %v, want slo_mwmb_firing (slo_burn_rate has no tier label, so a rule filtered by tier against it never matches)", metric)
	}

	filter := spec["filter"].(map[string]any)["expression"].(string)
	for _, want := range []string{"slo = 'successful-agent-runs'", "tier = 'fast'", "service = 'support-agent'", "environment = 'local'"} {
		if !strings.Contains(filter, want) {
			t.Errorf("filter %q missing %q", filter, want)
		}
	}
}
