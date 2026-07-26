// Package faults is the demo's fault injector: named failure modes, each with
// a rate, toggled at runtime from the UI.
//
// Two design points that came out of actually running the demo:
//
//   - Every mode has a *rate*, not just on/off. At a 100% error rate the
//     good-path metric stops being emitted entirely, so an SLO's "good" counter
//     has no samples, the completeness gate cannot tell "everything failed"
//     apart from "telemetry stopped", and it correctly refuses to diagnose.
//     That is the product working, but it is not a diagnosable incident. A
//     partial rate keeps both paths alive and is what produces a real one.
//   - The default rate is therefore 0.8, and 1.0 is reachable deliberately -
//     it is how you demonstrate the refusal on purpose.
package faults

import (
	"math/rand"
	"sort"
	"sync"
)

// Mode names a failure the demo can inject.
type Mode string

const (
	// PaymentsLatency makes payments slow enough to push the latency SLO past
	// its threshold without erroring.
	PaymentsLatency Mode = "payments_latency"
	// PaymentsErrors makes payments return 5xx.
	PaymentsErrors Mode = "payments_errors"
	// PaymentsTimeout makes payments hang past the caller's timeout, which
	// surfaces as an error in the caller and a long span in payments.
	PaymentsTimeout Mode = "payments_timeout"
	// LogsMissingTraceID drops trace correlation from log records. This is a
	// telemetry-quality fault, not a reliability one: the service keeps working
	// while the evidence about it degrades, which is what Track A exists to
	// catch and what the "honesty beat" needs.
	LogsMissingTraceID Mode = "logs_missing_trace_id"
)

// DefaultRate is used when a mode is enabled without an explicit rate. See the
// package comment for why it is not 1.0.
const DefaultRate = 0.8

// AllModes is every mode, in a stable order for the UI and for tests.
func AllModes() []Mode {
	return []Mode{PaymentsLatency, PaymentsErrors, PaymentsTimeout, LogsMissingTraceID}
}

// Valid reports whether name is a known mode.
func Valid(name string) bool {
	for _, mode := range AllModes() {
		if Mode(name) == mode {
			return true
		}
	}
	return false
}

// Controller holds the live fault state. Safe for concurrent use: every
// request reads it and the UI writes it.
//
// There is deliberately no change callback. An earlier version pushed
// notifications to consumers so the log emitter could cache a flag, which
// duplicated the state and introduced both a data race and a notification
// ordering hazard - two Set calls could apply in one order and notify in
// another, leaving the cached copy disagreeing with Snapshot. Consumers pull
// from Active instead, so there is exactly one copy of the truth.
type Controller struct {
	mu    sync.RWMutex
	rates map[Mode]float64
	// rng is injectable so tests are deterministic.
	rng func() float64
}

func New() *Controller {
	return &Controller{
		rates: map[Mode]float64{},
		rng:   rand.Float64,
	}
}

// WithRandom replaces the sampler. Tests use it to make Active deterministic.
func (c *Controller) WithRandom(rng func() float64) *Controller {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rng = rng
	return c
}

// Set enables a mode at the given rate. A rate <= 0 disables it; a rate > 1 is
// clamped. Returns the effective rate.
func (c *Controller) Set(mode Mode, rate float64) float64 {
	if rate > 1 {
		rate = 1
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if rate <= 0 {
		delete(c.rates, mode)
		return 0
	}
	c.rates[mode] = rate
	return rate
}

// Clear disables every mode.
func (c *Controller) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rates = map[Mode]float64{}
}

// Rate returns the configured rate for a mode, 0 when disabled.
func (c *Controller) Rate(mode Mode) float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.rates[mode]
}

// Active rolls the dice for one request. A mode at rate 0.8 returns true for
// roughly 80% of calls.
func (c *Controller) Active(mode Mode) bool {
	c.mu.RLock()
	rate, ok := c.rates[mode]
	rng := c.rng
	c.mu.RUnlock()
	if !ok || rate <= 0 {
		return false
	}
	if rate >= 1 {
		return true
	}
	return rng() < rate
}

// Snapshot is the current state, for the UI and for the /api/fault response.
func (c *Controller) Snapshot() map[string]float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]float64, len(AllModes()))
	for _, mode := range AllModes() {
		out[string(mode)] = c.rates[mode]
	}
	return out
}

// Enabled lists the currently active modes, sorted, for logging.
func (c *Controller) Enabled() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var names []string
	for mode, rate := range c.rates {
		if rate > 0 {
			names = append(names, string(mode))
		}
	}
	sort.Strings(names)
	return names
}
