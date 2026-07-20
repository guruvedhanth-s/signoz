package implslo

import (
	"testing"

	"github.com/SigNoz/signoz/pkg/types/slotypes"
)

func TestResolveState(t *testing.T) {
	tests := []struct {
		name          string
		sli           float64
		target        float64
		completeness  float64
		gate          float64
		requiresCompl bool
		want          slotypes.State
	}{
		{"healthy when met and complete", 0.995, 0.99, 1.0, 0.95, true, slotypes.StateHealthy},
		{"unhealthy when missed and complete", 0.98, 0.99, 1.0, 0.95, true, slotypes.StateUnhealthy},
		{"indeterminate when telemetry incomplete", 0.995, 0.99, 0.40, 0.95, true, slotypes.StateIndeterminate},
		{"incomplete but not gated -> trust the number", 0.995, 0.99, 0.40, 0.95, false, slotypes.StateHealthy},
		{"exactly at target is healthy", 0.99, 0.99, 1.0, 0.95, true, slotypes.StateHealthy},
		{"exactly at gate is trusted", 0.98, 0.99, 0.95, 0.95, true, slotypes.StateUnhealthy},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveState(tt.sli, tt.target, tt.completeness, tt.gate, tt.requiresCompl)
			if got != tt.want {
				t.Fatalf("resolveState = %v, want %v", got, tt.want)
			}
		})
	}
}
