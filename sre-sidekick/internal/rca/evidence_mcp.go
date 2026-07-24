package rca

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/mcp"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/slo"
)

// MCPToolCaller is the subset of *mcp.Client that MCPEvidenceSource needs.
// Defined as an interface here (rather than depending on the concrete
// *mcp.Client type) so tests can substitute a fake that returns canned
// tool results without a live MCP server.
type MCPToolCaller interface {
	CallTool(ctx context.Context, name string, args map[string]any) (mcp.ToolResult, error)
}

// EvidenceSource is an optional evidence gatherer. When set on Agent, it
// replaces the M1 gatherEvidence(snapshot) placeholder: Agent.Diagnose
// calls Gather instead of building evidence from the evidence gate's
// snapshot. MCPEvidenceSource implements this by calling live SigNoz MCP
// tools; when Agent.EvidenceSource is nil (the default), M1 behavior is
// unchanged.
type EvidenceSource interface {
	Gather(ctx context.Context, inc Incident) ([]notify.Evidence, error)
}

// MCPEvidenceSource gathers evidence for an incident by calling SigNoz MCP
// tools directly - error traces, the top failing trace's spans, and
// correlated error logs, plus an optional metric snapshot - and converts
// the results into notify.Evidence with real SigNoz deep links (from each
// tool result's webUrl) where available.
//
// Telemetry text (log bodies, span attributes, status messages) is
// attacker-controllable: it is written by the instrumented application and
// everything it talks to, not by a trusted operator. Every Note built here
// goes through SanitizeFields/SanitizeNote (sanitize.go) before being
// attached to a notify.Evidence. Callers - especially the M4 LLM reasoner
// that will eventually read these Notes - MUST still treat Evidence.Note
// as untrusted data to quote, never as instructions to follow.
type MCPEvidenceSource struct {
	Client MCPToolCaller
	// Now returns the current time; overridable in tests. Defaults to
	// time.Now.
	Now func() time.Time
	// MetricName, if set, is queried once per incident via
	// signoz_query_metrics for a coarse metric snapshot (e.g. a call or
	// error counter). Left empty, no metric evidence is gathered - which
	// metric of the many SigNoz can report is actually informative varies
	// per service, so there is no safe universal default. Optional.
	MetricName string
}

func (s *MCPEvidenceSource) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Gather implements EvidenceSource. Errors calling any one tool are
// swallowed and that source simply contributes no evidence - a partial
// evidence set from the tools that did answer is more useful than failing
// the whole diagnosis over one flaky call - but ctx cancellation/deadline
// still aborts promptly since every CallTool honors ctx.
func (s *MCPEvidenceSource) Gather(ctx context.Context, inc Incident) ([]notify.Evidence, error) {
	if s.Client == nil {
		return nil, fmt.Errorf("rca: MCPEvidenceSource has no MCP client")
	}
	start, end := windowMillis(s.now(), inc.Window)

	var out []notify.Evidence
	add := func(items []notify.Evidence) {
		for _, item := range items {
			if len(out) >= maxEvidenceItems {
				return
			}
			item.ID = fmt.Sprintf("e%d", len(out)+1)
			out = append(out, item)
		}
	}

	traceEvidence, topTraceID := s.gatherTraces(ctx, inc, start, end)
	add(traceEvidence)

	if topTraceID != "" && len(out) < maxEvidenceItems {
		add(s.gatherTraceDetails(ctx, inc, topTraceID, start, end))
	}
	if len(out) < maxEvidenceItems {
		add(s.gatherLogs(ctx, inc, start, end))
	}
	if s.MetricName != "" && len(out) < maxEvidenceItems {
		add(s.gatherMetric(ctx, inc, start, end))
	}

	return out, nil
}

// windowMillis converts an incident window (e.g. "1h") and an end time
// into the [start, end] unix-millisecond bounds SigNoz MCP tools expect.
// An unparsable window falls back to a 1-hour lookback rather than
// failing evidence gathering outright.
func windowMillis(end time.Time, window string) (startMs, endMs int64) {
	duration, err := slo.WindowDuration(window)
	if err != nil {
		duration = time.Hour
	}
	return end.Add(-duration).UnixMilli(), end.UnixMilli()
}

// spanQueryEnvelope is the response shape shared by signoz_search_traces,
// signoz_search_logs, and signoz_get_trace_details: a query-result
// envelope wrapping one or more named queries, each with rows whose "data"
// field carries the actual span/log record.
type spanQueryEnvelope struct {
	Status string `json:"status"`
	Data   struct {
		Data struct {
			Results []struct {
				QueryName string `json:"queryName"`
				Rows      []struct {
					Data      map[string]any `json:"data"`
					Timestamp string         `json:"timestamp"`
				} `json:"rows"`
			} `json:"results"`
		} `json:"data"`
	} `json:"data"`
}

// records flattens every row's data map across every result set in the
// envelope, in order.
func (e spanQueryEnvelope) records() []map[string]any {
	var out []map[string]any
	for _, result := range e.Data.Data.Results {
		for _, row := range result.Rows {
			if row.Data == nil {
				continue
			}
			out = append(out, row.Data)
		}
	}
	return out
}

// traceFieldAllowlist names the span fields SanitizeFields is allowed to
// surface in trace evidence Notes. Chosen to be diagnostically useful
// (what operation, what error, how slow, which trace/span to look up)
// without pulling in every raw span attribute SigNoz returns.
var traceFieldAllowlist = []string{
	"name", "service.name", "status_code_string", "status_message",
	"duration_nano", "trace_id", "span_id", "http.route", "http_method",
}

// logFieldAllowlist names the log fields SanitizeFields is allowed to
// surface in log evidence Notes.
var logFieldAllowlist = []string{
	"body", "severity_text", "trace_id", "span_id",
}

func (s *MCPEvidenceSource) gatherTraces(ctx context.Context, inc Incident, start, end int64) ([]notify.Evidence, string) {
	result, err := s.Client.CallTool(ctx, "signoz_search_traces", map[string]any{
		"service": inc.Service,
		"error":   true,
		"start":   start,
		"end":     end,
		"limit":   maxEvidenceItems,
	})
	if err != nil {
		return nil, ""
	}
	var payload spanQueryEnvelope
	if err := result.Decode(&payload); err != nil {
		return nil, ""
	}

	var evs []notify.Evidence
	var topTraceID string
	for _, record := range payload.records() {
		if len(evs) >= maxEvidenceItems {
			break
		}
		evs = append(evs, spanEvidence(notify.EvidenceKindTrace, record, inc))
		if topTraceID == "" {
			if id, ok := record["trace_id"].(string); ok && id != "" {
				topTraceID = id
			}
		}
	}
	return evs, topTraceID
}

// gatherTraceDetails fetches the full trace for the top failing trace id
// found by gatherTraces, and surfaces the spans within it that actually
// carry an error - the trace search result is one span row per match, but
// the trace details call lets us show sibling context (e.g. the parent
// span that failed because a child timed out).
func (s *MCPEvidenceSource) gatherTraceDetails(ctx context.Context, inc Incident, traceID string, start, end int64) []notify.Evidence {
	result, err := s.Client.CallTool(ctx, "signoz_get_trace_details", map[string]any{
		"traceId":      traceID,
		"start":        start,
		"end":          end,
		"includeSpans": true,
	})
	if err != nil {
		return nil
	}
	var payload spanQueryEnvelope
	if err := result.Decode(&payload); err != nil {
		return nil
	}

	var evs []notify.Evidence
	for _, record := range payload.records() {
		if len(evs) >= maxEvidenceItems {
			break
		}
		if !recordHasErrorFlag(record) {
			continue
		}
		evs = append(evs, spanEvidence(notify.EvidenceKindTrace, record, inc))
	}
	return evs
}

func (s *MCPEvidenceSource) gatherLogs(ctx context.Context, inc Incident, start, end int64) []notify.Evidence {
	result, err := s.Client.CallTool(ctx, "signoz_search_logs", map[string]any{
		"service":  inc.Service,
		"severity": "ERROR",
		"start":    start,
		"end":      end,
		"limit":    maxEvidenceItems,
	})
	if err != nil {
		return nil
	}
	var payload spanQueryEnvelope
	if err := result.Decode(&payload); err != nil {
		return nil
	}

	var evs []notify.Evidence
	for _, record := range payload.records() {
		if len(evs) >= maxEvidenceItems {
			break
		}
		evs = append(evs, logEvidence(record, inc))
	}
	return evs
}

// metricScalarEnvelope is the response shape of signoz_query_metrics with
// requestType=scalar: a query-result envelope whose results carry a
// column list and a matching row of scalar values, rather than the
// span/log row shape spanQueryEnvelope handles.
type metricScalarEnvelope struct {
	Status string `json:"status"`
	Data   struct {
		Data struct {
			Results []struct {
				QueryName string      `json:"queryName"`
				Data      [][]float64 `json:"data"`
			} `json:"results"`
		} `json:"data"`
	} `json:"data"`
}

func (s *MCPEvidenceSource) gatherMetric(ctx context.Context, inc Incident, start, end int64) []notify.Evidence {
	result, err := s.Client.CallTool(ctx, "signoz_query_metrics", map[string]any{
		"metricName":       s.MetricName,
		"filter":           fmt.Sprintf("service.name = '%s'", escapeFilterValue(inc.Service)),
		"requestType":      "scalar",
		"reduceTo":         "sum",
		"spaceAggregation": "sum",
		"timeAggregation":  "increase",
		"start":            start,
		"end":              end,
	})
	if err != nil {
		return nil
	}
	var payload metricScalarEnvelope
	if err := result.Decode(&payload); err != nil {
		return nil
	}

	var evs []notify.Evidence
	for _, r := range payload.Data.Data.Results {
		if len(evs) >= maxEvidenceItems {
			break
		}
		if len(r.Data) == 0 || len(r.Data[0]) == 0 {
			continue
		}
		note := SanitizeNote(fmt.Sprintf("metric %s = %v over the incident window", s.MetricName, r.Data[0][0]))
		evs = append(evs, notify.Evidence{
			Kind:       notify.EvidenceKindMetric,
			SignozLink: placeholderLink(inc),
			Note:       note,
		})
	}
	return evs
}

// spanEvidence converts one trace-span record into evidence, preferring
// the record's own webUrl (a real SigNoz deep link to that trace) over
// the generic scoped placeholder link.
func spanEvidence(kind notify.EvidenceKind, record map[string]any, inc Incident) notify.Evidence {
	fields := SanitizeFields(record, traceFieldAllowlist)
	return notify.Evidence{
		Kind:       kind,
		SignozLink: recordLink(record, inc),
		Note:       SanitizeNote(formatTraceNote(fields)),
	}
}

func logEvidence(record map[string]any, inc Incident) notify.Evidence {
	fields := SanitizeFields(record, logFieldAllowlist)
	return notify.Evidence{
		Kind:       notify.EvidenceKindLog,
		SignozLink: recordLink(record, inc),
		Note:       SanitizeNote(formatLogNote(fields)),
	}
}

func formatTraceNote(fields map[string]string) string {
	var parts []string
	if op := fields["name"]; op != "" {
		parts = append(parts, fmt.Sprintf("operation %s", op))
	}
	if msg := fields["status_message"]; msg != "" {
		parts = append(parts, fmt.Sprintf("errored: %s", msg))
	} else if code := fields["status_code_string"]; code != "" {
		parts = append(parts, fmt.Sprintf("status %s", code))
	}
	if dur := fields["duration_nano"]; dur != "" {
		parts = append(parts, fmt.Sprintf("duration=%s", formatDurationNanos(dur)))
	}
	if len(parts) == 0 {
		return "trace span with no further detail available"
	}
	return strings.Join(parts, ", ")
}

// formatDurationNanos renders a nanosecond count (as decoded by
// SanitizeFields, so already a plain-decimal string, never scientific
// notation) as a human-readable Go duration, e.g. "5s" instead of
// "5000000000". Falls back to the raw string if it cannot be parsed.
func formatDurationNanos(nanos string) string {
	value, err := strconv.ParseFloat(nanos, 64)
	if err != nil {
		return nanos
	}
	return time.Duration(value).String()
}

func formatLogNote(fields map[string]string) string {
	severity := fields["severity_text"]
	body := fields["body"]
	switch {
	case severity == "" && body == "":
		return "log record with no further detail available"
	case severity == "":
		return body
	case body == "":
		return fmt.Sprintf("[%s] log record with no message body", severity)
	default:
		return fmt.Sprintf("[%s] %s", severity, body)
	}
}

// recordLink returns the record's own webUrl (a real SigNoz deep link
// produced by the MCP tool) when present; SigNoz search results include
// this for spans but not, in current server versions, for logs. When
// absent, a scoped-but-imprecise fallback link is used instead, so it is
// clear from the incident alone what the evidence is about even without a
// precise deep link.
func recordLink(record map[string]any, inc Incident) string {
	if link, ok := record["webUrl"].(string); ok && link != "" {
		return link
	}
	return placeholderLink(inc)
}

func recordHasErrorFlag(record map[string]any) bool {
	if v, ok := record["has_error"].(bool); ok {
		return v
	}
	return false
}

// escapeFilterValue strips single quotes from a value interpolated into a
// SigNoz filter expression, so an incident's Service/Environment (which
// may ultimately originate from a firing alert, not a trusted operator)
// cannot break out of the quoted string literal it is placed into.
func escapeFilterValue(value string) string {
	return strings.ReplaceAll(value, "'", "")
}
