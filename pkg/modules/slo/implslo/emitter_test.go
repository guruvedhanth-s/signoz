package implslo

import (
	"context"
	"testing"

	"github.com/SigNoz/signoz/pkg/instrumentation/instrumentationtest"
	"github.com/SigNoz/signoz/pkg/types/slotypes"
)

func TestStateToValue(t *testing.T) {
	cases := map[slotypes.State]float64{
		slotypes.StateHealthy:       stateValueHealthy,
		slotypes.StateUnhealthy:     stateValueUnhealthy,
		slotypes.StateIndeterminate: stateValueIndeterminate,
	}
	for state, want := range cases {
		if got := stateToValue(state); got != want {
			t.Fatalf("stateToValue(%v) = %v, want %v", state, got, want)
		}
	}
}

func TestEmitterNilSafe(t *testing.T) {
	// A nil emitter must not panic, making emission optional.
	var e *emitter
	e.Emit(context.Background(), &slotypes.Report{Name: "x"})
}

func TestNewEmitterAndEmit(t *testing.T) {
	meter := instrumentationtest.New().ToProviderSettings().MeterProvider.Meter("test")
	e, err := newEmitter(meter)
	if err != nil {
		t.Fatalf("newEmitter: %v", err)
	}
	// Should record without error for each state.
	e.Emit(context.Background(), &slotypes.Report{Name: "healthy", State: slotypes.StateHealthy, SLI: 0.99})
	e.Emit(context.Background(), &slotypes.Report{Name: "indeterminate", State: slotypes.StateIndeterminate})
}
