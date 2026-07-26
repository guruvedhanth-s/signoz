package answer

import (
	"context"
	"fmt"
	"sort"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/audit"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/slo"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/source"
)

// TrustFinding is one Track A audit finding, summarised. The full
// evidence map is deliberately dropped: it contains raw telemetry values
// (log bodies, span attributes) which are attacker-controllable, and this
// struct is destined for a model's context. The rule ID, counts and
// recommendation are enough to explain the verdict, and they are ours.
type TrustFinding struct {
	RuleID         string       `json:"rule_id"`
	Status         audit.Status `json:"status"`
	Severity       string       `json:"severity"`
	Signal         string       `json:"signal"`
	AffectedCount  int          `json:"affected_count"`
	Recommendation string       `json:"recommendation,omitempty"`
}

// TelemetryTrust is the answer to "can I trust what I'm seeing?".
type TelemetryTrust struct {
	Service     string  `json:"service"`
	Environment string  `json:"environment"`
	Profile     string  `json:"profile"`
	Score       float64 `json:"score"`
	Coverage    float64 `json:"coverage"`
	// QueryComplete is SigNoz's own completeness metadata for the audit
	// queries, passed through rather than re-derived.
	QueryComplete bool         `json:"query_complete"`
	OverallStatus audit.Status `json:"overall_status"`
	// CountsBySeverity is the audit's own severity tally (blocker,
	// warning, info).
	CountsBySeverity map[string]int `json:"counts_by_severity"`
	// FailedFindings lists only the findings that actually failed, sorted
	// blocker-first. A passing audit produces an empty list, not a wall of
	// passes nobody asked about.
	FailedFindings []TrustFinding `json:"failed_findings,omitempty"`
}

// severityRank orders findings by how much they should worry the reader.
var severityRank = map[string]int{"blocker": 0, "warning": 1, "info": 2}

// TelemetryTrustTool answers whether the telemetry behind every other
// answer is trustworthy: the Track A audit score, coverage, and the
// findings that failed.
//
// The "no profile registered" case is handled explicitly, as the issue
// requires: it returns a clear indeterminate result naming the missing
// profile rather than a vague failure, because the remedy ("register a
// profile for this service") is actionable and worth stating.
func TelemetryTrustTool(deps Deps) Tool[ServiceArgs, TelemetryTrust] {
	return NewTool("telemetry_trust",
		"Report the Track A telemetry audit for a service: instrumentation score, coverage, overall status and the findings that failed. Use this for questions about whether the data can be trusted, or why an answer was indeterminate.",
		serviceSchema,
		func(ctx context.Context, args ServiceArgs) (Envelope[TelemetryTrust], error) {
			if deps.Profiles == nil {
				return indeterminate[TelemetryTrust]("no profile registry is configured, so telemetry cannot be audited"), nil
			}
			p, err := deps.Profiles.Active(args.Service, args.Environment)
			if err != nil {
				return indeterminate[TelemetryTrust](fmt.Sprintf(
					"no telemetry profile is registered for %s in %s, so there is nothing to audit against; "+
						"register a profile for this service to get a trust verdict",
					args.Service, args.Environment)), nil
			}
			if deps.Telemetry == nil {
				return Envelope[TelemetryTrust]{}, fmt.Errorf("answer: telemetry source is required for telemetry_trust")
			}

			now := deps.now()
			lookback := deps.auditLookback()
			start := now.Add(-lookback)
			snapshot, err := deps.Telemetry.Snapshot(ctx, p, source.Target{
				Service:     args.Service,
				Environment: args.Environment,
				Start:       start,
				End:         now,
			})
			if err != nil {
				return Envelope[TelemetryTrust]{}, err
			}
			report, err := (audit.Engine{}).Run(p, snapshot, now)
			if err != nil {
				return Envelope[TelemetryTrust]{}, err
			}

			failed := make([]TrustFinding, 0, len(report.Findings))
			for _, finding := range report.Findings {
				if finding.Status != audit.Fail && finding.Status != audit.Indeterminate {
					continue
				}
				failed = append(failed, TrustFinding{
					RuleID:         finding.RuleID,
					Status:         finding.Status,
					Severity:       finding.Severity,
					Signal:         finding.Signal,
					AffectedCount:  finding.AffectedCount,
					Recommendation: finding.Recommendation,
				})
			}
			sort.SliceStable(failed, func(i, j int) bool {
				return severityRank[failed[i].Severity] < severityRank[failed[j].Severity]
			})

			// Trust here is derived entirely from the audit's own numbers -
			// coverage and query completeness are the audit's, and Trusted
			// tracks its overall status. Nothing is invented to fill the
			// shape of a slo.GateResult.
			trust := &slo.GateResult{
				Coverage:      report.Coverage,
				QueryComplete: report.QueryComplete,
				Trusted:       report.OverallStatus == audit.Pass,
			}

			result := Envelope[TelemetryTrust]{
				Status:         StatusOK,
				Window:         formatWindow(lookback),
				EvaluatedStart: start,
				EvaluatedEnd:   now,
				Trust:          trust,
				Data: TelemetryTrust{
					Service:          args.Service,
					Environment:      args.Environment,
					Profile:          p.Metadata.Name,
					Score:            report.Score,
					Coverage:         report.Coverage,
					QueryComplete:    report.QueryComplete,
					OverallStatus:    report.OverallStatus,
					CountsBySeverity: report.Counts,
					FailedFindings:   failed,
				},
			}
			// An indeterminate audit is an indeterminate answer. Note the
			// asymmetry with audit.Fail: a failing audit is a *computed*
			// verdict about bad instrumentation and can be quoted with
			// confidence, so it stays StatusOK. Only "the audit could not
			// tell" becomes StatusIndeterminate.
			if report.OverallStatus == audit.Indeterminate {
				result.Status = StatusIndeterminate
				result.Reason = fmt.Sprintf(
					"the telemetry audit for %s in %s was indeterminate (query_complete=%t, coverage=%.2f); "+
						"the underlying queries did not return enough data to judge",
					args.Service, args.Environment, report.QueryComplete, report.Coverage)
			}
			return result, nil
		})
}
