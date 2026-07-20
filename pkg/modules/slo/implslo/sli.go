package implslo

import (
	"context"

	"github.com/SigNoz/signoz/pkg/errors"
	"github.com/SigNoz/signoz/pkg/types/slotypes"
	"github.com/SigNoz/signoz/pkg/valuer"
)

// ScalarQuerier is the transport seam for SLI evaluation. It runs a single
// scalar-producing query expression over a time range and returns one number.
//
// Decoupling the SLI math from query construction keeps evaluateRatio (and the
// other SLI evaluators to come) fully unit-testable with a mock, while the real
// querier wiring lives in querierScalarQuerier.
type ScalarQuerier interface {
	// Scalar runs expr (a PromQL metric expression) over [startMs, endMs] and
	// returns the resulting scalar value.
	Scalar(ctx context.Context, orgID valuer.UUID, expr string, startMs, endMs uint64) (float64, error)
}

// evaluateRatio computes a ratio SLI as good / total over the window ending at
// endMs.
//
// It returns the SLI as a fraction in the range 0..1. A total of zero means there
// is no traffic to measure and is surfaced as an error so the caller can decide
// whether that is indeterminate rather than a silent pass.
func evaluateRatio(
	ctx context.Context,
	sq ScalarQuerier,
	orgID valuer.UUID,
	def slotypes.SLODefinition,
	startMs, endMs uint64,
) (float64, error) {
	total, err := sq.Scalar(ctx, orgID, def.TotalQuery, startMs, endMs)
	if err != nil {
		return 0, errors.Wrapf(err, errors.TypeInternal, errors.CodeInternal, "slo %q: total query failed", def.Name)
	}
	if total <= 0 {
		return 0, errors.Newf(errors.TypeNotFound, errors.CodeNotFound, "slo %q: no events in window (total=0)", def.Name)
	}

	good, err := sq.Scalar(ctx, orgID, def.GoodQuery, startMs, endMs)
	if err != nil {
		return 0, errors.Wrapf(err, errors.TypeInternal, errors.CodeInternal, "slo %q: good query failed", def.Name)
	}
	if good < 0 {
		good = 0
	}
	if good > total {
		good = total
	}

	return good / total, nil
}
