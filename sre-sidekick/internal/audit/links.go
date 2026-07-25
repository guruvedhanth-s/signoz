package audit

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// The types below mirror pkg/contextlinks.URLShareableCompositeQuery and its
// nested shapes in the SigNoz backend/frontend (verified against the real
// link-building code in pkg/query-service/rules/threshold_rule.go's
// prepareParamsForLogs/prepareParamsForTraces and
// pkg/contextlinks/links.go's PrepareParamsForLogsV5/PrepareParamsForTracesV5
// - the same code SigNoz's own alert rules use to attach "related logs"/
// "related traces" links to a firing alert). Reimplemented here rather than
// imported: sre-sidekick is a separate Go module, and importing the whole
// SigNoz backend as a dependency just for this URL shape would be a much
// bigger dependency than the three small structs it actually needs.
type deepLinkTimeRange struct {
	Start    int64 `json:"start"`
	End      int64 `json:"end"`
	PageSize int64 `json:"pageSize"`
}

type deepLinkFilter struct {
	Expression string `json:"expression,omitempty"`
}

type deepLinkAttributeKey struct {
	Key      string `json:"key"`
	DataType string `json:"dataType"`
	Type     string `json:"type"`
	IsColumn bool   `json:"isColumn"`
	IsJSON   bool   `json:"isJSON"`
}

type deepLinkBuilderQuery struct {
	QueryName          string               `json:"queryName"`
	StepInterval       int64                `json:"stepInterval"`
	DataSource         string               `json:"dataSource"`
	AggregateOperator  string               `json:"aggregateOperator"`
	AggregateAttribute deepLinkAttributeKey `json:"aggregateAttribute,omitempty"`
	Expression         string               `json:"expression"`
	Disabled           bool                 `json:"disabled"`
	Having             []struct{}           `json:"having,omitempty"`
	Filter             *deepLinkFilter      `json:"filter,omitempty"`
}

type deepLinkBuilder struct {
	QueryData     []deepLinkBuilderQuery `json:"queryData"`
	QueryFormulas []string               `json:"queryFormulas"`
}

type deepLinkCompositeQuery struct {
	QueryType string          `json:"queryType"`
	Builder   deepLinkBuilder `json:"builder"`
}

// BuildDeepLink returns a SigNoz logs/traces explorer URL scoped to
// filterExpression and the [start,end) window, or "" if signal is neither
// "logs" nor "traces" (there is no explorer view for metrics findings
// today). signozBaseURL should be the externally-reachable SigNoz URL -
// the same host RCA evidence links are rewritten to (see
// rca.RewriteLinkHost) - not an in-cluster service name a human's browser
// cannot resolve.
func BuildDeepLink(signozBaseURL, signal, filterExpression string, start, end time.Time) string {
	var dataSource, path string
	switch signal {
	case "logs":
		dataSource, path = "logs", "logs/logs-explorer"
	case "traces":
		dataSource, path = "traces", "traces-explorer"
	default:
		return ""
	}
	base := strings.TrimRight(signozBaseURL, "/")
	if base == "" {
		return ""
	}

	// Traces list view expects nanoseconds, logs milliseconds - matches
	// PrepareParamsForTracesV5/LogsV5 exactly.
	var tr deepLinkTimeRange
	if signal == "traces" {
		tr = deepLinkTimeRange{Start: start.UnixNano(), End: end.UnixNano(), PageSize: 100}
	} else {
		tr = deepLinkTimeRange{Start: start.UnixMilli(), End: end.UnixMilli(), PageSize: 100}
	}

	query := deepLinkCompositeQuery{
		QueryType: "builder",
		Builder: deepLinkBuilder{
			QueryData: []deepLinkBuilderQuery{{
				QueryName:         "A",
				StepInterval:      60,
				DataSource:        dataSource,
				AggregateOperator: "noop",
				Expression:        "A",
				Having:            []struct{}{},
				Filter:            &deepLinkFilter{Expression: filterExpression},
			}},
			QueryFormulas: []string{},
		},
	}

	compositeQueryJSON, err := json.Marshal(query)
	if err != nil {
		return ""
	}
	timeRangeJSON, err := json.Marshal(tr)
	if err != nil {
		return ""
	}

	// A single url.Values.Encode() pass, matching PrepareParamsForTracesV5/
	// LogsV5 + BaseRule.ExternalURL exactly (RawQuery = params.Encode()) -
	// not the double-percent-encoding the older PrepareLinksToTraces/Logs
	// helpers use, which nothing in this codebase's target version calls.
	params := url.Values{}
	params.Set("compositeQuery", string(compositeQueryJSON))
	params.Set("timeRange", string(timeRangeJSON))
	params.Set("startTime", fmt.Sprintf("%d", tr.Start))
	params.Set("endTime", fmt.Sprintf("%d", tr.End))
	params.Set("options", "{}")

	return base + "/" + path + "?" + params.Encode()
}

// ScopeFilter builds the deterministic service/environment filter
// expression every deep link is scoped to, using the same attribute keys
// internal/source/signoz's own builder queries filter on (see
// telemetry.go's querySignal).
func ScopeFilter(service, environment string) string {
	filter := fmt.Sprintf("resource.service.name = '%s'", escapeFilterValue(service))
	if strings.TrimSpace(environment) != "" {
		filter += fmt.Sprintf(" AND resource.deployment.environment = '%s'", escapeFilterValue(environment))
	}
	return filter
}

func escapeFilterValue(value string) string {
	return strings.ReplaceAll(value, "'", "\\'")
}
