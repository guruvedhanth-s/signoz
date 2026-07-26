package faults

import "testing"

// A rate is the whole point of this package. At 1.0 the good-path metric stops
// being emitted entirely, so an SLO's "good" counter has no samples and the
// completeness gate correctly refuses to diagnose - which is the product
// working, but not a diagnosable incident. Verified live before this test was
// written.
func TestActiveHonoursTheRate(t *testing.T) {
	controller := New()

	// A deterministic sampler walking 0.0, 0.1, ... 0.9 stands in for 10
	// requests, so "80% of requests" is checkable rather than probabilistic.
	var call int
	controller.WithRandom(func() float64 {
		value := float64(call) / 10
		call++
		return value
	})

	controller.Set(PaymentsErrors, 0.8)
	active := 0
	for range 10 {
		if controller.Active(PaymentsErrors) {
			active++
		}
	}
	if active != 8 {
		t.Errorf("rate 0.8 over 10 samples fired %d times, want 8", active)
	}
}

func TestRateBoundaries(t *testing.T) {
	controller := New().WithRandom(func() float64 { return 0.999 })

	// 1.0 must fire every time without consulting the sampler, so "always" is
	// exact rather than nearly-always.
	controller.Set(PaymentsLatency, 1)
	for range 5 {
		if !controller.Active(PaymentsLatency) {
			t.Fatal("rate 1.0 must always be active")
		}
	}

	// Above 1 is clamped rather than rejected: the UI sends a slider value and
	// should not be able to produce an invalid state.
	if got := controller.Set(PaymentsLatency, 4.2); got != 1 {
		t.Errorf("Set(4.2) = %v, want clamped to 1", got)
	}

	// Zero and negative disable, and a disabled mode never consults the
	// sampler - important because the sampler is shared and a stray call would
	// perturb another mode's sequence.
	if got := controller.Set(PaymentsLatency, 0); got != 0 {
		t.Errorf("Set(0) = %v, want 0", got)
	}
	if controller.Active(PaymentsLatency) {
		t.Error("rate 0 must never be active")
	}
	if got := controller.Set(PaymentsLatency, -1); got != 0 {
		t.Errorf("Set(-1) = %v, want 0", got)
	}
}

func TestClearDisablesEverythingAndNotifies(t *testing.T) {
	controller := New()
	var notified []Mode
	controller.OnChange(func(mode Mode, rate float64) {
		if rate == 0 {
			notified = append(notified, mode)
		}
	})

	controller.Set(PaymentsErrors, 0.5)
	controller.Set(LogsMissingTraceID, 1)
	controller.Clear()

	for _, mode := range AllModes() {
		if controller.Rate(mode) != 0 {
			t.Errorf("%s still enabled after Clear", mode)
		}
	}
	// The log-correlation fault is applied outside the request path, on the log
	// emitter, so clearing it *must* notify or logs would silently stay broken
	// after the UI says faults are off.
	if len(notified) != 2 {
		t.Errorf("Clear notified %v, want both previously-enabled modes", notified)
	}
}

func TestSnapshotAlwaysReportsEveryMode(t *testing.T) {
	controller := New()
	controller.Set(PaymentsTimeout, 0.3)

	snapshot := controller.Snapshot()

	// Every mode is present even when off, so the UI can render the full list
	// from the server's answer rather than hardcoding it and drifting.
	if len(snapshot) != len(AllModes()) {
		t.Fatalf("snapshot has %d modes, want %d", len(snapshot), len(AllModes()))
	}
	if snapshot[string(PaymentsTimeout)] != 0.3 {
		t.Errorf("snapshot rate = %v, want 0.3", snapshot[string(PaymentsTimeout)])
	}
	if snapshot[string(PaymentsErrors)] != 0 {
		t.Errorf("disabled mode should report 0, got %v", snapshot[string(PaymentsErrors)])
	}
}

func TestValidRejectsUnknownModes(t *testing.T) {
	if !Valid("payments_errors") {
		t.Error("payments_errors must be valid")
	}
	// The HTTP handlers gate on this, so an unknown mode is a 400 rather than a
	// silently ignored request that leaves the operator thinking a fault is on.
	if Valid("drop_database") {
		t.Error("unknown modes must be rejected")
	}
}
