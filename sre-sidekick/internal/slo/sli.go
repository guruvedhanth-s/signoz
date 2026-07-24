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

func evaluateSLI(ctx context.Context, querier source.MetricQuerier, cfg Config, definition Definition, start, end uint64) (float64, error) {
	goodQuery, totalQuery, err := deriveMetricQueries(cfg, definition)
	if err != nil {
		return 0, err
	}
	total, err := querier.ScalarBuilder(ctx, totalQuery, start, end)
	if err != nil {
		return 0, fmt.Errorf("total query: %w", err)
	}
	if total <= 0 {
		return 0, ErrNoData
	}
	good, err := querier.ScalarBuilder(ctx, goodQuery, start, end)
	if err != nil {
		return 0, fmt.Errorf("good query: %w", err)
	}
	if good < 0 {
		good = 0
	}
	if good > total {
		good = total
	}
	return good / total, nil
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
	filter := scopeExpression(cfg.Service, cfg.Environment, serviceLabel, environmentLabel)
	switch definition.Type {
	case SLITypeRatio, SLITypeCompleteness, SLITypeGroundedAnswers:
		return counterQuery(definition.GoodMetric, filter), counterQuery(definition.TotalMetric, filter), nil
	case SLITypeLatencyThreshold:
		metric := strings.TrimSpace(definition.LatencyMetric)
		if metric == "" {
			return source.MetricQuery{}, source.MetricQuery{}, fmt.Errorf("latency metric is empty")
		}
		thresholdSeconds := strconv.FormatFloat(definition.ThresholdMS/1000, 'f', -1, 64)
		bucketFilter := filter + fmt.Sprintf(" AND le = '%s'", thresholdSeconds)
		good := counterQuery(metric+"_bucket", bucketFilter)
		total := counterQuery(metric+"_count", filter)
		return good, total, nil
	default:
		return source.MetricQuery{}, source.MetricQuery{}, fmt.Errorf("unsupported SLI type %q", definition.Type)
	}
}

func counterQuery(metric, filter string) source.MetricQuery {
	return source.MetricQuery{
		Metric:           strings.TrimSpace(metric),
		Filter:           filter,
		TimeAggregation:  "increase",
		SpaceAggregation: "sum",
		Temporality:      "Cumulative",
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
