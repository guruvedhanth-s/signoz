package answer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/slo"
)

// BurnTierResult is one multi-window burn tier as the answer reports it.
// The thresholds and windows are carried alongside the measured burn rates
// so a reply can say "3.6 against a threshold of 3 over 24h" without the
// composer having to look either number up - which it must not do,
// because looking one up means knowing one that was never computed.
type BurnTierResult struct {
	SLO         string  `json:"slo"`
	Tier        string  `json:"tier"`
	Severity    string  `json:"severity"`
	LongWindow  string  `json:"long_window"`
	ShortWindow string  `json:"short_window"`
	Threshold   float64 `json:"threshold"`
	LongBurn    float64 `json:"long_burn"`
	ShortBurn   float64 `json:"short_burn"`
	// Firing is the deterministic MWMB verdict: both windows at or above
	// the tier threshold, and neither window indeterminate.
	Firing bool `json:"firing"`
	// Indeterminate reports that at least one of the two windows could not
	// be evaluated with confidence, so Firing is meaningless for this tier
	// rather than false.
	Indeterminate bool `json:"indeterminate"`
}

// BurnRates is the answer to "what is my burn rate?".
type BurnRates struct {
	Service     string           `json:"service"`
	Environment string           `json:"environment"`
	Tiers       []BurnTierResult `json:"tiers"`
	// FiringTiers names the tiers currently firing, so the most
	// decision-relevant fact does not have to be re-derived by scanning
	// the list.
	FiringTiers []string `json:"firing_tiers,omitempty"`
}

// BurnRateTool answers the multi-window multi-burn-rate question: how fast
// is the error budget being consumed, per tier, and is any tier firing.
//
// Built on slo.Engine.EvaluateMultiWindow with slo.DefaultBurnTiers - the
// same fast/medium/slow tiers and the same 14.4/6/3 thresholds the alert
// rules use, so an answer in chat cannot disagree with a page.
func BurnRateTool(deps Deps) Tool[SLOArgs, BurnRates] {
	return NewTool("burn_rate",
		"Report the multi-window burn rate for a service's SLOs across the fast, medium and slow tiers, and whether any tier is firing. Use this for questions about how quickly the error budget is being consumed.",
		sloSchema,
		func(ctx context.Context, args SLOArgs) (Envelope[BurnRates], error) {
			if deps.SLOConfigs == nil {
				return indeterminate[BurnRates]("no SLO config source is configured"), nil
			}
			if deps.Metrics == nil {
				return Envelope[BurnRates]{}, fmt.Errorf("answer: metric querier is required")
			}
			cfg, err := deps.SLOConfigs.SLOConfig(ctx, args.Service, args.Environment)
			if err != nil {
				if errors.Is(err, ErrNoSLOConfig) {
					return indeterminate[BurnRates](fmt.Sprintf(
						"no SLO is defined for %s in %s, so there is no burn rate to report",
						args.Service, args.Environment)), nil
				}
				return Envelope[BurnRates]{}, err
			}
			scoped, ok := scopeConfig(cfg, args.SLO)
			if !ok {
				return indeterminate[BurnRates](fmt.Sprintf(
					"no SLO named %q is defined for %s in %s", args.SLO, args.Service, args.Environment)), nil
			}

			now := deps.now()
			tiers := slo.DefaultBurnTiers
			engine := slo.NewEngine(deps.Metrics, slo.NewMetricPresenceGate(deps.Metrics, nil))
			burns, err := engine.EvaluateMultiWindow(ctx, scoped, now, tiers)
			if err != nil {
				return Envelope[BurnRates]{}, err
			}

			byName := make(map[string]slo.BurnTier, len(tiers))
			for _, tier := range tiers {
				byName[tier.Name] = tier
			}
			results := make([]BurnTierResult, 0, len(burns))
			var firing []string
			allIndeterminate := len(burns) > 0
			for _, burn := range burns {
				tier := byName[burn.Tier]
				results = append(results, BurnTierResult{
					SLO: burn.SLO, Tier: burn.Tier, Severity: burn.Severity,
					LongWindow: tier.LongWindow, ShortWindow: tier.ShortWindow,
					Threshold: tier.Threshold,
					LongBurn:  burn.LongBurn, ShortBurn: burn.ShortBurn,
					Firing: burn.Firing, Indeterminate: burn.Indeterminate,
				})
				if burn.Firing {
					firing = append(firing, burn.SLO+"/"+burn.Tier)
				}
				if !burn.Indeterminate {
					allIndeterminate = false
				}
			}

			// The trust verdict is computed by running the same
			// completeness gate the SLO engine runs, over the longest tier
			// window - not inferred from the burn numbers. Inferring it
			// would mean this tool reporting a completeness verdict nobody
			// measured, which is the precise failure mode rule 2 exists to
			// stop.
			trust, err := gateTrust(ctx, deps, scoped, longestWindow(tiers), now)
			if err != nil {
				return Envelope[BurnRates]{}, err
			}

			result := Envelope[BurnRates]{
				Status: StatusOK,
				// Window is empty by design: this answer spans six windows
				// at once, and naming one of them would misdescribe it.
				// The per-tier long/short windows carry the real ranges.
				EvaluatedStart: now.Add(-longestWindow(tiers)),
				EvaluatedEnd:   now,
				Trust:          trust,
				Data: BurnRates{
					Service:     args.Service,
					Environment: args.Environment,
					Tiers:       results,
					FiringTiers: firing,
				},
			}
			if allIndeterminate {
				result.Status = StatusIndeterminate
				result.Reason = "every burn tier evaluated indeterminate, so no burn rate can be quoted"
				if trust != nil && trust.Reason != "" {
					result.Reason += ": " + trust.Reason
				}
			}
			return result, nil
		})
}

// scopeConfig narrows a config to a single named SLO. Returns false when
// the name matches nothing, which is an indeterminate answer rather than
// an error - the user asked about an SLO that does not exist, and saying
// so is more useful than failing.
func scopeConfig(cfg slo.Config, name string) (slo.Config, bool) {
	if name == "" {
		return cfg, len(cfg.SLOs) > 0
	}
	for _, definition := range cfg.SLOs {
		if definition.Name == name {
			scoped := cfg
			scoped.SLOs = []slo.Definition{definition}
			return scoped, true
		}
	}
	return slo.Config{}, false
}

func longestWindow(tiers []slo.BurnTier) time.Duration {
	longest := time.Duration(0)
	for _, tier := range tiers {
		if duration, err := slo.WindowDuration(tier.LongWindow); err == nil && duration > longest {
			longest = duration
		}
	}
	return longest
}

// gateTrust runs the completeness gate for every definition in cfg over
// the given window and reduces the results the same pessimistic way
// worstTrust does, applying each definition's own gate threshold exactly
// as slo.Engine.checkCompleteness does. This mirrors the engine rather
// than reimplementing a judgement: the threshold, the dependency list and
// the label overrides all come from the config.
func gateTrust(ctx context.Context, deps Deps, cfg slo.Config, window time.Duration, now time.Time) (*slo.GateResult, error) {
	gate := slo.NewMetricPresenceGate(deps.Metrics, nil)
	worst := slo.GateResult{Coverage: 1, QueryComplete: true, Trusted: true}
	var reasons, warnings []string
	evaluated := false
	for _, definition := range cfg.SLOs {
		if !definition.RequiresCompleteness && definition.Type != slo.SLITypeCompleteness {
			continue
		}
		dependencies := definition.Dependencies
		if len(dependencies) == 0 && cfg.Completeness != nil {
			dependencies = cfg.Completeness.ExpectedMetrics
		}
		if len(dependencies) == 0 {
			// Nothing to check for this definition. The engine treats this
			// as full coverage; so do we, rather than inventing a
			// pessimistic verdict the engine would not have produced.
			continue
		}
		serviceLabel, environmentLabel := cfg.MetricLabels()
		if trimmed := strings.TrimSpace(definition.ServiceLabel); trimmed != "" {
			serviceLabel = trimmed
		}
		if trimmed := strings.TrimSpace(definition.EnvironmentLabel); trimmed != "" {
			environmentLabel = trimmed
		}
		result, err := gate.Check(ctx, slo.GateRequest{
			Service:          cfg.Service,
			Environment:      cfg.Environment,
			Window:           window,
			Dependencies:     dependencies,
			ServiceLabel:     serviceLabel,
			EnvironmentLabel: environmentLabel,
			Now:              now,
		})
		if err != nil {
			return nil, err
		}
		result.Trusted = result.QueryComplete && result.Coverage >= cfg.GateThreshold(definition)
		evaluated = true
		if result.Coverage < worst.Coverage {
			worst.Coverage = result.Coverage
		}
		worst.QueryComplete = worst.QueryComplete && result.QueryComplete
		worst.Trusted = worst.Trusted && result.Trusted
		if result.Reason != "" {
			reasons = append(reasons, definition.Name+": "+result.Reason)
		}
		if result.Warning != "" {
			warnings = append(warnings, definition.Name+": "+result.Warning)
		}
	}
	if !evaluated {
		worst.Reason = "no completeness dependencies are configured for this service"
		return &worst, nil
	}
	worst.Reason = strings.Join(reasons, "; ")
	worst.Warning = strings.Join(warnings, "; ")
	return &worst, nil
}
