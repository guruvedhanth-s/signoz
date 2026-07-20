package implslo

import (
	"testing"

	"github.com/SigNoz/signoz/pkg/types/slotypes"
)

func TestBurnRateTierFires(t *testing.T) {
	tier := BurnRateTier{Name: "fast", Threshold: 14.4, Severity: slotypes.BurnSeverityPage}

	tests := []struct {
		name string
		burn WindowBurn
		want bool
	}{
		{"both above", WindowBurn{LongBurn: 15, ShortBurn: 20}, true},
		{"only long above", WindowBurn{LongBurn: 15, ShortBurn: 2}, false},
		{"only short above", WindowBurn{LongBurn: 2, ShortBurn: 20}, false},
		{"both below", WindowBurn{LongBurn: 1, ShortBurn: 1}, false},
		{"exactly at threshold", WindowBurn{LongBurn: 14.4, ShortBurn: 14.4}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tier.Fires(tt.burn); got != tt.want {
				t.Fatalf("Fires(%+v) = %v, want %v", tt.burn, got, tt.want)
			}
		})
	}
}

func TestEvaluateBurnAlerts(t *testing.T) {
	burnByTier := map[string]WindowBurn{
		"fast":   {LongBurn: 20, ShortBurn: 20},  // fires -> page
		"medium": {LongBurn: 7, ShortBurn: 1},    // long only, no fire
		"slow":   {LongBurn: 4, ShortBurn: 3.5},  // fires -> ticket
	}

	alerts := EvaluateBurnAlerts(DefaultBurnRateTiers, burnByTier)
	if len(alerts) != 2 {
		t.Fatalf("expected 2 alerts, got %d: %+v", len(alerts), alerts)
	}
	if alerts[0].Tier != "fast" || alerts[0].Severity != slotypes.BurnSeverityPage {
		t.Fatalf("first alert = %+v, want fast/page", alerts[0])
	}
	if alerts[1].Tier != "slow" || alerts[1].Severity != slotypes.BurnSeverityTicket {
		t.Fatalf("second alert = %+v, want slow/ticket", alerts[1])
	}
}

func TestEvaluateBurnAlertsNoMeasurements(t *testing.T) {
	alerts := EvaluateBurnAlerts(DefaultBurnRateTiers, map[string]WindowBurn{})
	if len(alerts) != 0 {
		t.Fatalf("expected 0 alerts when no measurements, got %d", len(alerts))
	}
}
