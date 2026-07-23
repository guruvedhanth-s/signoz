package slo

import "fmt"

// DefaultChannelName is the notification channel the agent ensures exists and
// attaches to generated burn-rate alerts.
const DefaultChannelName = "reliability-agent-default"

// BurnRuleName is the stable alert name for an SLO's burn-rate rule, used for
// idempotent creation.
func BurnRuleName(sloName string) string {
	return fmt.Sprintf("SLO fast burn - %s", sloName)
}

// BuildBurnRateRule assembles a metric threshold alert (not a formula, to avoid
// upstream formula-alert bugs) that fires when slo_burn_rate for the SLO exceeds
// the fast-burn threshold. The channel must already exist.
func BuildBurnRateRule(sloName string, threshold float64, channel string) map[string]any {
	target := threshold
	return map[string]any{
		"alert":       BurnRuleName(sloName),
		"alertType":   "METRIC_BASED_ALERT",
		"description":  fmt.Sprintf("Fast burn: slo_burn_rate for %q exceeded %gx.", sloName, threshold),
		"ruleType":    "threshold_rule",
		"evalWindow":  "5m",
		"frequency":   "1m",
		"version":     "v5",
		"labels":      map[string]any{"severity": "critical", "slo": sloName},
		"preferredChannels": []any{channel},
		"condition": map[string]any{
			"compositeQuery": map[string]any{
				"queryType": "builder",
				"queries": []any{
					map[string]any{
						"type": "builder_query",
						"spec": map[string]any{
							"name":   "A",
							"signal": "metrics",
							"aggregations": []any{
								map[string]any{"metricName": "slo_burn_rate", "spaceAggregation": "max"},
							},
							"filter":       map[string]any{"expression": fmt.Sprintf("slo = '%s'", sloName)},
							"stepInterval": "1m",
						},
					},
				},
			},
			"target":        &target,
			"matchType":     "1",
			"op":            "1",
			"selectedQuery": "A",
		},
	}
}
