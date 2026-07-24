package source

import (
	"context"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/evidence"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/profile"
)

type Target struct {
	Service     string
	Environment string
	Start       time.Time
	End         time.Time
}

type TelemetrySource interface {
	Snapshot(context.Context, profile.Profile, Target) (evidence.Snapshot, error)
}

// MemorySource provides deterministic evidence for local development and
// tests. A SigNoz adapter will implement the same interface in Phase 1.
type MemorySource struct {
	Data evidence.Snapshot
}

func (s MemorySource) Snapshot(context.Context, profile.Profile, Target) (evidence.Snapshot, error) {
	return s.Data, nil
}

// MetricQuery describes a single scalar metric read as a SigNoz builder
// query: a metric name, a builder filter expression (e.g.
// "service_name = 'support-agent' AND environment = 'local'"), and the
// aggregation shape needed to collapse the metric's samples in a time
// window down to one number. It intentionally carries no time range -
// callers supply [startMs, endMs] to MetricQuerier.ScalarBuilder - so the
// same MetricQuery can be reused across windows (see the SLO engine's
// multi-window burn-rate evaluation).
type MetricQuery struct {
	Metric           string
	Filter           string
	TimeAggregation  string
	SpaceAggregation string
	Temporality      string
}

// MetricQuerier issues a single builder-query scalar read against the
// telemetry backend and returns the extracted value. Implementations must
// treat "no samples in the window" as a valid answer of 0, not an error;
// callers that need to distinguish absent telemetry from a real zero (for
// example a completeness gate) should query with a presence-oriented
// aggregation (e.g. TimeAggregation "count") instead of relying on an
// error return.
type MetricQuerier interface {
	ScalarBuilder(ctx context.Context, query MetricQuery, startMs, endMs uint64) (float64, error)
}
