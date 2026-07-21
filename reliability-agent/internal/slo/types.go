// Package slo is the SLO and error-budget engine for the reliability agent.
//
// It is fully decoupled from SigNoz internals: it talks to a stock SigNoz over
// HTTP (through the ScalarQuerier seam) and never imports SigNoz packages.
package slo

// State is the trust-aware outcome of an SLO evaluation.
//
//	healthy       -> telemetry is complete AND SLI >= target
//	unhealthy     -> telemetry is complete AND SLI <  target
//	indeterminate -> telemetry is incomplete, so the SLO cannot be trusted
type State string

const (
	StateHealthy       State = "healthy"
	StateUnhealthy     State = "unhealthy"
	StateIndeterminate State = "indeterminate"
)

// SLIType enumerates the supported service-level-indicator kinds.
type SLIType string

const (
	SLITypeRatio            SLIType = "ratio"
	SLITypeLatencyThreshold SLIType = "latency_threshold"
	SLITypeCompleteness     SLIType = "completeness"
	SLITypeGroundedAnswers  SLIType = "grounded_answers"
)

// Window is a duration string such as "7d" or "30d".
type Window string

// Report is the evaluated result of a single SLO.
type Report struct {
	Name                 string  `json:"name"`
	Service              string  `json:"service"`
	Type                 SLIType `json:"type"`
	Target               float64 `json:"target"`
	Window               Window  `json:"window"`
	SLI                  float64 `json:"sli"`
	State                State   `json:"state"`
	Completeness         float64 `json:"completeness"`
	ErrorBudgetRemaining float64 `json:"errorBudgetRemaining"`
	BurnRate             float64 `json:"burnRate"`
}
