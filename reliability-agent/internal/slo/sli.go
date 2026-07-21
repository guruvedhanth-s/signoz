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

// evaluateRatio computes a ratio SLI as good / total over the window.
//
// A total of zero means there is no traffic to measure and is returned as an
// error so the caller can decide it is indeterminate rather than a silent pass.
func evaluateRatio(ctx context.Context, sq ScalarQuerier, def SLODefinition, startMs, endMs uint64) (float64, error) {
	total, err := sq.Scalar(ctx, def.TotalQuery, startMs, endMs)
	if err != nil {
		return 0, fmt.Errorf("slo %q: total query failed: %w", def.Name, err)
	}
	if total <= 0 {
		return 0, fmt.Errorf("slo %q: no events in window (total=0)", def.Name)
	}
	good, err := sq.Scalar(ctx, def.GoodQuery, startMs, endMs)
	if err != nil {
		return 0, fmt.Errorf("slo %q: good query failed: %w", def.Name, err)
	}
	if good < 0 {
		good = 0
	}
	if good > total {
		good = total
	}
	return good / total, nil
}
