package implslo

import "github.com/SigNoz/signoz/pkg/types/slotypes"

// defaultGateThreshold is the minimum telemetry completeness (0..1) an SLO must
// have before its result is trusted. Below this, a gated SLO resolves to
// StateIndeterminate.
const defaultGateThreshold = 0.95

// resolveState is the trust-aware SLO state machine.
//
//	indeterminate -> the SLO requires completeness and telemetry is below the gate
//	healthy       -> telemetry is trustworthy and the SLI meets the target
//	unhealthy     -> telemetry is trustworthy and the SLI misses the target
//
// This is the single source of truth for SLO state and is kept pure so it can be
// exhaustively unit-tested.
func resolveState(sli, target, completeness, gateThreshold float64, requiresCompleteness bool) slotypes.State {
	if requiresCompleteness && completeness < gateThreshold {
		return slotypes.StateIndeterminate
	}
	if sli >= target {
		return slotypes.StateHealthy
	}
	return slotypes.StateUnhealthy
}
