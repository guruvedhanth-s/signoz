package slo

// DefaultGateThreshold is the minimum telemetry completeness (0..1) an SLO must
// have before its result is trusted.
const DefaultGateThreshold = 0.95

// resolveState is the trust-aware SLO state machine and the single source of
// truth for SLO state.
//
//	indeterminate -> requires completeness and telemetry is below the gate
//	healthy       -> telemetry is trustworthy and the SLI meets the target
//	unhealthy     -> telemetry is trustworthy and the SLI misses the target
func resolveState(sli, target, completeness, gateThreshold float64, requiresCompleteness bool) State {
	if requiresCompleteness && completeness < gateThreshold {
		return StateIndeterminate
	}
	if sli >= target {
		return StateHealthy
	}
	return StateUnhealthy
}
