package implslo

import "github.com/SigNoz/signoz/pkg/errors"

// BudgetInput is the raw measurement an error budget is computed from.
type BudgetInput struct {
	// Target is the SLO objective as a fraction in the range (0,1], e.g. 0.99.
	Target float64
	// Good and Total are event counts over the SLO window.
	Good  float64
	Total float64
}

// Budget is the computed error-budget state for an SLO window.
type Budget struct {
	// AllowedBadEvents is the number of failures the budget permits: (1-target)*total.
	AllowedBadEvents float64
	// BadEvents is the number of failures observed: total-good.
	BadEvents float64
	// RemainingEvents is AllowedBadEvents-BadEvents. Negative when the budget is
	// overspent.
	RemainingEvents float64
	// RemainingFraction is RemainingEvents/AllowedBadEvents, clamped at the top to
	// 1.0. It goes negative once the budget is exhausted.
	RemainingFraction float64
	// ConsumedFraction is BadEvents/AllowedBadEvents. 1.0 means the budget is fully
	// spent; above 1.0 means the SLO is already violated for the window.
	ConsumedFraction float64
}

// ComputeBudget derives the error-budget state from an input measurement.
//
// Target must be in (0,1] and Total must be positive; otherwise there is no
// budget to reason about and an error is returned.
func ComputeBudget(in BudgetInput) (Budget, error) {
	if in.Target <= 0 || in.Target >= 1 {
		return Budget{}, errors.Newf(errors.TypeInvalidInput, errors.CodeInvalidInput, "target must be in (0,1), got %v", in.Target)
	}
	if in.Total <= 0 {
		return Budget{}, errors.New(errors.TypeNotFound, errors.CodeNotFound, "no events in window (total=0)")
	}

	allowed := (1 - in.Target) * in.Total
	bad := in.Total - in.Good
	if bad < 0 {
		bad = 0
	}

	b := Budget{
		AllowedBadEvents: allowed,
		BadEvents:        bad,
		RemainingEvents:  allowed - bad,
	}
	if allowed > 0 {
		b.ConsumedFraction = bad / allowed
		remaining := 1 - b.ConsumedFraction
		if remaining > 1 {
			remaining = 1
		}
		b.RemainingFraction = remaining
	}
	return b, nil
}

// ErrorRate is bad/total in the range 0..1. Total must be positive.
func ErrorRate(good, total float64) float64 {
	if total <= 0 {
		return 0
	}
	bad := total - good
	if bad < 0 {
		bad = 0
	}
	return bad / total
}

// BurnRate is how fast the error budget is being consumed relative to the SLO.
//
// A burn rate of 1.0 exactly exhausts the budget by the end of the window; 2.0
// consumes it twice as fast. It is the observed error rate divided by the
// allowed error rate (1-target).
func BurnRate(errorRate, target float64) float64 {
	allowedErrorRate := 1 - target
	if allowedErrorRate <= 0 {
		return 0
	}
	return errorRate / allowedErrorRate
}
