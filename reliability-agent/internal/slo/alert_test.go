package slo

import "testing"

func TestBuildBurnRateRule(t *testing.T) {
	r := BuildBurnRateRule("successful-agent-runs", 14.4, DefaultChannelName)

	if r["alert"] != BurnRuleName("successful-agent-runs") {
		t.Fatalf("alert = %v", r["alert"])
	}
	if r["ruleType"] != "threshold_rule" {
		t.Fatalf("ruleType = %v, want threshold_rule", r["ruleType"])
	}
	chans, ok := r["preferredChannels"].([]any)
	if !ok || len(chans) != 1 || chans[0] != DefaultChannelName {
		t.Fatalf("preferredChannels = %v", r["preferredChannels"])
	}

	cond := r["condition"].(map[string]any)
	target := cond["target"].(*float64)
	if *target != 14.4 {
		t.Fatalf("target = %v, want 14.4", *target)
	}
	cq := cond["compositeQuery"].(map[string]any)
	spec := cq["queries"].([]any)[0].(map[string]any)["spec"].(map[string]any)
	agg := spec["aggregations"].([]any)[0].(map[string]any)
	if agg["metricName"] != "slo_burn_rate" {
		t.Fatalf("metricName = %v, want slo_burn_rate", agg["metricName"])
	}
}

func TestBurnRuleNameStable(t *testing.T) {
	if BurnRuleName("x") != BurnRuleName("x") {
		t.Fatal("BurnRuleName must be stable for idempotency")
	}
}
