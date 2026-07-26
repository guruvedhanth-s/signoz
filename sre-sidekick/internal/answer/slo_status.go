package answer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/rca"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/slo"
)

// SLOState is one SLO's deterministic state. Every field here was computed
// by the SLO engine and the completeness gate; this struct is a
// field-by-field copy of slo.Report, never a re-derivation, for the same
// reason rca.GroundingFromReport is one (PRD section 7, section 13).
type SLOState struct {
	Name                 string      `json:"name"`
	Type                 slo.SLIType `json:"type"`
	State                slo.State   `json:"state"`
	SLI                  float64     `json:"sli"`
	Target               float64     `json:"target"`
	ErrorBudgetRemaining float64     `json:"error_budget_remaining"`
	BurnRate             float64     `json:"burn_rate"`
	Completeness         float64     `json:"completeness"`
	Window               string      `json:"window"`
	// EvaluatedStart/End are per-SLO because a config may mix windows, in
	// which case the Envelope's own range spans them all and its Window is
	// empty. An answer about a specific SLO should cite these.
	EvaluatedStart *time.Time     `json:"evaluated_start,omitempty"`
	EvaluatedEnd   *time.Time     `json:"evaluated_end,omitempty"`
	Trust          slo.GateResult `json:"trust"`
	Warning        string         `json:"warning,omitempty"`
	Error          string         `json:"error,omitempty"`
}

// SLOStatus is the answer to "how is my application doing?".
type SLOStatus struct {
	Service     string     `json:"service"`
	Environment string     `json:"environment"`
	SLOs        []SLOState `json:"slos"`
}

// SLOStatusTool answers the state of every SLO for a service: state, SLI,
// target, error budget, burn rate and the trust verdict behind them.
//
// It is built on rca.EvaluateReports, which is the same engine invocation
// the incident path grounds diagnoses with - so a number quoted in a chat
// reply and the same number quoted in an incident thread came from one
// code path, not two implementations that might drift.
func SLOStatusTool(deps Deps) Tool[SLOArgs, SLOStatus] {
	return NewTool("slo_status",
		"Report every SLO for a service: state (healthy/unhealthy/indeterminate), SLI, target, error budget remaining, burn rate, and whether the telemetry behind them is trusted. Use this for general questions like \"how is my service doing?\".",
		sloSchema,
		func(ctx context.Context, args SLOArgs) (Envelope[SLOStatus], error) {
			reports, env, err := evaluateFiltered[SLOStatus](ctx, deps, args)
			if err != nil {
				return Envelope[SLOStatus]{}, err
			}
			if env != nil {
				return *env, nil
			}

			states := make([]SLOState, 0, len(reports))
			for _, report := range reports {
				states = append(states, sloStateFromReport(report))
			}
			result := Envelope[SLOStatus]{
				Status: StatusOK,
				Trust:  worstTrust(reports),
				Data: SLOStatus{
					Service:     args.Service,
					Environment: args.Environment,
					SLOs:        states,
				},
			}
			provenance(&result, reports)
			// Rule 3: if the gate trusted nothing, the answer is
			// indeterminate - but the per-SLO detail is still returned, so
			// the reply can explain precisely what was untrusted rather
			// than going quiet. The numbers stay attached to a verdict
			// that forbids quoting them as fact.
			if result.Trust == nil || !result.Trust.Trusted {
				result.Status = StatusIndeterminate
				result.Reason = untrustedReason(reports)
			}
			return result, nil
		})
}

func sloStateFromReport(report slo.Report) SLOState {
	state := SLOState{
		Name:                 report.Name,
		Type:                 report.Type,
		State:                report.State,
		SLI:                  report.SLI,
		Target:               report.Target,
		ErrorBudgetRemaining: report.ErrorBudgetRemaining,
		BurnRate:             report.BurnRate,
		Completeness:         report.Completeness,
		Window:               report.Window,
		Trust:                report.Gate,
		Warning:              report.Warning,
		Error:                report.Error,
	}
	if !report.EvaluatedStart.IsZero() {
		start := report.EvaluatedStart.UTC()
		state.EvaluatedStart = &start
	}
	if !report.EvaluatedEnd.IsZero() {
		end := report.EvaluatedEnd.UTC()
		state.EvaluatedEnd = &end
	}
	return state
}

func anyTrusted(reports []slo.Report) bool {
	for _, report := range reports {
		if report.Gate.Trusted {
			return true
		}
	}
	return false
}

// evaluateFiltered is the shared path for slo_status and error_budget:
// resolve the config, run the engine, narrow to the requested SLO.
//
// It returns either reports (proceed) or a ready-made indeterminate
// envelope (stop, and say why) - never both, and never an error for a
// condition a human would consider an answer. A missing SLO config, a
// service with no matching SLO and untrusted telemetry are all
// indeterminate results, not failures; only a genuinely broken dependency
// (an unreadable config, a backend that errored) is an error.
func evaluateFiltered[T any](ctx context.Context, deps Deps, args SLOArgs) ([]slo.Report, *Envelope[T], error) {
	if deps.SLOConfigs == nil {
		env := indeterminate[T]("no SLO config source is configured")
		return nil, &env, nil
	}
	if deps.Metrics == nil {
		return nil, nil, fmt.Errorf("answer: metric querier is required")
	}
	cfg, err := deps.SLOConfigs.SLOConfig(ctx, args.Service, args.Environment)
	if err != nil {
		if errors.Is(err, ErrNoSLOConfig) {
			env := indeterminate[T](fmt.Sprintf(
				"no SLO is defined for %s in %s, so there is no target, error budget or burn rate to report",
				args.Service, args.Environment))
			return nil, &env, nil
		}
		return nil, nil, err
	}
	reports, err := rca.EvaluateConfig(ctx, deps.Metrics, cfg, deps.now())
	if err != nil {
		return nil, nil, err
	}
	filtered := make([]slo.Report, 0, len(reports))
	for _, report := range reports {
		if matchesSLO(args.SLO, report.Name) {
			filtered = append(filtered, report)
		}
	}
	if len(filtered) == 0 {
		env := indeterminate[T](fmt.Sprintf(
			"no SLO named %q is defined for %s in %s", args.SLO, args.Service, args.Environment))
		return nil, &env, nil
	}
	return filtered, nil, nil
}
