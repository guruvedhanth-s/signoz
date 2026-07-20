package slotypes

// Report is the evaluated result of a single SLO for a service and window.
//
// M0 note: fields are populated by later milestones (SLI evaluation, budget and
// burn-rate math). The shape is frozen here so the API contract and the frontend
// can be built in parallel.
type Report struct {
	Name    string  `json:"name"`
	Service string  `json:"service"`
	Type    SLIType `json:"type"`
	Target  float64 `json:"target"`
	Window  Window  `json:"window"`

	// SLI is the measured indicator value in the range 0..1. Valid only when State
	// is healthy or unhealthy.
	SLI float64 `json:"sli"`

	// State is the trust-aware outcome. See State for semantics.
	State State `json:"state"`

	// Completeness is the auditor-provided telemetry completeness (0..1) used to
	// decide the indeterminate state.
	Completeness float64 `json:"completeness"`

	// ErrorBudgetRemaining is the remaining error budget as a fraction (0..1) of the
	// total allowed budget. Valid only when State is healthy or unhealthy.
	ErrorBudgetRemaining float64 `json:"errorBudgetRemaining"`

	// BurnRate is the current error-budget burn rate. 1.0 exactly exhausts the
	// budget by the end of the window.
	BurnRate float64 `json:"burnRate"`
}
