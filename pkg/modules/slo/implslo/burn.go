package implslo

import (
	"github.com/SigNoz/signoz/pkg/types/slotypes"
)

// BurnRateTier is one rung of the Google SRE multi-window multi-burn-rate
// alerting ladder. An alert fires only when the burn rate exceeds Threshold over
// BOTH the long and the short window, which balances fast detection against
// false positives.
type BurnRateTier struct {
	Name        string
	LongWindow  slotypes.Window
	ShortWindow slotypes.Window
	Threshold   float64
	Severity    slotypes.BurnSeverity
}

// DefaultBurnRateTiers is the standard 3-tier ladder tuned for a 30-day SLO
// window. Fast burn pages; medium and slow burn open tickets.
var DefaultBurnRateTiers = []BurnRateTier{
	{Name: "fast", LongWindow: "1h", ShortWindow: "5m", Threshold: 14.4, Severity: slotypes.BurnSeverityPage},
	{Name: "medium", LongWindow: "6h", ShortWindow: "30m", Threshold: 6, Severity: slotypes.BurnSeverityTicket},
	{Name: "slow", LongWindow: "24h", ShortWindow: "2h", Threshold: 3, Severity: slotypes.BurnSeverityTicket},
}

// WindowBurn holds the burn rate measured over a tier's long and short windows.
type WindowBurn struct {
	LongBurn  float64
	ShortBurn float64
}

// Fires reports whether this tier's alert condition is met: the burn rate must
// exceed the threshold over both windows.
func (t BurnRateTier) Fires(b WindowBurn) bool {
	return b.LongBurn >= t.Threshold && b.ShortBurn >= t.Threshold
}

// BurnAlert is a tier that has fired for a given SLO.
type BurnAlert struct {
	Tier     string                `json:"tier"`
	Severity slotypes.BurnSeverity `json:"severity"`
	Burn     WindowBurn            `json:"-"`
}

// EvaluateBurnAlerts returns every tier that is currently firing, given the
// measured burn per tier keyed by tier name. Tiers with no measurement are
// skipped. The result is ordered to match tiers.
func EvaluateBurnAlerts(tiers []BurnRateTier, burnByTier map[string]WindowBurn) []BurnAlert {
	alerts := make([]BurnAlert, 0, len(tiers))
	for _, tier := range tiers {
		burn, ok := burnByTier[tier.Name]
		if !ok {
			continue
		}
		if tier.Fires(burn) {
			alerts = append(alerts, BurnAlert{Tier: tier.Name, Severity: tier.Severity, Burn: burn})
		}
	}
	return alerts
}
