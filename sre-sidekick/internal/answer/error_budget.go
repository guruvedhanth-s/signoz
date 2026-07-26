package answer

import (
	"context"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/slo"
)

// BudgetEntry is one SLO's error-budget position.
type BudgetEntry struct {
	SLO    string `json:"slo"`
	Window string `json:"window"`
	// Remaining is the fraction of the error budget left, as computed by
	// slo.RemainingBudget. It is negative when the budget is exhausted and
	// overspent, which is a meaningful, quotable number - not an error.
	Remaining      float64        `json:"remaining"`
	BurnRate       float64        `json:"burn_rate"`
	SLI            float64        `json:"sli"`
	Target         float64        `json:"target"`
	State          slo.State      `json:"state"`
	EvaluatedStart string         `json:"evaluated_start,omitempty"`
	EvaluatedEnd   string         `json:"evaluated_end,omitempty"`
	Trust          slo.GateResult `json:"trust"`
}

// ErrorBudget is the answer to "how much error budget is left?".
type ErrorBudget struct {
	Service     string        `json:"service"`
	Environment string        `json:"environment"`
	Budgets     []BudgetEntry `json:"budgets"`
	// Exhausted names the SLOs whose budget is at or below zero.
	Exhausted []string `json:"exhausted,omitempty"`
}

// ErrorBudgetTool answers how much error budget remains, over which
// window, for which SLOs.
//
// It shares evaluateFiltered with slo_status rather than issuing its own
// engine call, so "what's my budget?" and "how am I doing?" asked one
// after the other return the same numbers - reinforced by the result cache
// when both land inside the same TTL.
func ErrorBudgetTool(deps Deps) Tool[SLOArgs, ErrorBudget] {
	return NewTool("error_budget",
		"Report the remaining error budget for a service's SLOs, the window it covers, and the evaluated time range. Use this for questions about budget left, budget burn or budget exhaustion.",
		sloSchema,
		func(ctx context.Context, args SLOArgs) (Envelope[ErrorBudget], error) {
			reports, env, err := evaluateFiltered[ErrorBudget](ctx, deps, args)
			if err != nil {
				return Envelope[ErrorBudget]{}, err
			}
			if env != nil {
				return *env, nil
			}

			entries := make([]BudgetEntry, 0, len(reports))
			var exhausted []string
			for _, report := range reports {
				entry := BudgetEntry{
					SLO:       report.Name,
					Window:    report.Window,
					Remaining: report.ErrorBudgetRemaining,
					BurnRate:  report.BurnRate,
					SLI:       report.SLI,
					Target:    report.Target,
					State:     report.State,
					Trust:     report.Gate,
				}
				if !report.EvaluatedStart.IsZero() {
					entry.EvaluatedStart = report.EvaluatedStart.UTC().Format("2006-01-02T15:04:05Z")
				}
				if !report.EvaluatedEnd.IsZero() {
					entry.EvaluatedEnd = report.EvaluatedEnd.UTC().Format("2006-01-02T15:04:05Z")
				}
				// Only a trusted, determinate report can be called
				// exhausted. An indeterminate SLO has no budget position
				// at all, and listing it here would turn "we don't know"
				// into "it's gone".
				if report.Gate.Trusted && report.State != slo.StateIndeterminate && report.ErrorBudgetRemaining <= 0 {
					exhausted = append(exhausted, report.Name)
				}
				entries = append(entries, entry)
			}

			result := Envelope[ErrorBudget]{
				Status: StatusOK,
				Trust:  worstTrust(reports),
				Data: ErrorBudget{
					Service:     args.Service,
					Environment: args.Environment,
					Budgets:     entries,
					Exhausted:   exhausted,
				},
			}
			provenance(&result, reports)
			if !anyTrusted(reports) {
				result.Status = StatusIndeterminate
				result.Reason = untrustedReason(reports)
			}
			return result, nil
		})
}
