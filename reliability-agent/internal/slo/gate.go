package slo

import (
	"context"
	"time"
)

// MetricPresenceGate is a minimal, real completeness gate. It reports the
// fraction of a service's expected metrics that actually have data in SigNoz
// over the window. When that fraction drops below an SLO's gate threshold, the
// SLO resolves to indeterminate: the telemetry needed to trust it is incomplete.
//
// This replaces NoopGate with a genuine signal, using only the public query API.
type MetricPresenceGate struct {
	scalar   ScalarQuerier
	expected []string
	now      func() time.Time
}

// NewMetricPresenceGate builds a gate that checks the given metric names.
func NewMetricPresenceGate(scalar ScalarQuerier, expected []string) *MetricPresenceGate {
	return &MetricPresenceGate{scalar: scalar, expected: expected, now: time.Now}
}

// Completeness returns present/expected in the range 0..1. With no expected
// metrics configured it returns 1.0 (nothing to check, so trust).
func (g *MetricPresenceGate) Completeness(ctx context.Context, _ string, window Window) (float64, error) {
	if len(g.expected) == 0 {
		return 1.0, nil
	}
	dur, err := window.Duration()
	if err != nil {
		return 0, err
	}
	end := uint64(g.now().UnixMilli())
	start := uint64(g.now().Add(-dur).UnixMilli())

	present := 0
	for _, metric := range g.expected {
		// count(<metric>) returns the series count (>0) when the metric has data;
		// an absent metric yields no data points and an error.
		v, err := g.scalar.Scalar(ctx, "count("+metric+")", start, end)
		if err == nil && v > 0 {
			present++
		}
	}
	return float64(present) / float64(len(g.expected)), nil
}
