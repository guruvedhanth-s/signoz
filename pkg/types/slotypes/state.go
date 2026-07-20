package slotypes

// State is the trust-aware outcome of an SLO evaluation.
//
// The three states are the heart of the SLO engine:
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

// Window is a duration string such as "7d" or "30d" that bounds an SLO evaluation.
type Window string
