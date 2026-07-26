package mutation

import (
	"math"
	"testing"
	"time"
)

func TestRequestValidationRejectsUnsafeValues(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), 0, 1001} {
		if err := (Request{Kind: UpdateBurn, SLO: "checkout", Tier: "fast", NewMultiplier: value}).Validate(); err == nil {
			t.Errorf("multiplier %v was accepted", value)
		}
	}
	if err := (Request{Kind: SilenceAlert, Name: "alert", Duration: 25 * time.Hour}).Validate(); err == nil {
		t.Fatal("overlong silence was accepted")
	}
}
