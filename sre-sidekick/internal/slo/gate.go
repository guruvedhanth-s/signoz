package slo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/source"
)

// MetricPresenceGate is the first real gate implementation. It checks that
// every configured dependency has telemetry for the requested service,
// environment, and window. Track A will later provide a richer
// dependency-aware gate.
type MetricPresenceGate struct {
	Metrics          source.MetricQuerier
	Expected         []string
	ServiceLabel     string
	EnvironmentLabel string
	Now              func() time.Time
}

func NewMetricPresenceGate(metrics source.MetricQuerier, expected []string) *MetricPresenceGate {
	return &MetricPresenceGate{
		Metrics:          metrics,
		Expected:         expected,
		ServiceLabel:     "service_name",
		EnvironmentLabel: "environment",
		Now:              time.Now,
	}
}

type GateRequest struct {
	Service          string
	Environment      string
	Window           time.Duration
	Dependencies     []string
	ServiceLabel     string
	EnvironmentLabel string
	// Now is the evaluation instant the caller (Engine.Evaluate) is scoring
	// against - the end of the [start,end] window the gate must check
	// dependency presence for. Zero means "use the gate's own clock"
	// (g.Now, defaulting to time.Now), which keeps direct/test callers
	// that never set it working unchanged. Engine always sets this to the
	// same `now` it evaluates the SLI against, so a completeness check for
	// a historical window (e.g. server.go's SLORequest.Now) is scoped to
	// that window - not to whatever time the gate happens to run at -
	// otherwise a gate call for a past window would check today's
	// telemetry instead of the window actually being evaluated.
	Now time.Time
}

func (g *MetricPresenceGate) Check(ctx context.Context, request GateRequest) (GateResult, error) {
	metrics := request.Dependencies
	if len(metrics) == 0 {
		metrics = g.Expected
	}
	if len(metrics) == 0 {
		return GateResult{Coverage: 1, QueryComplete: true}, nil
	}
	if g.Metrics == nil {
		return GateResult{QueryComplete: false, Trusted: false, Reason: "metric querier is not configured"}, nil
	}
	now := request.Now
	if now.IsZero() {
		now = g.Now()
	}
	start := uint64(now.Add(-request.Window).UnixMilli())
	end := uint64(now.UnixMilli())
	serviceLabel := request.ServiceLabel
	if serviceLabel == "" {
		serviceLabel = g.ServiceLabel
	}
	environmentLabel := request.EnvironmentLabel
	if environmentLabel == "" {
		environmentLabel = g.EnvironmentLabel
	}
	filter := scopeExpression(request.Service, request.Environment, serviceLabel, environmentLabel)
	present := 0
	for _, metric := range metrics {
		// A dependency is "present" when it has any samples at all in the
		// window, regardless of value - so this counts raw samples
		// ("count") rather than reading the counter's increase the way the
		// SLI query does. A counter that is genuinely stuck at zero (no
		// good events) still emits samples and must count as present; only
		// a metric with zero samples is missing telemetry.
		query := source.MetricQuery{
			Metric:           strings.TrimSpace(metric),
			Filter:           filter,
			TimeAggregation:  "count",
			SpaceAggregation: "sum",
		}
		value, err := g.Metrics.ScalarBuilder(ctx, query, start, end)
		if err != nil {
			return GateResult{
				Coverage:      float64(present) / float64(len(metrics)),
				QueryComplete: false,
				Trusted:       false,
				Reason:        fmt.Sprintf("dependency %s query failed: %v", metric, err),
			}, nil
		}
		if value > 0 {
			present++
		}
	}
	coverage := float64(present) / float64(len(metrics))
	return GateResult{
		Coverage:      coverage,
		QueryComplete: true,
		Reason:        fmt.Sprintf("%d of %d dependencies have data", present, len(metrics)),
	}, nil
}
