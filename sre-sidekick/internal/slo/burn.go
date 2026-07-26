package slo

import (
	"context"
	"fmt"
	"time"
)

type MultiWindowBurn struct {
	Service       string  `json:"service"`
	Environment   string  `json:"environment"`
	SLO           string  `json:"slo"`
	Tier          string  `json:"tier"`
	Severity      string  `json:"severity"`
	LongBurn      float64 `json:"long_burn"`
	ShortBurn     float64 `json:"short_burn"`
	Firing        bool    `json:"firing"`
	Indeterminate bool    `json:"indeterminate"`
}

// EvaluateMultiWindow evaluates every burn tier's long and short window for
// each configured SLO. Builder queries carry no window in the query text
// itself - the window only ever shows up as the [start,end] range passed
// to ScalarBuilder - so switching windows here is just a matter of
// re-running Engine.evaluate with definition.Window set to the tier's
// window; there is no query text to rewrite.
func (e *Engine) EvaluateMultiWindow(ctx context.Context, cfg Config, now time.Time, tiers []BurnTier) ([]MultiWindowBurn, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if e.Metrics == nil {
		return nil, fmt.Errorf("SLO metric querier is required")
	}
	results := make([]MultiWindowBurn, 0, len(cfg.SLOs)*len(tiers))
	for _, definition := range cfg.SLOs {
		for _, tier := range tiers {
			long := definition
			long.Window = tier.LongWindow
			short := definition
			short.Window = tier.ShortWindow
			longReport := e.evaluate(ctx, cfg, long, now)
			shortReport := e.evaluate(ctx, cfg, short, now)
			result := MultiWindowBurn{
				Service: cfg.Service, Environment: cfg.Environment, SLO: definition.Name,
				Tier: tier.Name, Severity: tier.Severity,
				LongBurn: longReport.BurnRate, ShortBurn: shortReport.BurnRate,
			}
			result.Indeterminate = longReport.State == StateIndeterminate || shortReport.State == StateIndeterminate
			result.Firing = !result.Indeterminate && result.LongBurn >= tier.Threshold && result.ShortBurn >= tier.Threshold
			results = append(results, result)
		}
	}
	return results, nil
}
