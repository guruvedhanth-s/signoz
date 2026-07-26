package slo

import "time"

type SLIType string

const (
	SLITypeRatio            SLIType = "ratio"
	SLITypeLatencyThreshold SLIType = "latency_threshold"
	SLITypeCompleteness     SLIType = "completeness"
	SLITypeGroundedAnswers  SLIType = "grounded_answers"
)

type State string

const (
	StateHealthy       State = "healthy"
	StateUnhealthy     State = "unhealthy"
	StateIndeterminate State = "indeterminate"
)

type GateResult struct {
	Coverage      float64 `json:"coverage"`
	QueryComplete bool    `json:"query_complete"`
	Trusted       bool    `json:"trusted"`
	Reason        string  `json:"reason,omitempty"`
	// Warning carries SigNoz's own top-level query-completeness warning
	// for any dependency query that raised one (PRD section 11.2),
	// joined with "; " if more than one dependency warned.
	Warning string `json:"warning,omitempty"`
}

type Report struct {
	SchemaVersion string  `json:"schema_version"`
	Name          string  `json:"name"`
	Service       string  `json:"service"`
	Environment   string  `json:"environment"`
	Type          SLIType `json:"type"`
	Window        string  `json:"window"`
	// EvaluatedStart and EvaluatedEnd are the actual [start,end) instants
	// the SLI was queried over - now.Add(-window) and now, at the moment
	// Engine.evaluate ran (PRD section 11.2: "return the evaluated start
	// and end timestamps"). Zero for a report that never reached a query
	// (e.g. an invalid window).
	EvaluatedStart       time.Time  `json:"evaluated_start,omitempty"`
	EvaluatedEnd         time.Time  `json:"evaluated_end,omitempty"`
	State                State      `json:"state"`
	SLI                  float64    `json:"sli,omitempty"`
	Target               float64    `json:"target"`
	Completeness         float64    `json:"completeness"`
	ErrorBudgetRemaining float64    `json:"error_budget_remaining,omitempty"`
	BurnRate             float64    `json:"burn_rate,omitempty"`
	Gate                 GateResult `json:"gate"`
	// Warning carries SigNoz's own top-level query-completeness warning
	// (PRD section 11.2), when either the completeness gate or the SLI
	// query itself raised one - e.g. a metric that has gone dormant, or a
	// clamped step interval. A non-empty Warning does not mean the report
	// is untrustworthy on its own (Gate.Trusted / State already say that);
	// it is additional context for why a number might look surprising.
	Warning string `json:"warning,omitempty"`
	Error   string `json:"error,omitempty"`
}

type BurnTier struct {
	Name        string
	LongWindow  string
	ShortWindow string
	Threshold   float64
	Severity    string
}

var DefaultBurnTiers = []BurnTier{
	{Name: "fast", LongWindow: "1h", ShortWindow: "5m", Threshold: 14.4, Severity: "page"},
	{Name: "medium", LongWindow: "6h", ShortWindow: "30m", Threshold: 6, Severity: "ticket"},
	{Name: "slow", LongWindow: "24h", ShortWindow: "2h", Threshold: 3, Severity: "ticket"},
}
