package rca

import (
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/slo"
)

// GroundingFromReport maps a deterministic slo.Report - computed entirely
// by the SLO engine and completeness gate, never by the RCA agent or its
// reasoner - into the notify.Grounding an Incident carries and Render
// copies verbatim into the final Diagnosis. This is a field-by-field copy,
// not a re-derivation: the RCA agent and its reasoner must never recompute
// or alter any of these facts (PRD section 7, section 13).
//
// report.Gate.Trusted is the authoritative telemetry-trust signal. It is
// false exactly when the SLO engine could not evaluate the SLO with
// confidence, including the indeterminate case: Engine.evaluate sets
// Gate.Trusted to false whenever it returns a slo.StateIndeterminate
// report (see internal/slo/engine.go), so GroundingFromReport does not
// need to special-case State itself to get TelemetryTrusted right.
func GroundingFromReport(environment string, report slo.Report) notify.Grounding {
	return notify.Grounding{
		Environment:          environment,
		SLO:                  report.Name,
		SLOState:             string(report.State),
		BurnRate:             report.BurnRate,
		ErrorBudgetRemaining: report.ErrorBudgetRemaining,
		TelemetryTrusted:     report.Gate.Trusted,
	}
}

// SelectReport picks the report named name from reports. If name is empty,
// or no report matches it, the first report is returned instead - most SLO
// config files scope one incident to a single primary SLO, so falling back
// to "the only one" (or "the first one") is a reasonable default rather
// than an error. Returns false only when reports is empty.
func SelectReport(reports []slo.Report, name string) (slo.Report, bool) {
	if len(reports) == 0 {
		return slo.Report{}, false
	}
	if name != "" {
		for _, report := range reports {
			if report.Name == name {
				return report, true
			}
		}
	}
	return reports[0], true
}
