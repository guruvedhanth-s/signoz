package otlp

import (
	"testing"

	"github.com/guruvedhanth-s/signoz/reliability-agent/internal/slo"
)

func TestStateValue(t *testing.T) {
	cases := map[slo.State]float64{
		slo.StateHealthy:       stateHealthy,
		slo.StateUnhealthy:     stateUnhealthy,
		slo.StateIndeterminate: stateIndeterminate,
	}
	for s, want := range cases {
		if got := stateValue(s); got != want {
			t.Fatalf("stateValue(%v) = %v, want %v", s, got, want)
		}
	}
}
