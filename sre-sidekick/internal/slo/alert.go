package slo

import "fmt"

const DefaultChannelName = "sre-sidekick-default"

func BurnRuleName(sloName, tier string) string {
	return fmt.Sprintf("SLO %s burn - %s", tier, sloName)
}

// BuildBurnRateRule builds a threshold alert on the precomputed
// slo_mwmb_firing gauge (see PRD section 11.3): the sidekick itself
// evaluates both the long and short burn-rate windows and emits a single
// 0/1 gauge, so the generated rule only needs "slo_mwmb_firing > 0" -
// never a PromQL AND across two burn-rate series, which does not match
// because those series carry different "window" labels.
//
// This intentionally does not filter or alert on the raw slo_burn_rate
// metric: that series carries no "tier" label (see EmitSLO), so a filter
// on tier against slo_burn_rate matches no series and the rule would
// never fire.
func BuildBurnRateRule(sloName string, tier BurnTier, channel string, scope ...string) map[string]any {
	filter := fmt.Sprintf("slo = '%s' AND tier = '%s'", sloName, tier.Name)
	if len(scope) >= 2 {
		filter += fmt.Sprintf(" AND service = '%s' AND environment = '%s'", scope[0], scope[1])
	}
	return map[string]any{
		"alert": BurnRuleName(sloName, tier.Name), "alertType": "METRIC_BASED_ALERT",
		"description": fmt.Sprintf("%s burn for SLO %q exceeded %gx (both the long and short burn-rate windows).", tier.Name, sloName, tier.Threshold),
		"ruleType":    "threshold_rule", "evalWindow": tier.ShortWindow, "frequency": "1m", "version": "v5",
		"labels":            map[string]any{"severity": tier.Severity, "slo": sloName, "tier": tier.Name},
		"preferredChannels": []any{channel},
		"condition": map[string]any{
			"compositeQuery": map[string]any{"queryType": "builder", "queries": []any{map[string]any{
				"type": "builder_query", "spec": map[string]any{
					"name": "A", "signal": "metrics", "aggregations": []any{map[string]any{"metricName": "slo_mwmb_firing", "spaceAggregation": "max"}},
					"filter": map[string]any{"expression": filter}, "stepInterval": "1m",
				},
			}}},
			"target": 0, "matchType": "1", "op": "1", "selectedQuery": "A",
		},
	}
}
