package slo

import (
	"strings"
	"testing"
)

func TestBuildDashboardAndBurnRuleAreStable(t *testing.T) {
	dashboard := BuildDashboard()
	if dashboard["title"] != DashboardTitle || len(dashboard["widgets"].([]any)) != 8 {
		t.Fatalf("unexpected dashboard: %#v", dashboard)
	}
	rule := BuildBurnRateRule("checkout", BurnTier{Name: "fast", ShortWindow: "5m", Threshold: 14.4, Severity: "page"}, DefaultChannelName)
	if rule["alert"] != "SLO fast burn - checkout" || !strings.Contains(rule["description"].(string), "14.4") {
		t.Fatalf("unexpected burn rule: %#v", rule)
	}
	condition := rule["condition"].(map[string]any)
	composite := condition["compositeQuery"].(map[string]any)
	queries := composite["queries"].([]any)
	query := queries[0].(map[string]any)
	spec := query["spec"].(map[string]any)
	aggregations := spec["aggregations"].([]any)
	aggregation := aggregations[0].(map[string]any)
	if aggregation["metricName"] != "slo_mwmb_firing" {
		t.Fatalf("burn rule must consume emitted multi-window firing metric: %#v", aggregations)
	}
}
