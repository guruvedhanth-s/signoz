package implslo

import (
	"math"
	"testing"
)

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestComputeBudget(t *testing.T) {
	// 99% target, 10,000 requests -> 100 allowed failures.
	// 40 observed failures -> 60 remaining, 40% consumed.
	b, err := ComputeBudget(BudgetInput{Target: 0.99, Good: 9960, Total: 10000})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !almostEqual(b.AllowedBadEvents, 100) {
		t.Fatalf("allowed = %v, want 100", b.AllowedBadEvents)
	}
	if !almostEqual(b.BadEvents, 40) {
		t.Fatalf("bad = %v, want 40", b.BadEvents)
	}
	if !almostEqual(b.RemainingEvents, 60) {
		t.Fatalf("remaining events = %v, want 60", b.RemainingEvents)
	}
	if !almostEqual(b.ConsumedFraction, 0.40) {
		t.Fatalf("consumed = %v, want 0.40", b.ConsumedFraction)
	}
	if !almostEqual(b.RemainingFraction, 0.60) {
		t.Fatalf("remaining fraction = %v, want 0.60", b.RemainingFraction)
	}
}

func TestComputeBudgetOverspent(t *testing.T) {
	// 99% target, 1,000 requests -> 10 allowed. 25 failures -> overspent.
	b, err := ComputeBudget(BudgetInput{Target: 0.99, Good: 975, Total: 1000})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b.RemainingEvents >= 0 {
		t.Fatalf("expected negative remaining events, got %v", b.RemainingEvents)
	}
	if b.RemainingFraction >= 0 {
		t.Fatalf("expected negative remaining fraction, got %v", b.RemainingFraction)
	}
	if b.ConsumedFraction <= 1 {
		t.Fatalf("expected consumed > 1, got %v", b.ConsumedFraction)
	}
}

func TestComputeBudgetInvalid(t *testing.T) {
	for _, in := range []BudgetInput{
		{Target: 0, Good: 1, Total: 10},
		{Target: 1, Good: 1, Total: 10},
		{Target: 0.99, Good: 0, Total: 0},
	} {
		if _, err := ComputeBudget(in); err == nil {
			t.Fatalf("expected error for input %+v", in)
		}
	}
}

func TestBurnRate(t *testing.T) {
	// SLO 99% -> allowed error rate 1%. Observed 10% -> burn 10x.
	got := BurnRate(0.10, 0.99)
	if !almostEqual(got, 10) {
		t.Fatalf("burn = %v, want 10", got)
	}
	// Exactly at budget.
	if got := BurnRate(0.01, 0.99); !almostEqual(got, 1) {
		t.Fatalf("burn = %v, want 1", got)
	}
	// Degenerate target.
	if got := BurnRate(0.5, 1.0); got != 0 {
		t.Fatalf("burn = %v, want 0 for target=1", got)
	}
}

func TestErrorRate(t *testing.T) {
	if got := ErrorRate(900, 1000); !almostEqual(got, 0.1) {
		t.Fatalf("error rate = %v, want 0.1", got)
	}
	if got := ErrorRate(5, 0); got != 0 {
		t.Fatalf("error rate = %v, want 0 for total=0", got)
	}
	if got := ErrorRate(1200, 1000); got != 0 {
		t.Fatalf("error rate = %v, want 0 when good>total", got)
	}
}
