package signoz

import (
	"context"
	"fmt"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/evidence"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/profile"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/source"
)

// TelemetrySource queries every signal referenced by a profile. It keeps
// unsupported signals out of reports while making traces and metrics available
// to live audit-watch runs when SigNoz supports those raw query signals.
type TelemetrySource struct {
	Client *Client
	Limit  int
}

func NewTelemetrySource(client *Client, limit int) *TelemetrySource {
	if limit <= 0 {
		limit = 200
	}
	return &TelemetrySource{Client: client, Limit: limit}
}

func (s *TelemetrySource) Snapshot(ctx context.Context, p profile.Profile, target source.Target) (evidence.Snapshot, error) {
	if s.Client == nil {
		return evidence.Snapshot{}, fmt.Errorf("SigNoz client is required")
	}
	snapshot := evidence.Snapshot{
		QueryComplete:    true,
		AvailableSignals: map[string]bool{},
		Logs:             []evidence.Record{},
		Metrics:          []evidence.Record{},
		Traces:           []evidence.Record{},
		LastSeen:         map[string]time.Time{},
		DistinctValues:   map[string]int{},
	}
	for _, signal := range []string{"logs", "traces", "metrics"} {
		if !profileUsesSignal(p, signal) {
			continue
		}
		// Metrics do not go through querySignal: the v5 API has no "raw"
		// request type for them and rejects the count() expression the
		// record-shaped signals use, because a metrics query is an
		// aggregation over a named series rather than a row scan. See
		// queryMetrics.
		if signal == "metrics" {
			part, err := s.queryMetrics(ctx, p, target)
			if err != nil {
				snapshot.QueryComplete = false
				snapshot.Partial = true
				continue
			}
			mergeSignalPart(&snapshot, part, signal)
			continue
		}
		response, err := s.querySignal(ctx, signal, target)
		if err != nil {
			snapshot.QueryComplete = false
			snapshot.Partial = true
			continue
		}
		part := normalizeSignal(response, signal, s.Limit)
		mergeSignalPart(&snapshot, part, signal)
	}
	return snapshot, nil
}

// mergeSignalPart folds one signal's snapshot into the combined one. Kept in
// one place so the metrics path and the record-shaped paths cannot drift in
// how they contribute completeness, freshness, and cardinality.
func mergeSignalPart(snapshot *evidence.Snapshot, part evidence.Snapshot, signal string) {
	snapshot.AvailableSignals[signal] = true
	snapshot.QueryComplete = snapshot.QueryComplete && part.QueryComplete
	snapshot.Partial = snapshot.Partial || part.Partial
	snapshot.Logs = append(snapshot.Logs, part.Logs...)
	snapshot.Traces = append(snapshot.Traces, part.Traces...)
	snapshot.Metrics = append(snapshot.Metrics, part.Metrics...)
	for key, value := range part.LastSeen {
		if value.After(snapshot.LastSeen[key]) {
			snapshot.LastSeen[key] = value
		}
	}
	// Summed, not maxed, because that is the pre-existing behaviour for the
	// record-shaped signals and this refactor is not the place to redefine
	// it. Note it is only correct while a given field name appears in one
	// signal: a name carried by both logs and traces double-counts.
	for key, value := range part.DistinctValues {
		snapshot.DistinctValues[key] += value
	}
}

func (s *TelemetrySource) querySignal(ctx context.Context, signal string, target source.Target) (rawLogsResponse, error) {
	filter := fmt.Sprintf("resource.service.name = '%s'", escapeFilter(target.Service))
	if target.Environment != "" {
		filter += fmt.Sprintf(" AND resource.deployment.environment = '%s'", escapeFilter(target.Environment))
	}
	// Newest first, with "id" as a tiebreaker so records sharing a
	// timestamp come back in a stable order. "id" exists on the logs
	// signal only: SigNoz's v5 API rejects the whole query with
	// "field `id` not found" when it is sent for traces, which used to
	// fail the traces query on every call and, through
	// Snapshot.QueryComplete, made the RCA evidence gate return
	// indeterminate for every incident. Confirmed live against SigNoz.
	order := []any{map[string]any{"key": map[string]any{"name": "timestamp"}, "direction": "desc"}}
	if signal == "logs" {
		order = append(order, map[string]any{"key": map[string]any{"name": "id"}, "direction": "desc"})
	}
	request := map[string]any{
		"schemaVersion": "v1", "start": target.Start.UnixMilli(), "end": target.End.UnixMilli(),
		"requestType": "raw", "noCache": true,
		"compositeQuery": map[string]any{"queries": []any{map[string]any{
			"type": "builder_query",
			"spec": map[string]any{
				"name": "A", "signal": signal, "disabled": false, "limit": s.Limit, "offset": 0,
				"filter":       map[string]any{"expression": filter},
				"order":        order,
				"aggregations": []any{map[string]any{"expression": "count()"}},
			},
		}}},
		"formatOptions": map[string]any{"formatTableResultForUI": false, "fillGaps": false},
	}
	var response rawLogsResponse
	if err := s.Client.QueryRange(ctx, request, &response); err != nil {
		return rawLogsResponse{}, err
	}
	if response.Status != "success" {
		return rawLogsResponse{}, fmt.Errorf("SigNoz %s query returned status %q", signal, response.Status)
	}
	return response, nil
}

func normalizeSignal(response rawLogsResponse, signal string, limit int) evidence.Snapshot {
	snapshot := evidence.Snapshot{
		QueryComplete: response.Status == "success", Logs: []evidence.Record{},
		Traces: []evidence.Record{}, Metrics: []evidence.Record{},
		LastSeen: map[string]time.Time{}, DistinctValues: map[string]int{},
	}
	distinct := map[string]map[string]struct{}{}
	rows := 0
	for _, result := range response.Data.Data.Results {
		for _, row := range result.Rows {
			rows++
			fields := flattenLogData(row.Data)
			fields["timestamp"] = row.Timestamp
			selector := signal
			for _, key := range []string{"name", "span_name", "metric_name", "operation_name"} {
				if value, ok := fields[key].(string); ok && value != "" {
					selector = value
					break
				}
			}
			record := evidence.Record{Selector: selector, Fields: fields}
			switch signal {
			case "logs":
				snapshot.Logs = append(snapshot.Logs, record)
			case "traces":
				snapshot.Traces = append(snapshot.Traces, record)
			case "metrics":
				snapshot.Metrics = append(snapshot.Metrics, record)
			}
			for key, value := range fields {
				if isEmpty(value) {
					continue
				}
				if row.Timestamp.After(snapshot.LastSeen[key]) {
					snapshot.LastSeen[key] = row.Timestamp
				}
				if isScalar(value) {
					if distinct[key] == nil {
						distinct[key] = map[string]struct{}{}
					}
					distinct[key][fmt.Sprintf("%T:%v", value, value)] = struct{}{}
				}
			}
		}
	}
	// See the same guard in logs.go: a capped sample is Partial, not an
	// incomplete query. Snapshot.Complete() still reports false, so Track A
	// is unaffected; the RCA evidence gate can now tell "we only saw the
	// first N records" apart from "the query did not run".
	if limit > 0 && rows >= limit {
		snapshot.Partial = true
	}
	for key, values := range distinct {
		snapshot.DistinctValues[key] = len(values)
	}
	return snapshot
}

func profileUsesSignal(p profile.Profile, signal string) bool {
	for _, rule := range p.Spec.AuditRules {
		if rule.Signal == signal {
			return true
		}
	}
	switch signal {
	case "logs":
		return len(p.Spec.Signals.Logs.Fields) > 0 || len(p.Spec.Signals.Logs.Spans) > 0
	case "traces":
		return p.Spec.Signals.Traces.RootSpan != "" || len(p.Spec.Signals.Traces.Fields) > 0 || len(p.Spec.Signals.Traces.Spans) > 0
	case "metrics":
		return len(p.Spec.Signals.Metrics.Fields) > 0 || len(p.Spec.Signals.Metrics.Spans) > 0
	default:
		return false
	}
}

// rawMetricsResponse is the v5 time_series response: one aggregation per
// query, each carrying labelled series of timestamped values.
type rawMetricsResponse struct {
	Status string `json:"status"`
	Data   struct {
		Data struct {
			Results []struct {
				Aggregations []struct {
					Series []struct {
						Labels []struct {
							Key struct {
								Name string `json:"name"`
							} `json:"key"`
							Value any `json:"value"`
						} `json:"labels"`
						Values []struct {
							Timestamp int64   `json:"timestamp"`
							Value     float64 `json:"value"`
						} `json:"values"`
					} `json:"series"`
				} `json:"aggregations"`
			} `json:"results"`
		} `json:"data"`
	} `json:"data"`
}

// queryMetrics gathers the metrics signal, one query per metric the profile
// declares.
//
// Metrics cannot go through querySignal. That builds a requestType "raw"
// row scan with an {"expression": "count()"} aggregation, which the v5 API
// rejects for this signal: a metrics query aggregates a *named* series, so it
// wants requestType "time_series" and a MetricAggregation carrying
// metricName, timeAggregation, and spaceAggregation. Sending the record shape
// produced `unknown field "expression" in query spec for MetricAggregation`
// on every call, which cleared Snapshot.QueryComplete and made any profile
// declaring a metrics signal (examples/checkout-api.yaml) undiagnosable.
//
// There is no way to ask for "every metric this service emits" in one query,
// which is why this iterates the profile's declared metric names - those are
// the only metrics an audit has an opinion about anyway.
//
// A metric that does not exist is not an error: SigNoz answers success with
// zero series, so it lands as a missing LastSeen entry and the freshness rule
// reports "no last-seen timestamp" on its own terms. Confirmed live.
func (s *TelemetrySource) queryMetrics(ctx context.Context, p profile.Profile, target source.Target) (evidence.Snapshot, error) {
	snapshot := evidence.Snapshot{
		QueryComplete:    true,
		AvailableSignals: map[string]bool{"metrics": true},
		Metrics:          []evidence.Record{},
		LastSeen:         map[string]time.Time{},
		DistinctValues:   map[string]int{},
	}
	groupBy := metricsGroupByKeys(p)
	distinct := map[string]map[string]struct{}{}

	var failures int
	for _, field := range metricsToQuery(p) {
		if field.Path == "" {
			continue
		}
		response, err := s.queryMetric(ctx, field, groupBy, target)
		if err != nil {
			// One unavailable metric must not discard the ones that did
			// answer, but the snapshot has to admit it is incomplete.
			failures++
			snapshot.Partial = true
			continue
		}
		collectMetricSeries(&snapshot, response, field.Path, distinct)
	}
	if failures > 0 {
		snapshot.QueryComplete = false
	}
	for key, values := range distinct {
		snapshot.DistinctValues[key] = len(values)
	}
	return snapshot, nil
}

// metricsToQuery is every metric this profile gives the audit an opinion
// about. Usually that is the declared metrics fields, but a profile may also
// name a metric only in a rule - profileUsesSignal already treats a rule with
// signal "metrics" as reason enough to query the signal, so taking the fields
// alone would query nothing at all and leave the rule permanently reporting
// "no last-seen timestamp".
//
// Only freshness and required_field rules name a metric. A cardinality rule's
// field is a *label* on some metric, which is what metricsGroupByKeys handles;
// querying it as a metric name would find nothing.
func metricsToQuery(p profile.Profile) []profile.FieldSpec {
	fields := make([]profile.FieldSpec, 0, len(p.Spec.Signals.Metrics.Fields))
	seen := map[string]struct{}{}
	for _, field := range p.Spec.Signals.Metrics.Fields {
		if field.Path == "" {
			continue
		}
		if _, ok := seen[field.Path]; ok {
			continue
		}
		seen[field.Path] = struct{}{}
		fields = append(fields, field)
	}
	for _, rule := range p.Spec.AuditRules {
		if rule.Signal != "metrics" || rule.Field == "" {
			continue
		}
		if rule.Type != "freshness" && rule.Type != "required_field" {
			continue
		}
		if _, ok := seen[rule.Field]; ok {
			continue
		}
		seen[rule.Field] = struct{}{}
		// No declared type, so metricAggregations falls back to avg/avg,
		// which is valid for a gauge and sufficient for presence and
		// freshness. Declare the metric under signals.metrics.fields with a
		// type to get the counter or histogram treatment.
		fields = append(fields, profile.FieldSpec{Path: rule.Field})
	}
	return fields
}

// metricsGroupByKeys is the set of labels the query must break series down
// by. Cardinality rules are the reason this exists: counting distinct values
// of an attribute is only possible if the series come back split by it.
func metricsGroupByKeys(p profile.Profile) []string {
	seen := map[string]struct{}{}
	var keys []string
	for _, rule := range p.Spec.AuditRules {
		if rule.Signal != "metrics" || rule.Type != "cardinality" || rule.Field == "" {
			continue
		}
		if _, ok := seen[rule.Field]; ok {
			continue
		}
		seen[rule.Field] = struct{}{}
		keys = append(keys, rule.Field)
	}
	return keys
}

// metricScopeLabels are the label spellings a metric may carry for its
// service and environment, most specific first.
//
// There is no single correct spelling. A counter from an OTel SDK resolves
// under the resource.* keys, while signoz_latency and its siblings - derived
// by the spanmetrics processor from trace resource attributes - carry the bare
// semantic-convention keys, and a hand-written counter may use underscored
// custom labels. Sending a key a metric does not have is not an empty result:
// SigNoz rejects the query with "Found 2 errors while parsing the search
// expression", which is why one fixed spelling made the histogram in
// examples/checkout-api.yaml unqueryable. The same problem is why
// examples/support-agent-slo.yaml has service_label/environment_label.
//
// queryMetric tries these in order and keeps the first that both parses and
// returns data, so results are always scoped to the requested service - it
// never falls back to an unfiltered query.
var metricScopeLabels = []struct{ Service, Environment string }{
	{"resource.service.name", "resource.deployment.environment"},
	{"service.name", "deployment.environment"},
	{"service_name", "environment"},
}

func (s *TelemetrySource) queryMetric(
	ctx context.Context,
	field profile.FieldSpec,
	groupBy []string,
	target source.Target,
) (rawMetricsResponse, error) {
	var firstParsed *rawMetricsResponse
	var lastErr error
	for _, labels := range metricScopeLabels {
		filter := fmt.Sprintf("%s = '%s'", labels.Service, escapeFilter(target.Service))
		if target.Environment != "" {
			filter += fmt.Sprintf(" AND %s = '%s'", labels.Environment, escapeFilter(target.Environment))
		}
		response, err := s.queryMetricWithFilter(ctx, field, groupBy, target, filter)
		if err != nil {
			lastErr = err
			continue
		}
		if metricSeriesCount(response) > 0 {
			return response, nil
		}
		if firstParsed == nil {
			parsed := response
			firstParsed = &parsed
		}
	}
	// Every spelling parsed but none matched: the metric genuinely has no
	// data for this window, which freshness reports on its own terms.
	if firstParsed != nil {
		return *firstParsed, nil
	}
	return rawMetricsResponse{}, lastErr
}

func metricSeriesCount(response rawMetricsResponse) int {
	count := 0
	for _, result := range response.Data.Data.Results {
		for _, aggregation := range result.Aggregations {
			count += len(aggregation.Series)
		}
	}
	return count
}

func (s *TelemetrySource) queryMetricWithFilter(
	ctx context.Context,
	field profile.FieldSpec,
	groupBy []string,
	target source.Target,
	filter string,
) (rawMetricsResponse, error) {
	timeAggregation, spaceAggregation := metricAggregations(field.Type)
	group := make([]any, 0, len(groupBy))
	for _, key := range groupBy {
		group = append(group, map[string]any{"name": key})
	}
	request := map[string]any{
		"schemaVersion": "v1", "start": target.Start.UnixMilli(), "end": target.End.UnixMilli(),
		"requestType": "time_series",
		"compositeQuery": map[string]any{"queries": []any{map[string]any{
			"type": "builder_query",
			"spec": map[string]any{
				"name": "A", "signal": "metrics", "stepInterval": "1m",
				"aggregations": []any{map[string]any{
					"metricName":      metricSeriesName(field),
					"timeAggregation": timeAggregation, "spaceAggregation": spaceAggregation,
				}},
				"filter":  map[string]any{"expression": filter},
				"groupBy": group,
			},
		}}},
	}
	var response rawMetricsResponse
	if err := s.Client.QueryRange(ctx, request, &response); err != nil {
		return rawMetricsResponse{}, err
	}
	if response.Status != "success" {
		return rawMetricsResponse{}, fmt.Errorf("SigNoz metrics query returned status %q", response.Status)
	}
	return response, nil
}

// metricSeriesName maps a declared metric to the series that actually holds
// data. A histogram is stored as child series (.bucket, .count, .sum) and its
// base name returns zero series, so freshness has to read the .count child.
// Confirmed live against signoz_latency, whose base name is empty while
// signoz_latency.count has data.
func metricSeriesName(field profile.FieldSpec) string {
	if field.Type == "histogram" {
		return field.Path + ".count"
	}
	return field.Path
}

// metricAggregations picks aggregations valid for the metric's type. SigNoz
// rejects "sum" as a time aggregation on a monotonic counter ("valid values:
// rate, increase"), so the counter and histogram-count cases use rate.
func metricAggregations(metricType string) (timeAggregation, spaceAggregation string) {
	switch metricType {
	case "counter", "histogram":
		return "rate", "sum"
	default:
		// Gauges, and anything a profile leaves untyped: an average over
		// time and space is meaningful for a level and harmless for
		// presence and freshness, which is all the audit rules read.
		return "avg", "avg"
	}
}

// collectMetricSeries folds one metric's series into the snapshot: one record
// per series carrying its labels and latest value, the newest sample as the
// metric's LastSeen, and every observed label value toward cardinality.
func collectMetricSeries(
	snapshot *evidence.Snapshot,
	response rawMetricsResponse,
	metricName string,
	distinct map[string]map[string]struct{},
) {
	for _, result := range response.Data.Data.Results {
		for _, aggregation := range result.Aggregations {
			for _, series := range aggregation.Series {
				fields := map[string]any{"metric": metricName}
				for _, label := range series.Labels {
					if label.Key.Name == "" || isEmpty(label.Value) {
						continue
					}
					fields[label.Key.Name] = label.Value
					if distinct[label.Key.Name] == nil {
						distinct[label.Key.Name] = map[string]struct{}{}
					}
					distinct[label.Key.Name][fmt.Sprintf("%v", label.Value)] = struct{}{}
				}
				var newest time.Time
				for _, value := range series.Values {
					at := time.UnixMilli(value.Timestamp).UTC()
					if at.After(newest) {
						newest = at
						fields["value"] = value.Value
					}
				}
				if newest.IsZero() {
					continue
				}
				fields["timestamp"] = newest
				snapshot.Metrics = append(snapshot.Metrics, evidence.Record{Selector: "metric", Fields: fields})
				if newest.After(snapshot.LastSeen[metricName]) {
					snapshot.LastSeen[metricName] = newest
				}
			}
		}
	}
}
