package slo

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/source"
)

var ErrNoData = errors.New("no data in SLO window")

// scalarWithWarning is ScalarBuilder plus, when querier also implements
// source.WarningQuerier, SigNoz's own top-level query-completeness
// warning for the same read (PRD section 11.2). A querier that doesn't
// implement it (an in-memory or test double) behaves exactly as before -
// this is purely additive.
func scalarWithWarning(ctx context.Context, querier source.MetricQuerier, query source.MetricQuery, start, end uint64) (float64, string, error) {
	if warned, ok := querier.(source.WarningQuerier); ok {
		return warned.ScalarBuilderWarning(ctx, query, start, end)
	}
	value, err := querier.ScalarBuilder(ctx, query, start, end)
	return value, "", err
}

// evaluateSLI returns the SLI value and, when SigNoz raised one, a
// non-fatal completeness warning collected from either the total or the
// good query (PRD section 11.2) - a warning does not fail the query, the
// same way TimeSeriesValue.Partial doesn't; it is surfaced so a caller can
// decide whether to treat this window's answer with less confidence.
func evaluateSLI(ctx context.Context, querier source.MetricQuerier, cfg Config, definition Definition, start, end uint64) (float64, string, error) {
	goodQuery, totalQuery, err := deriveMetricQueries(cfg, definition)
	if err != nil {
		return 0, "", err
	}
	total, totalWarning, err := scalarWithWarning(ctx, querier, totalQuery, start, end)
	if err != nil {
		return 0, "", fmt.Errorf("total query: %w", err)
	}
	if total <= 0 {
		return 0, totalWarning, ErrNoData
	}
	good, goodWarning, err := scalarWithWarning(ctx, querier, goodQuery, start, end)
	if err != nil {
		return 0, totalWarning, fmt.Errorf("good query: %w", err)
	}
	if good < 0 {
		good = 0
	}
	if good > total {
		good = total
	}
	warning := totalWarning
	if warning == "" {
		warning = goodWarning
	} else if goodWarning != "" && goodWarning != warning {
		warning += "; " + goodWarning
	}
	return good / total, warning, nil
}

// deriveMetricQueries builds the good/total builder queries for a
// definition, scoped to cfg.Service/cfg.Environment (and, for latency
// SLIs, the threshold bucket). The window is deliberately not part of
// either query: Engine.evaluate already picks [start,end] from
// definition.Window before calling ScalarBuilder, and the "increase" time
// aggregation reduces over exactly that range, so the same MetricQuery
// works unchanged across windows (see EvaluateMultiWindow).
func deriveMetricQueries(cfg Config, definition Definition) (source.MetricQuery, source.MetricQuery, error) {
	serviceLabel, environmentLabel := cfg.MetricLabels()
	if trimmed := strings.TrimSpace(definition.ServiceLabel); trimmed != "" {
		serviceLabel = trimmed
	}
	if trimmed := strings.TrimSpace(definition.EnvironmentLabel); trimmed != "" {
		environmentLabel = trimmed
	}
	filter := scopeExpression(cfg.Service, cfg.Environment, serviceLabel, environmentLabel)
	temporality := metricTemporality(definition)
	switch definition.Type {
	case SLITypeRatio, SLITypeGroundedAnswers:
		return counterQuery(definition.GoodMetric, filter, temporality), counterQuery(definition.TotalMetric, filter, temporality), nil
	case SLITypeLatencyThreshold:
		metric := strings.TrimSpace(definition.LatencyMetric)
		if metric == "" {
			return source.MetricQuery{}, source.MetricQuery{}, fmt.Errorf("latency metric is empty")
		}
		thresholdValue := latencyThresholdBucketValue(definition)
		bucketFilter := filter + fmt.Sprintf(" AND le = '%s'", thresholdValue)
		bucketMetric := latencyChildMetric(definition.LatencyBucketMetric, metric, "_bucket")
		countMetric := latencyChildMetric(definition.LatencyCountMetric, metric, "_count")
		good := counterQuery(bucketMetric, bucketFilter, temporality)
		total := counterQuery(countMetric, filter, temporality)
		return good, total, nil
	default:
		return source.MetricQuery{}, source.MetricQuery{}, fmt.Errorf("unsupported SLI type %q", definition.Type)
	}
}

// metricTemporality resolves the OTel temporality deriveMetricQueries'
// counter queries use: "Cumulative" (default - matches a typical
// OTel-SDK-instrumented counter, e.g. a custom request-count metric) or
// "Delta" for MetricTemporality: "delta". This is NOT a given: SigNoz's
// own spanmetrics-processor-derived metrics (signoz_latency.bucket/.count,
// and any other signoz_*-prefixed derived metric) are Delta temporality -
// confirmed live: an "increase" aggregation against signoz_latency.count
// with Cumulative temporality returned an empty result set, while the
// identical query with Delta temporality returned the real count.
func metricTemporality(definition Definition) string {
	if strings.EqualFold(strings.TrimSpace(definition.MetricTemporality), "delta") {
		return "Delta"
	}
	return "Cumulative"
}

// latencyThresholdBucketValue converts ThresholdMS into the string the
// histogram's "le" label is expected to carry, per LatencyBucketUnit.
// Getting the unit wrong does not error - the filter still matches A
// bucket, just the wrong one, silently corrupting the SLI instead of
// failing loudly (see LatencyBucketUnit's doc comment for the live
// evidence this default is based on).
func latencyThresholdBucketValue(definition Definition) string {
	if strings.EqualFold(strings.TrimSpace(definition.LatencyBucketUnit), "s") {
		return strconv.FormatFloat(definition.ThresholdMS/1000, 'f', -1, 64)
	}
	return strconv.FormatFloat(definition.ThresholdMS, 'f', -1, 64)
}

// latencyChildMetric resolves the concrete metric name for a latency
// histogram's bucket or count series: an explicit override
// (LatencyBucketMetric/LatencyCountMetric) if the config set one,
// otherwise the derived <base><suffix> name (today's behavior - the OTel
// semantic-convention underscore suffix). An override is required for
// histograms whose child metrics don't follow that convention - e.g.
// SigNoz's own zero-instrumentation "signoz_latency" metric, whose real
// child metrics are dot-separated (signoz_latency.bucket,
// signoz_latency.count), confirmed live against a running SigNoz
// instance. Getting this wrong does not error: a nonexistent metric name
// queries successfully with zero rows (see ScalarBuilder), so the SLO
// silently reports indeterminate/no-data instead of failing loudly.
func latencyChildMetric(override, base, suffix string) string {
	if trimmed := strings.TrimSpace(override); trimmed != "" {
		return trimmed
	}
	return base + suffix
}

func counterQuery(metric, filter, temporality string) source.MetricQuery {
	return source.MetricQuery{
		Metric:           strings.TrimSpace(metric),
		Filter:           filter,
		TimeAggregation:  "increase",
		SpaceAggregation: "sum",
		Temporality:      temporality,
	}
}

// scopeExpression builds the builder filter expression that scopes a
// metric read to a service and environment, e.g.
// "service_name = 'support-agent' AND environment = 'local'". The engine
// always builds this from cfg.Service/cfg.Environment, so an SLO
// definition can never be misconfigured with the wrong scope the way a
// hand-written PromQL matcher could.
func scopeExpression(service, environment, serviceLabel, environmentLabel string) string {
	if serviceLabel == "" {
		serviceLabel = "service_name"
	}
	if environmentLabel == "" {
		environmentLabel = "environment"
	}
	return fmt.Sprintf("%s = '%s' AND %s = '%s'", serviceLabel, escapeFilterValue(service), environmentLabel, escapeFilterValue(environment))
}

func escapeFilterValue(value string) string {
	return strings.ReplaceAll(value, "'", "\\'")
}
