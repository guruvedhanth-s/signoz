package slo

import "fmt"

// BudgetInput is the raw measurement an error budget is computed from.
type BudgetInput struct {
	Target float64 // objective in (0,1]
	Good   float64
	Total  float64
}

// Budget is the computed error-budget state for an SLO window.
type Budget struct {
	AllowedBadEvents  float64
	BadEvents         float64
	RemainingEvents   float64
	RemainingFraction float64 // clamped at 1.0; negative when overspent
	ConsumedFraction  float64
}

// ComputeBudget derives the error-budget state from a measurement.
func ComputeBudget(in BudgetInput) (Budget, error) {
	if in.Target <= 0 || in.Target >= 1 {
		return Budget{}, fmt.Errorf("target must be in (0,1), got %v", in.Target)
	}
	if in.Total <= 0 {
		return Budget{}, fmt.Errorf("no events in window (total=0)")
	}
	allowed := (1 - in.Target) * in.Total
	bad := in.Total - in.Good
	if bad < 0 {
		bad = 0
	}
	b := Budget{AllowedBadEvents: allowed, BadEvents: bad, RemainingEvents: allowed - bad}
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

// ErrorRate is bad/total in the range 0..1.
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

// BurnRate is the observed error rate divided by the allowed error rate.
// A burn rate of 1.0 exhausts the budget exactly by the end of the window.
func BurnRate(errorRate, target float64) float64 {
	allowed := 1 - target
	if allowed <= 0 {
		return 0
	}
	return errorRate / allowed
}
