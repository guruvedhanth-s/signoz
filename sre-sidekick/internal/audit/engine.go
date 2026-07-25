package audit

import (
	"fmt"
	"strings"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/evidence"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/profile"
)

// deepLinkWindow is the cosmetic time range shown in a finding's SigNoz
// deep link. It is independent of whatever window the caller actually
// queried the snapshot over - the deep link exists so a human can open the
// explorer and look around, not to reproduce the exact query.
const deepLinkWindow = 15 * time.Minute

type Engine struct{}

func (e Engine) Run(p profile.Profile, snapshot evidence.Snapshot, now time.Time) (Report, error) {
	if err := p.Validate(); err != nil {
		return Report{}, err
	}

	report := Report{
		SchemaVersion: "1.0",
		Profile:       p.Metadata.Name,
		Service:       p.Metadata.Service,
		Environment:   p.Metadata.Environment,
		QueryComplete: snapshot.Complete(),
		Counts:        map[string]int{"blocker": 0, "warning": 0, "info": 0},
	}
	for _, rule := range p.Spec.AuditRules {
		finding := evaluate(rule, snapshot, now)
		e.attachDeepLink(&finding, rule, p, now)
		report.Findings = append(report.Findings, finding)
		if finding.Status == Fail {
			report.Counts[finding.Severity]++
		}
	}

	applicable, complete := 0, 0
	for _, finding := range report.Findings {
		if finding.Status == NotApplicable {
			continue
		}
		applicable++
		if finding.Status != Indeterminate {
			complete++
		}
	}
	if applicable > 0 {
		report.Coverage = float64(complete) / float64(applicable)
	} else if snapshot.Complete() {
		report.Coverage = 1
	}
	report.Score = score(report.Findings)
	report.OverallStatus = overallStatus(report.Findings, snapshot.Complete())
	return report, nil
}

// attachDeepLink populates f.Evidence with a SigNoz explorer link scoped to
// the rule's own signal and the profile's service/environment, so a
// finding can be clicked through to instead of only read as text (PRD
// section 8). Skipped when the rule has no signal (nothing to scope an
// explorer view to) or the profile names no reachable SigNoz endpoint.
func (Engine) attachDeepLink(f *Finding, rule profile.RuleSpec, p profile.Profile, now time.Time) {
	if rule.Signal == "" || p.Spec.Source.Endpoint == "" {
		return
	}
	link := BuildDeepLink(
		p.Spec.Source.Endpoint,
		rule.Signal,
		ScopeFilter(p.Metadata.Service, p.Metadata.Environment),
		now.Add(-deepLinkWindow),
		now,
	)
	if link == "" {
		return
	}
	if f.Evidence == nil {
		f.Evidence = map[string]any{}
	}
	f.Evidence["signoz_link"] = link
}

func evaluate(rule profile.RuleSpec, snapshot evidence.Snapshot, now time.Time) Finding {
	f := Finding{
		RuleID:         rule.ID,
		RuleVersion:    "1.0",
		Status:         Indeterminate,
		Severity:       rule.Severity,
		Signal:         rule.Signal,
		Recommendation: rule.Recommendation,
		Evidence:       map[string]any{},
	}
	if f.Recommendation == "" {
		f.Recommendation = "Review the telemetry profile and instrumentation for this rule."
	}
	if rule.Signal != "" && !snapshot.SignalAvailable(rule.Signal) {
		f.Status = NotApplicable
		f.Observed = fmt.Sprintf("%s evidence is not available from the configured source", rule.Signal)
		return f
	}

	switch rule.Type {
	case "required_field":
		return requiredField(rule, snapshot, f)
	case "required_span":
		return requiredSpan(rule, snapshot, f)
	case "freshness":
		return freshness(rule, snapshot, f, now)
	case "cardinality":
		return cardinality(rule, snapshot, f)
	default:
		f.Observed = fmt.Sprintf("unsupported rule type %q", rule.Type)
		return f
	}
}

func requiredField(rule profile.RuleSpec, snapshot evidence.Snapshot, f Finding) Finding {
	records := matchingRecords(rule, snapshot)
	if len(records) == 0 {
		f.Observed = "no matching records were returned"
		return f
	}
	missing := 0
	for _, record := range records {
		if empty(record.Fields[rule.Field]) {
			missing++
		}
	}
	f.AffectedCount = missing
	f.Expected = fmt.Sprintf("Every matching record has %s", rule.Field)
	if missing == 0 {
		f.Status = Pass
		f.Observed = fmt.Sprintf("all %d matching records contain %s", len(records), rule.Field)
	} else {
		f.Status = Fail
		f.Observed = fmt.Sprintf("%d of %d records lack %s", missing, len(records), rule.Field)
	}
	return f
}

func requiredSpan(rule profile.RuleSpec, snapshot evidence.Snapshot, f Finding) Finding {
	for _, record := range snapshot.Traces {
		if record.Selector == rule.SpanName {
			f.Status = Pass
			f.Observed = fmt.Sprintf("span %s was found", rule.SpanName)
			f.Expected = fmt.Sprintf("span %s exists", rule.SpanName)
			return f
		}
	}
	f.Status = Fail
	f.AffectedCount = 1
	f.Observed = fmt.Sprintf("span %s was not found", rule.SpanName)
	f.Expected = fmt.Sprintf("span %s exists", rule.SpanName)
	return f
}

func freshness(rule profile.RuleSpec, snapshot evidence.Snapshot, f Finding, now time.Time) Finding {
	lastSeen, ok := snapshot.LastSeen[rule.Field]
	if !ok || lastSeen.IsZero() {
		f.Observed = fmt.Sprintf("no last-seen timestamp for %s", rule.Field)
		return f
	}
	maxAge, err := time.ParseDuration(rule.MaxAge)
	if err != nil {
		f.Observed = err.Error()
		return f
	}
	age := now.Sub(lastSeen)
	f.Expected = fmt.Sprintf("last activity is newer than %s", rule.MaxAge)
	f.Observed = fmt.Sprintf("last activity was %s ago", age.Round(time.Second))
	if age <= maxAge {
		f.Status = Pass
	} else {
		f.Status = Fail
		f.AffectedCount = 1
	}
	return f
}

func cardinality(rule profile.RuleSpec, snapshot evidence.Snapshot, f Finding) Finding {
	distinct, ok := snapshot.DistinctValues[rule.Field]
	if !ok {
		f.Observed = fmt.Sprintf("no cardinality data for %s", rule.Field)
		return f
	}
	f.Expected = fmt.Sprintf("%s has at most %d distinct values", rule.Field, rule.MaxDistinctValues)
	f.Observed = fmt.Sprintf("%s has %d distinct values", rule.Field, distinct)
	if distinct <= rule.MaxDistinctValues {
		f.Status = Pass
	} else {
		f.Status = Fail
		f.AffectedCount = distinct - rule.MaxDistinctValues
	}
	return f
}

func matchingRecords(rule profile.RuleSpec, snapshot evidence.Snapshot) []evidence.Record {
	records := snapshot.Records(rule.Signal)
	if rule.AppliesTo == "" {
		return records
	}
	matched := make([]evidence.Record, 0)
	for _, record := range records {
		if record.Selector == rule.AppliesTo {
			matched = append(matched, record)
		}
	}
	return matched
}

func empty(value any) bool {
	if value == nil {
		return true
	}
	if s, ok := value.(string); ok {
		return strings.TrimSpace(s) == ""
	}
	return false
}

func overallStatus(findings []Finding, queryComplete bool) Status {
	for _, finding := range findings {
		if finding.Status == Indeterminate {
			return Indeterminate
		}
	}
	if !queryComplete {
		return Indeterminate
	}
	for _, finding := range findings {
		if finding.Status == Fail {
			return Fail
		}
	}
	return Pass
}
