package slo

// BurnSeverity classifies how a burn-rate alert should be routed.
type BurnSeverity string

const (
	BurnSeverityPage   BurnSeverity = "page"
	BurnSeverityTicket BurnSeverity = "ticket"
)

// BurnRateTier is one rung of the Google SRE multi-window multi-burn-rate ladder.
// An alert fires only when the burn rate exceeds Threshold over BOTH windows.
type BurnRateTier struct {
	Name        string
	LongWindow  Window
	ShortWindow Window
	Threshold   float64
	Severity    BurnSeverity
}

// DefaultBurnRateTiers is the standard 3-tier ladder tuned for a 30-day SLO.
var DefaultBurnRateTiers = []BurnRateTier{
	{Name: "fast", LongWindow: "1h", ShortWindow: "5m", Threshold: 14.4, Severity: BurnSeverityPage},
	{Name: "medium", LongWindow: "6h", ShortWindow: "30m", Threshold: 6, Severity: BurnSeverityTicket},
	{Name: "slow", LongWindow: "24h", ShortWindow: "2h", Threshold: 3, Severity: BurnSeverityTicket},
}

// WindowBurn holds the burn rate over a tier's long and short windows.
type WindowBurn struct {
	LongBurn  float64
	ShortBurn float64
}

// Fires reports whether the tier's alert condition is met.
func (t BurnRateTier) Fires(b WindowBurn) bool {
	return b.LongBurn >= t.Threshold && b.ShortBurn >= t.Threshold
}

// BurnAlert is a tier that has fired for a given SLO.
type BurnAlert struct {
	Tier     string       `json:"tier"`
	Severity BurnSeverity `json:"severity"`
	Burn     WindowBurn   `json:"-"`
}

// EvaluateBurnAlerts returns every tier currently firing.
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
