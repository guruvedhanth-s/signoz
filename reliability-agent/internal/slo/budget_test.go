package slo

import (
	"math"
	"testing"
)

func almost(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestComputeBudget(t *testing.T) {
	b, err := ComputeBudget(BudgetInput{Target: 0.99, Good: 9960, Total: 10000})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !almost(b.AllowedBadEvents, 100) || !almost(b.BadEvents, 40) || !almost(b.RemainingFraction, 0.60) {
		t.Fatalf("budget = %+v", b)
	}
}

func TestComputeBudgetOverspent(t *testing.T) {
	b, err := ComputeBudget(BudgetInput{Target: 0.99, Good: 975, Total: 1000})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b.RemainingFraction >= 0 || b.ConsumedFraction <= 1 {
		t.Fatalf("expected overspent, got %+v", b)
	}
}

func TestComputeBudgetInvalid(t *testing.T) {
	for _, in := range []BudgetInput{{Target: 0}, {Target: 1}, {Target: 0.99, Total: 0}} {
		if _, err := ComputeBudget(in); err == nil {
			t.Fatalf("expected error for %+v", in)
		}
	}
}

func TestBurnRate(t *testing.T) {
	if got := BurnRate(0.10, 0.99); !almost(got, 10) {
		t.Fatalf("burn = %v, want 10", got)
	}
	if got := BurnRate(0.01, 0.99); !almost(got, 1) {
		t.Fatalf("burn = %v, want 1", got)
	}
}

func TestBurnTierFires(t *testing.T) {
	tier := BurnRateTier{Threshold: 14.4}
	if !tier.Fires(WindowBurn{LongBurn: 15, ShortBurn: 20}) {
		t.Fatal("expected fire when both windows exceed threshold")
	}
	if tier.Fires(WindowBurn{LongBurn: 15, ShortBurn: 2}) {
		t.Fatal("expected no fire when only long window exceeds")
	}
}

func TestEvaluateBurnAlerts(t *testing.T) {
	alerts := EvaluateBurnAlerts(DefaultBurnRateTiers, map[string]WindowBurn{
		"fast": {LongBurn: 20, ShortBurn: 20},
		"slow": {LongBurn: 4, ShortBurn: 3.5},
	})
	if len(alerts) != 2 || alerts[0].Tier != "fast" || alerts[0].Severity != BurnSeverityPage {
		t.Fatalf("unexpected alerts: %+v", alerts)
	}
}
