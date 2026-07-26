package answer

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/slo"
)

// maxIdentLen bounds every identifier-shaped argument. Service names,
// environments and SLO names are all short by construction; anything
// longer is either a mistake or an attempt to smuggle a payload through a
// field that is supposed to be an identifier.
const maxIdentLen = 128

// maxLimit caps any list-shaped result. Without it, "show me the last
// 100000 incidents" becomes a denial-of-service against the store and an
// unbounded prompt against the model.
const maxLimit = 50

// validIdent enforces rule 4: no tool accepts free-form text. A question
// never becomes a query string; an argument is an identifier drawn from a
// conservative charset, so there is no syntax available to inject into a
// downstream filter expression or into the model's context.
//
// This is the choke point that bounds prompt-injection blast radius for
// the @mention entry point. Widening this charset - in particular allowing
// quotes, whitespace or the builder-filter operators - would quietly
// undo that, because the SLO engine composes filter expressions such as
// "service_name = '<service>'" from these values.
func validIdent(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", field)
	}
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("%s must not have leading or trailing whitespace", field)
	}
	if len(value) > maxIdentLen {
		return fmt.Errorf("%s must be at most %d characters", field, maxIdentLen)
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == '/', r == ':':
		default:
			return fmt.Errorf("%s contains an unsupported character %q; "+
				"identifiers may use letters, digits and -_./:", field, r)
		}
	}
	return nil
}

// EmptyArgs is the input to a tool that takes no arguments at all.
type EmptyArgs struct{}

func (EmptyArgs) Validate() error  { return nil }
func (EmptyArgs) CacheKey() string { return "-" }

// ServiceArgs scopes a question to one service in one environment.
type ServiceArgs struct {
	Service     string `json:"service"`
	Environment string `json:"environment"`
}

func (a ServiceArgs) Validate() error {
	if err := validIdent("service", a.Service); err != nil {
		return err
	}
	return validIdent("environment", a.Environment)
}

func (a ServiceArgs) CacheKey() string { return a.Service + "|" + a.Environment }

// SLOArgs scopes a question to one service, optionally narrowed to a
// single named SLO. An empty SLO means "every SLO configured for this
// service" rather than an error, because most configs define one primary
// SLO and asking about "the" burn rate is the common case.
type SLOArgs struct {
	Service     string `json:"service"`
	Environment string `json:"environment"`
	SLO         string `json:"slo,omitempty"`
}

func (a SLOArgs) Validate() error {
	if err := validIdent("service", a.Service); err != nil {
		return err
	}
	if err := validIdent("environment", a.Environment); err != nil {
		return err
	}
	if a.SLO == "" {
		return nil
	}
	return validIdent("slo", a.SLO)
}

func (a SLOArgs) CacheKey() string { return a.Service + "|" + a.Environment + "|" + a.SLO }

func (a SLOArgs) service() ServiceArgs {
	return ServiceArgs{Service: a.Service, Environment: a.Environment}
}

// HistoryArgs scopes a question to a bounded slice of recent history.
type HistoryArgs struct {
	Service     string `json:"service"`
	Environment string `json:"environment"`
	Limit       int    `json:"limit,omitempty"`
}

func (a HistoryArgs) Validate() error {
	if err := validIdent("service", a.Service); err != nil {
		return err
	}
	if err := validIdent("environment", a.Environment); err != nil {
		return err
	}
	if a.Limit < 0 || a.Limit > maxLimit {
		return fmt.Errorf("limit must be between 0 and %d, got %d", maxLimit, a.Limit)
	}
	return nil
}

func (a HistoryArgs) CacheKey() string {
	return fmt.Sprintf("%s|%s|%d", a.Service, a.Environment, a.Limit)
}

// serviceSchema, sloSchema and emptySchema are the JSON Schemas the model
// sees. They are closed (additionalProperties: false) so a model cannot
// smuggle an unmodelled field past the decoder, which is also enforced
// independently by DisallowUnknownFields in Tool.invokeJSON.
var (
	emptySchema = json.RawMessage(`{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`)

	serviceSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "service": {"type": "string", "description": "Service name as registered in the profile registry."},
    "environment": {"type": "string", "description": "Environment name, e.g. production or local."}
  },
  "required": ["service", "environment"],
  "additionalProperties": false
}`)

	sloSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "service": {"type": "string", "description": "Service name as registered in the profile registry."},
    "environment": {"type": "string", "description": "Environment name, e.g. production or local."},
    "slo": {"type": "string", "description": "Optional SLO name. Omit to cover every SLO configured for the service."}
  },
  "required": ["service", "environment"],
  "additionalProperties": false
}`)

	historySchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "service": {"type": "string", "description": "Service name as registered in the profile registry."},
    "environment": {"type": "string", "description": "Environment name, e.g. production or local."},
    "limit": {"type": "integer", "description": "Maximum number of incidents to return (1-50)."}
  },
  "required": ["service", "environment"],
  "additionalProperties": false
}`)
)

// matchesSLO reports whether a report/tier belongs to the SLO the caller
// asked about. An empty filter matches everything.
func matchesSLO(filter, name string) bool { return filter == "" || filter == name }

// provenance folds the per-report evaluation ranges into the single window
// an Envelope advertises. The window is only reported when every report
// agrees on it - a config mixing a 1h latency SLO with a 30d availability
// SLO has no single answer, and claiming one would be a fabricated fact.
func provenance[T any](env *Envelope[T], reports []slo.Report) {
	window := ""
	for i, report := range reports {
		if i == 0 {
			window = report.Window
		} else if window != report.Window {
			window = ""
		}
		if !report.EvaluatedStart.IsZero() &&
			(env.EvaluatedStart.IsZero() || report.EvaluatedStart.Before(env.EvaluatedStart)) {
			env.EvaluatedStart = report.EvaluatedStart
		}
		if report.EvaluatedEnd.After(env.EvaluatedEnd) {
			env.EvaluatedEnd = report.EvaluatedEnd
		}
	}
	env.Window = window
}

// worstTrust reduces per-SLO gate results to the one verdict the answer
// carries. It is deliberately pessimistic: coverage is the minimum seen,
// QueryComplete and Trusted hold only if they hold everywhere, and the
// reasons are joined. An answer covering four SLOs, one of which had
// untrusted telemetry, must not present itself as fully trusted.
func worstTrust(reports []slo.Report) *slo.GateResult {
	if len(reports) == 0 {
		return nil
	}
	worst := slo.GateResult{Coverage: 1, QueryComplete: true, Trusted: true}
	var reasons, warnings []string
	for _, report := range reports {
		if report.Gate.Coverage < worst.Coverage {
			worst.Coverage = report.Gate.Coverage
		}
		worst.QueryComplete = worst.QueryComplete && report.Gate.QueryComplete
		worst.Trusted = worst.Trusted && report.Gate.Trusted
		if report.Gate.Reason != "" {
			reasons = append(reasons, report.Name+": "+report.Gate.Reason)
		}
		if report.Gate.Warning != "" {
			warnings = append(warnings, report.Name+": "+report.Gate.Warning)
		}
	}
	worst.Reason = strings.Join(reasons, "; ")
	worst.Warning = strings.Join(warnings, "; ")
	return &worst
}

// untrustedReason explains, in the words the engine itself produced, why
// an answer is indeterminate. It never invents an explanation: if the gate
// gave no reason, neither do we.
func untrustedReason(reports []slo.Report) string {
	var reasons []string
	for _, report := range reports {
		// Both halves matter and neither subsumes the other: the report
		// error is the engine's verdict ("completeness is below the gate")
		// while the gate reason is the underlying cause ("dependency total
		// query failed: clickhouse unavailable"). Reporting only the
		// verdict would tell someone their telemetry is untrusted without
		// telling them their database is down.
		var parts []string
		if report.Error != "" {
			parts = append(parts, report.Error)
		}
		if report.Gate.Reason != "" && report.Gate.Reason != report.Error {
			parts = append(parts, report.Gate.Reason)
		}
		if len(parts) > 0 {
			reasons = append(reasons, report.Name+": "+strings.Join(parts, " (")+
				strings.Repeat(")", len(parts)-1))
		}
	}
	if len(reasons) == 0 {
		return "the completeness gate did not trust the telemetry for any configured SLO"
	}
	return "telemetry was not trusted: " + strings.Join(reasons, "; ")
}
