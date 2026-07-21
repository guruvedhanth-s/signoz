package slo

import (
	"context"
	"fmt"
)

// ScalarQuerier is the transport seam for SLI evaluation. It runs a single
// scalar-producing PromQL expression over a time range and returns one number.
//
// The signoz package provides the production implementation over HTTP; tests use
// a stub. Decoupling the math from transport keeps the evaluators pure.
type ScalarQuerier interface {
	Scalar(ctx context.Context, expr string, startMs, endMs uint64) (float64, error)
}

// evaluateSLI evaluates any good/total-based SLI type by deriving the good and
// total queries for the definition's type and computing good / total.
func evaluateSLI(ctx context.Context, sq ScalarQuerier, def SLODefinition, startMs, endMs uint64) (float64, error) {
	goodQuery, totalQuery, err := deriveQueries(def)
	if err != nil {
		return 0, err
	}
	return computeRatio(ctx, sq, def.Name, goodQuery, totalQuery, startMs, endMs)
}

// deriveQueries returns the good and total PromQL expressions for an SLI type.
//
//	ratio / completeness / grounded_answers -> the good_query and total_query as authored
//	latency_threshold                       -> histogram bucket queries built from
//	                                           latency_metric and threshold_ms
//
// All four types reduce to a good/total ratio; the type gives semantic meaning
// and drives how the two queries are obtained.
func deriveQueries(def SLODefinition) (good, total string, err error) {
	switch def.Type {
	case SLITypeRatio, SLITypeCompleteness, SLITypeGroundedAnswers:
		return def.GoodQuery, def.TotalQuery, nil
	case SLITypeLatencyThreshold:
		le := def.ThresholdMs / 1000 // OTel duration histograms bucket in seconds
		good = fmt.Sprintf("sum(%s_bucket{le=\"%g\"})", def.LatencyMetric, le)
		total = fmt.Sprintf("sum(%s_count)", def.LatencyMetric)
		return good, total, nil
	default:
		return "", "", fmt.Errorf("slo %q: unsupported SLI type %q", def.Name, def.Type)
	}
}

// computeRatio evaluates good / total over the window.
//
// A total of zero means there is no traffic to measure and is returned as an
// error so the caller can decide it is indeterminate rather than a silent pass.
func computeRatio(ctx context.Context, sq ScalarQuerier, name, goodQuery, totalQuery string, startMs, endMs uint64) (float64, error) {
	total, err := sq.Scalar(ctx, totalQuery, startMs, endMs)
	if err != nil {
		return 0, fmt.Errorf("slo %q: total query failed: %w", name, err)
	}
	if total <= 0 {
		return 0, fmt.Errorf("slo %q: no events in window (total=0)", name)
	}
	good, err := sq.Scalar(ctx, goodQuery, startMs, endMs)
	if err != nil {
		return 0, fmt.Errorf("slo %q: good query failed: %w", name, err)
	}
	if good < 0 {
		good = 0
	}
	if good > total {
		good = total
	}
	return good / total, nil
}
