package answer

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/audit"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/slo"
)

// Fact is one already-formatted value, ready to be read aloud. The Value
// is a string on purpose: by the time anything reaches the composer, the
// decision of how many decimal places a burn rate has must already be
// made. Handing a model 3.6363636363636322 and hoping for "3.6x" gives it
// both a rounding decision and an opportunity to restate the number
// differently; handing it "3.6x" gives it neither.
type Fact struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// Facts is the complete composer input for one answer: pre-formatted
// values, the provenance that must always be stated, and a verdict the
// rules decided. There is no float anywhere in this struct, and free-form
// text is either our own wording or bounded identifiers/provenance.
type Facts struct {
	Intent      string `json:"intent"`
	Service     string `json:"service,omitempty"`
	Environment string `json:"environment,omitempty"`
	// Headline is the verdict, decided by rules over the data - never by
	// the model, and never by the model's confidence. It leads the answer.
	Headline string `json:"headline"`
	// Window and EvaluatedRange are the provenance every answer must name.
	// An answer that states a burn rate without saying what it covers is
	// misleading even when the number is right.
	Window         string `json:"window,omitempty"`
	EvaluatedRange string `json:"evaluated_range,omitempty"`
	// Values are the quotable figures. Empty for an indeterminate answer:
	// if the telemetry was not trusted, there is nothing to quote, and
	// offering numbers alongside a refusal invites them to be read anyway.
	Values []Fact `json:"values,omitempty"`
	// Caveat is trust context in our own words. It never carries text
	// authored outside this codebase: a SigNoz query-completeness warning
	// becomes a fixed sentence of ours rather than the warning's own
	// string, because any text that reaches a model is text a model may
	// obey, and that string is influenced by the systems being monitored.
	// The full warning stays on the typed Envelope for logs and Slack
	// context blocks; it just never enters a prompt.
	Caveat string `json:"caveat,omitempty"`
	// Indeterminate marks a refusal. The composer must state it plainly
	// and must not soften it into a guess.
	Indeterminate bool `json:"indeterminate"`
	// Reason explains a refusal, in the engine's own words.
	Reason string `json:"reason,omitempty"`
}

// AnyEnvelope is the type-erased view of an Envelope, carrying the same
// provenance with the payload as `any`. It exists so the composer can work
// from a result that came back through Registry.Invoke, where the static
// type was lost at the LLM edge.
type AnyEnvelope struct {
	Intent         string
	Status         Status
	Reason         string
	Window         string
	EvaluatedStart time.Time
	EvaluatedEnd   time.Time
	Trust          *slo.GateResult
	Data           any
}

// Erasable is implemented by every Envelope[T]. Registry.Invoke returns
// `any`; asserting to this interface recovers the provenance without
// knowing T.
type Erasable interface {
	Erase() AnyEnvelope
}

// Erase implements Erasable.
func (e Envelope[T]) Erase() AnyEnvelope {
	return AnyEnvelope{
		Intent:         e.Intent,
		Status:         e.Status,
		Reason:         e.Reason,
		Window:         e.Window,
		EvaluatedStart: e.EvaluatedStart,
		EvaluatedEnd:   e.EvaluatedEnd,
		Trust:          e.Trust,
		Data:           e.Data,
	}
}

// FactsFrom converts a typed envelope into composer input. Call it
// directly from a typed call site; call FactsFromAny for a result that
// came back through the registry.
func FactsFrom[T any](env Envelope[T]) Facts {
	return FactsFromAny(env.Erase())
}

// FactsFromAny converts an erased envelope into composer input.
//
// An indeterminate envelope returns early with no Values at all. That is
// the single most important branch in this file: it is what makes "I can't
// tell you" structurally incapable of arriving with numbers attached.
func FactsFromAny(env AnyEnvelope) Facts {
	facts := Facts{
		Intent:         env.Intent,
		Window:         env.Window,
		EvaluatedRange: formatRange(env.EvaluatedStart, env.EvaluatedEnd),
		Indeterminate:  env.Status == StatusIndeterminate,
		Reason:         env.Reason,
		Caveat:         caveatFor(env.Trust),
	}
	facts.Service, facts.Environment = scopeOf(env.Data)

	if facts.Indeterminate {
		facts.Headline = "indeterminate"
		return facts
	}

	switch data := env.Data.(type) {
	case SLOStatus:
		sloStatusFacts(&facts, data)
	case BurnRates:
		burnRateFacts(&facts, data)
	case ErrorBudget:
		errorBudgetFacts(&facts, data)
	case TelemetryTrust:
		telemetryTrustFacts(&facts, data)
	case Inventory:
		inventoryFacts(&facts, data)
	case RecentIncidents:
		recentIncidentsFacts(&facts, data)
	default:
		// An unknown payload is not an excuse to improvise. Refusing is
		// the same answer the closed intent set gives everywhere else.
		facts.Indeterminate = true
		facts.Headline = "indeterminate"
		facts.Reason = fmt.Sprintf("no answer template exists for intent %q", env.Intent)
		facts.Values = nil
	}
	return facts
}

func scopeOf(data any) (string, string) {
	switch d := data.(type) {
	case SLOStatus:
		return d.Service, d.Environment
	case BurnRates:
		return d.Service, d.Environment
	case ErrorBudget:
		return d.Service, d.Environment
	case TelemetryTrust:
		return d.Service, d.Environment
	case RecentIncidents:
		return d.Service, d.Environment
	default:
		return "", ""
	}
}

// caveatFor renders the trust verdict in our own words. Note what is
// absent: GateResult.Warning, which SigNoz authored, is reduced to a fixed
// sentence rather than quoted.
func caveatFor(trust *slo.GateResult) string {
	if trust == nil {
		return ""
	}
	var parts []string
	if trust.Trusted {
		parts = append(parts, "Telemetry trusted")
	} else {
		parts = append(parts, "Telemetry NOT trusted")
	}
	if !trust.QueryComplete {
		parts = append(parts, "the underlying queries did not complete")
	}
	if trust.Warning != "" {
		parts = append(parts, "the telemetry backend reported a query-completeness warning for this window")
	}
	return strings.Join(parts, "; ") + "."
}

func sloStatusFacts(facts *Facts, data SLOStatus) {
	facts.Headline = headlineForStates(data.SLOs)
	for _, state := range data.SLOs {
		prefix := state.Name + " "
		if len(data.SLOs) == 1 {
			prefix = ""
			// The headline already says the state; repeating it inside
			// the value list makes a one-SLO answer read like a form.
		} else {
			facts.Values = append(facts.Values, Fact{prefix + "state", string(state.State)})
		}
		facts.Values = append(facts.Values,
			Fact{prefix + "SLI", formatPercent(state.SLI)},
			Fact{prefix + "target", formatPercent(state.Target)},
			Fact{prefix + "burn rate", formatBurnRate(state.BurnRate)},
			Fact{prefix + "error budget remaining", formatPercent(state.ErrorBudgetRemaining)},
		)
		if facts.Window == "" && state.Window != "" {
			facts.Values = append(facts.Values, Fact{prefix + "window", state.Window})
		}
	}
	facts.Values = append(facts.Values, Fact{"SLOs evaluated", strconv.Itoa(len(data.SLOs))})
}

// headlineForStates decides the verdict by rule: any unhealthy SLO makes
// the whole answer unhealthy, any indeterminate one (with none unhealthy)
// makes it indeterminate, otherwise healthy. The model never gets to
// choose this word, exactly as internal/rca/presentation.go never lets the
// model choose a presentation mode.
func headlineForStates(states []SLOState) string {
	verdict := "healthy"
	indeterminate := false
	for _, state := range states {
		switch state.State {
		case slo.StateUnhealthy:
			return "unhealthy"
		case slo.StateIndeterminate:
			indeterminate = true
		}
	}
	if indeterminate {
		return "partially indeterminate"
	}
	return verdict
}

func burnRateFacts(facts *Facts, data BurnRates) {
	if len(data.FiringTiers) > 0 {
		facts.Headline = "burning fast enough to fire"
	} else {
		facts.Headline = "no burn tier firing"
	}
	// One fact per tier, not four. Six tiers times four figures is a
	// table, and a table posted into an incident channel is read by
	// nobody; the shape has to stay a sentence.
	for _, tier := range data.Tiers {
		label := tier.SLO + " " + tier.Tier + " (" + tier.LongWindow + "/" + tier.ShortWindow + ")"
		if tier.Indeterminate {
			facts.Values = append(facts.Values, Fact{label, "indeterminate"})
			continue
		}
		value := formatBurnRate(tier.LongBurn) + " and " + formatBurnRate(tier.ShortBurn) +
			" against " + formatBurnRate(tier.Threshold)
		if tier.Firing {
			// Parenthesised, not comma-separated: the value list is joined
			// with commas, and a comma inside a value makes "FIRING" look
			// like it belongs to the next tier.
			value += " (FIRING)"
		}
		facts.Values = append(facts.Values, Fact{label, value})
	}
	facts.Values = append(facts.Values,
		Fact{"tiers evaluated", strconv.Itoa(len(data.Tiers))},
		Fact{"tiers firing", strconv.Itoa(len(data.FiringTiers))},
	)
}

func errorBudgetFacts(facts *Facts, data ErrorBudget) {
	switch {
	case len(data.Exhausted) > 0:
		facts.Headline = "error budget exhausted"
	default:
		facts.Headline = "error budget remaining"
	}
	for _, budget := range data.Budgets {
		prefix := budget.SLO + " "
		if len(data.Budgets) == 1 {
			prefix = ""
		}
		facts.Values = append(facts.Values,
			Fact{prefix + "budget remaining", formatPercent(budget.Remaining)},
			Fact{prefix + "burn rate", formatBurnRate(budget.BurnRate)},
			Fact{prefix + "target", formatPercent(budget.Target)},
		)
		// The per-SLO window is only worth stating when it differs from
		// the one the answer already names; otherwise every reply ends
		// "window 1h ... Window 1h".
		if budget.Window != "" && budget.Window != facts.Window {
			facts.Values = append(facts.Values, Fact{prefix + "window", budget.Window})
		}
	}
	facts.Values = append(facts.Values,
		Fact{"SLOs evaluated", strconv.Itoa(len(data.Budgets))},
		Fact{"SLOs exhausted", strconv.Itoa(len(data.Exhausted))},
	)
}

func telemetryTrustFacts(facts *Facts, data TelemetryTrust) {
	switch data.OverallStatus {
	case audit.Pass:
		facts.Headline = "telemetry audit passing"
	case audit.Fail:
		facts.Headline = "telemetry audit failing"
	default:
		facts.Headline = "telemetry audit " + string(data.OverallStatus)
	}
	facts.Values = append(facts.Values,
		Fact{"profile", data.Profile},
		Fact{"score", formatScore(data.Score)},
		Fact{"coverage", formatPercent(data.Coverage)},
		Fact{"overall status", string(data.OverallStatus)},
	)
	for _, severity := range []string{"blocker", "warning", "info"} {
		// Zero counts are noise: "0 info findings" tells a reader nothing
		// they did not already infer from the absence of info findings.
		if count := data.CountsBySeverity[severity]; count > 0 {
			facts.Values = append(facts.Values, Fact{severity + " findings", strconv.Itoa(count)})
		}
	}
	// Rule IDs and recommendations come from the operator's own profile
	// YAML, so they are safe to quote; the raw evidence behind a finding
	// was already dropped one layer down, in TrustFinding.
	for _, finding := range data.FailedFindings {
		facts.Values = append(facts.Values, Fact{
			"failed rule " + finding.RuleID,
			fmt.Sprintf("%s, %s, %d affected", finding.Status, finding.Severity, finding.AffectedCount),
		})
	}
	facts.Values = append(facts.Values, Fact{"failed findings", strconv.Itoa(len(data.FailedFindings))})
}

func inventoryFacts(facts *Facts, data Inventory) {
	facts.Headline = "known services"
	for _, entry := range data.Services {
		detail := entry.Environment
		if entry.HasSLOConfig {
			detail += ", has SLO config"
		} else {
			detail += ", no SLO config"
		}
		facts.Values = append(facts.Values, Fact{entry.Service, detail})
	}
	facts.Values = append(facts.Values, Fact{"services known", strconv.Itoa(len(data.Services))})
}

func recentIncidentsFacts(facts *Facts, data RecentIncidents) {
	if len(data.Incidents) == 0 {
		facts.Headline = "no recent incidents"
	} else {
		facts.Headline = "recent incidents"
	}
	for _, incident := range data.Incidents {
		// RootCause is model-authored free text from a previous RCA. Do not
		// feed it back into the answer prompt or number allowlist.
		facts.Values = append(facts.Values, Fact{
			"incident " + incident.CorrelationID,
			strings.TrimSpace(fmt.Sprintf("%s %s", incident.Decision, formatInstant(incident.OpenedAt))),
		})
	}
	facts.Values = append(facts.Values, Fact{"incidents listed", strconv.Itoa(len(data.Incidents))})
}

// formatBurnRate renders a burn rate as e.g. "14.2x", matching the Slack
// Block Kit renderer exactly. Two surfaces rendering the same number
// differently is its own kind of inconsistency, and someone reading both
// should never have to wonder whether they are looking at one figure or
// two.
func formatBurnRate(value float64) string {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "n/a"
	}
	return strconv.FormatFloat(value, 'f', 1, 64) + "x"
}

// formatPercent renders a fraction as e.g. "-3.4%", preserving the sign: a
// negative error budget means it is already overspent, and rounding that
// away to zero would turn a breach into a near-miss.
func formatPercent(fraction float64) string {
	if math.IsNaN(fraction) || math.IsInf(fraction, 0) {
		return "n/a"
	}
	return strconv.FormatFloat(fraction*100, 'f', 1, 64) + "%"
}

// formatScore renders an audit score as a plain one-decimal number.
func formatScore(value float64) string {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "n/a"
	}
	return strconv.FormatFloat(value, 'f', 1, 64)
}

// formatWindow renders a lookback duration the way an SLO config would
// write it: "15m", not Go's "15m0s". The window appears in every answer,
// so it must not be shortened by suffix tricks that corrupt common values
// like 30m0s into "3".
func formatWindow(d time.Duration) string {
	if d == 0 {
		return "0s"
	}
	if d%time.Hour == 0 {
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	}
	if d%time.Minute == 0 {
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	}
	if d%time.Second == 0 {
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	}
	return d.String()
}

func formatInstant(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04 MST")
}

// formatRange renders the evaluated window as one readable span, collapsed
// to a single date when both ends fall on the same day. Always UTC, so two
// engineers in two timezones read the same clock.
func formatRange(start, end time.Time) string {
	if start.IsZero() || end.IsZero() {
		return ""
	}
	start, end = start.UTC(), end.UTC()
	if start.Format("2006-01-02") == end.Format("2006-01-02") {
		return fmt.Sprintf("%s %s-%s", start.Format("2006-01-02"), start.Format("15:04"), end.Format("15:04 MST"))
	}
	return fmt.Sprintf("%s to %s", start.Format("2006-01-02 15:04"), end.Format("2006-01-02 15:04 MST"))
}
