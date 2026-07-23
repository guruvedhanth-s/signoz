package slo

import (
	"context"
	"testing"
	"time"
)

// countStub answers count(<metric>) queries; a metric is "present" if listed.
type countStub struct{ present map[string]bool }

func (s countStub) Scalar(_ context.Context, expr string, _, _ uint64) (float64, error) {
	// expr is count(<metric>); extract the metric name.
	name := expr[len("count(") : len(expr)-1]
	if s.present[name] {
		return 1, nil
	}
	return 0, errNoData
}

var errNoData = &noDataError{}

type noDataError struct{}

func (*noDataError) Error() string { return "no data" }

func newGate(sq ScalarQuerier, expected []string) *MetricPresenceGate {
	return &MetricPresenceGate{scalar: sq, expected: expected, now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
}

func TestCompletenessAllPresent(t *testing.T) {
	g := newGate(countStub{present: map[string]bool{"a": true, "b": true}}, []string{"a", "b"})
	got, err := g.Completeness(context.Background(), "svc", "30d")
	if err != nil || got != 1.0 {
		t.Fatalf("got %v, %v; want 1.0", got, err)
	}
}

func TestCompletenessPartial(t *testing.T) {
	// 2 of 3 present -> 0.666..., below a 0.95 gate.
	g := newGate(countStub{present: map[string]bool{"a": true, "b": true}}, []string{"a", "b", "missing"})
	got, err := g.Completeness(context.Background(), "svc", "30d")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got >= DefaultGateThreshold {
		t.Fatalf("completeness %v should be below gate %v", got, DefaultGateThreshold)
	}
}

func TestCompletenessNoExpectedTrusts(t *testing.T) {
	g := newGate(countStub{}, nil)
	got, _ := g.Completeness(context.Background(), "svc", "30d")
	if got != 1.0 {
		t.Fatalf("got %v, want 1.0 when nothing to check", got)
	}
}
