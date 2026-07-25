package rca

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/slo"
)

// This file computes the deterministic signals PRD 13.4's presentation
// rule engine (presentation.go) decides on: error-rate delta, latency
// shift, error concentration, saturation, and deploy-onset alignment -
// Google's Four Golden Signals (Latency, Traffic, Errors, Saturation) and
// RED (Rate, Errors, Duration) / USE (Utilization, Saturation, Errors),
// applied to one incident's evidence. Nothing in this file is authored by
// the reasoner: every field of Signals is computed by code from telemetry,
// and any signal that cannot be cleanly computed is left absent
// (Present/Metric.Present/BoolSignal.Present == false) rather than
// fabricated - see each field's doc comment on Signals for exactly what
// "cannot be cleanly computed" means for that signal.

// Metric is a deterministically computed numeric signal that may be
// unavailable from the telemetry gathered for an incident. Present is
// false whenever the signal could not be computed - never fabricated as
// zero - so Decide (presentation.go) can tell "measured and below
// threshold" apart from "not measured at all".
type Metric struct {
	Present bool
	Value   float64
}

// BoolSignal is the same idea as Metric for a fact that is naturally
// boolean rather than numeric (e.g. "is the system saturated right now").
type BoolSignal struct {
	Present bool
	Value   bool
}

// Concentration reports how tightly the error evidence gathered for an
// incident clusters around one operation (span name) or one classified
// exception signature - the "errors" axis of the Four Golden Signals / RED
// error rate. A single dependency timing out on every failing request
// looks very different from ten unrelated failure modes, and PRD 13.4
// singles this out as the one signal that "MUST work" from evidence alone.
type Concentration struct {
	// Present is true when there was at least one error evidence record to
	// analyze. False only when ev contained no trace/log evidence at all.
	Present bool
	// TopOperation is the most common span/operation name among the error
	// evidence (e.g. "tool.search_kb", parsed from the "operation ..."
	// prefix trace evidence Notes carry - see extractOperation), and
	// TopOperationShare is its share of all error evidence records (0..1).
	// Log evidence has no operation name and never contributes here.
	TopOperation      string
	TopOperationShare float64
	// TopException is the most common classified exception signature (see
	// classifyException) among the error evidence whose message could be
	// classified at all, and TopExceptionShare is its share of that
	// classifiable subset (0..1) - not of all evidence. Records whose
	// error text matches no known exception pattern (e.g. a bare
	// "operation X failed" message with no distinguishing detail) are
	// excluded from this share's denominator rather than diluting it:
	// they carry no signature to concentrate on, so they should not count
	// as "not concentrated" evidence either. When nothing was
	// classifiable, TopException is "" and TopExceptionShare is 0.
	TopException      string
	TopExceptionShare float64
}

// DominantShare is the strongest concentration reading available: the
// larger of the by-operation and by-exception shares. A cascading failure
// often shows up as several operations each reporting an error for the
// same underlying incident (the callee times out, and the caller then
// fails too) so no single operation dominates by name alone, while the
// exception signature (e.g. "TimeoutError") stays identical across all of
// them - DominantShare lets that count as concentrated.
func (c Concentration) DominantShare() float64 {
	if c.TopExceptionShare > c.TopOperationShare {
		return c.TopExceptionShare
	}
	return c.TopOperationShare
}

// Signals is the deterministic evidence PRD 13.4's presentation rule
// engine decides on, computed from an incident's already-gathered evidence
// plus (where cleanly available) a small, bounded set of targeted
// follow-up telemetry queries. The model never sees or sets a Signals
// value.
type Signals struct {
	// ErrorSampleCount is how many error-evidence records (trace or log
	// kind) Concentration is based on. Below
	// PresentationConfig.MinErrorSamples, the presentation rules refuse to
	// draw any conclusion regardless of how concentrated the few samples
	// look.
	ErrorSampleCount int
	Concentration    Concentration
	// LatencyShift is present when a latency comparison could be computed
	// via MCP: the ratio of the incident window's failing-request p99
	// duration to its successful-request p99 duration, both queried over
	// the same window. A ratio near 1 means failures are not
	// latency-driven (e.g. fast validation errors); a ratio much greater
	// than 1 means failing requests run far longer than successful ones
	// (e.g. a timeout). This is computed within one window rather than
	// against a prior "healthy" window because a prior window may already
	// be inside the same ongoing incident - see the doc comment on
	// latencyShiftSignal for why a window-over-window comparison was
	// rejected.
	LatencyShift Metric
	// ErrorRateDelta is present when a window-over-window error-rate
	// comparison could be computed via MCP: the incident window's error
	// rate (errors/sec) minus the immediately preceding window of equal
	// length's error rate. A meaningfully positive delta means the error
	// rate itself is rising, not merely present; near zero (including for
	// a long-sustained incident whose "before" window is already
	// degraded) means this signal does not add anything beyond
	// Concentration.
	ErrorRateDelta Metric
	// Saturation reports whether a resource-saturation signal (USE
	// method: queue depth, CPU, memory, connection-pool exhaustion, ...)
	// could be found for the incident's service. No generic
	// service-agnostic saturation metric exists to query here, so this is
	// always Present=false ("unknown") today - never fabricated. Wiring a
	// real saturation source (e.g. a configured metric name per service)
	// is future work.
	Saturation BoolSignal
	// DeployOnset reports whether the incident's onset aligns with a
	// change/deploy marker. No deploy-marker telemetry is available from
	// the SigNoz MCP tools this package calls (there is no
	// deployment/change-event tool), so this is always Present=false
	// ("unknown") today. PRD 13.4 says explicitly to use this signal only
	// "when that telemetry exists" - it does not here.
	DeployOnset BoolSignal
}

// ComputeSignals computes Signals for an incident from its already-
// gathered evidence, plus (when caller is non-nil) a small, bounded set of
// targeted MCP calls for the signals that cannot be derived from evidence
// text alone (LatencyShift, ErrorRateDelta - four signoz_aggregate_traces
// calls total). ErrorSampleCount and Concentration are always computed
// from ev alone and never depend on caller - PRD 13.4 requires this one to
// "MUST work" even with no MCP access, and it is what makes ComputeSignals
// unit-testable with canned evidence and no live telemetry.
//
// caller may be nil (e.g. when Agent.EvidenceSource is not MCP-backed, or
// in tests): every MCP-derived field is simply left Present=false in that
// case, never fabricated. A tool-call error or an unparsable response is
// treated the same way - this function never fails the diagnosis over a
// missing or flaky signal.
func ComputeSignals(ctx context.Context, inc Incident, ev []notify.Evidence, caller MCPToolCaller, now time.Time) Signals {
	count, concentration := evidenceSignals(ev)
	signals := Signals{ErrorSampleCount: count, Concentration: concentration}
	if caller == nil {
		return signals
	}
	signals.LatencyShift = latencyShiftSignal(ctx, caller, inc, now)
	signals.ErrorRateDelta = errorRateDeltaSignal(ctx, caller, inc, now)
	return signals
}

// evidenceSignals is the pure, deterministic part of ComputeSignals: it
// looks only at the evidence already gathered for the incident (trace and
// log evidence - the kinds MCPEvidenceSource gathers as error evidence for
// a diagnosis; metric evidence carries no operation/exception to group by
// and is ignored here) and needs no telemetry access of its own.
func evidenceSignals(ev []notify.Evidence) (int, Concentration) {
	operationCounts := map[string]int{}
	exceptionCounts := map[string]int{}
	sampleCount := 0
	classifiable := 0
	for _, item := range ev {
		if item.Kind != notify.EvidenceKindTrace && item.Kind != notify.EvidenceKindLog {
			continue
		}
		sampleCount++
		if op := extractOperation(item.Note); op != "" {
			operationCounts[op]++
		}
		if exc := classifyException(item.Note); exc != "" {
			exceptionCounts[exc]++
			classifiable++
		}
	}
	if sampleCount == 0 {
		return 0, Concentration{}
	}
	concentration := Concentration{Present: true}
	if topOp, topOpCount := topCount(operationCounts); topOp != "" {
		concentration.TopOperation = topOp
		concentration.TopOperationShare = shareOf(topOpCount, sampleCount)
	}
	if classifiable > 0 {
		topExc, topExcCount := topCount(exceptionCounts)
		concentration.TopException = topExc
		concentration.TopExceptionShare = shareOf(topExcCount, classifiable)
	}
	return sampleCount, concentration
}

func shareOf(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total)
}

// topCount returns the most frequent key in counts and its count. Ties are
// broken by lexical key order (map iteration order in Go is randomized,
// and this signal must be reproducible), not by insertion order.
func topCount(counts map[string]int) (string, int) {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var best string
	var bestCount int
	for _, k := range keys {
		if counts[k] > bestCount {
			best, bestCount = k, counts[k]
		}
	}
	return best, bestCount
}

// operationPattern matches the "operation <name>" prefix formatTraceNote
// (evidence_mcp.go) writes when a trace span's name field was present -
// the leading token of a trace evidence Note. Log evidence Notes
// (formatLogNote) never match this, correctly: a log line has no span
// name to group by.
var operationPattern = regexp.MustCompile(`^operation ([^,\s]+)`)

// extractOperation pulls the span/operation name back out of a trace
// evidence Note built by formatTraceNote.
func extractOperation(note string) string {
	m := operationPattern.FindStringSubmatch(note)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// errorMessagePattern matches the "errored: <message>" segment
// formatTraceNote writes for a trace span whose status_message was
// non-empty.
var errorMessagePattern = regexp.MustCompile(`errored: ([^,]+)`)

// exceptionSignature is one recognizable, distinct failure shape this
// package knows how to classify from evidence text. Order matters:
// classifyException checks signatures in slice order and the first match
// wins, so more specific patterns are listed before more general ones.
type exceptionSignature struct {
	label    string
	patterns []string
}

var exceptionSignatures = []exceptionSignature{
	{"TimeoutError", []string{"timed out", "timeout", "deadline exceeded", "context canceled", "context cancelled"}},
	{"ConnectionError", []string{"connection refused", "connection reset", "econnrefused", "broken pipe", "no route to host"}},
	{"AuthError", []string{"unauthorized", "forbidden", "permission denied", "access denied"}},
	{"RateLimitError", []string{"rate limit", "too many requests", "429"}},
	{"NotFoundError", []string{"not found", "404"}},
	{"ServerError", []string{"internal server error", "502", "503", "504", "500"}},
	{"NilPointerError", []string{"nil pointer", "null pointer", "nullpointerexception", "nil dereference"}},
}

// classifyException extracts the error message to classify from a trace
// or log evidence Note (see errorMessage) and matches it against
// exceptionSignatures, returning the first matching signature's label, or
// "" when the message carries no recognizable signature (e.g. a bare
// "operation X failed" message that says something failed without saying
// how). Matching is case-insensitive substring matching against a small,
// deliberately conservative allowlist, not NLP - see Concentration's doc
// comment for what an unclassifiable record means for TopExceptionShare.
func classifyException(note string) string {
	message := errorMessage(note)
	if message == "" {
		return ""
	}
	lower := strings.ToLower(message)
	for _, sig := range exceptionSignatures {
		for _, pattern := range sig.patterns {
			if strings.Contains(lower, pattern) {
				return sig.label
			}
		}
	}
	return ""
}

// errorMessage extracts the text classifyException should inspect from an
// evidence Note:
//   - for a trace Note with an "errored: ..." segment (formatTraceNote),
//     just that segment;
//   - for a trace Note with no such segment (e.g. formatTraceNote's bare
//     status-code fallback, "operation X, status STATUS_CODE_ERROR,
//     duration=..."), nothing - there is no distinguishing message here,
//     and classifying the operation name or a duration string (e.g. a
//     "500ms" duration spuriously matching the "500" ServerError pattern)
//     would be a false signature, not a real one;
//   - for a log Note (formatLogNote, which is never prefixed with
//     "operation ..."), the whole Note - severity and body together are
//     the message.
func errorMessage(note string) string {
	if m := errorMessagePattern.FindStringSubmatch(note); len(m) == 2 {
		return m[1]
	}
	if operationPattern.MatchString(note) {
		return ""
	}
	return note
}

// latencyShiftSignal computes Signals.LatencyShift via two
// signoz_aggregate_traces calls: p99 duration for failing spans and p99
// duration for successful spans, both over the incident window.
//
// A window-over-window comparison (this window's p99 vs. the immediately
// preceding window's p99, which is what PRD 13.4's "now vs baseline"
// phrasing suggests most literally) was considered and rejected: for a
// sustained incident - the common case an on-call engineer is actually
// paged for - the "before" window can already be inside the same outage,
// making the ratio ~1 and hiding a real latency shift behind a baseline
// that was never healthy to begin with. Comparing failing-request latency
// to the service's own successful-request latency in the same window
// avoids that trap, is always available whenever both successful and
// failed requests coexist, and directly answers the diagnostically useful
// question: are these failures latency-driven (e.g. a timeout) or fail-fast
// (e.g. a validation error)?
func latencyShiftSignal(ctx context.Context, caller MCPToolCaller, inc Incident, now time.Time) Metric {
	start, end := windowMillis(now, inc.Window)
	errorP99, ok1 := aggregateTracesScalar(ctx, caller, inc.Service, "p99", "duration_nano", true, start, end)
	successP99, ok2 := aggregateTracesScalar(ctx, caller, inc.Service, "p99", "duration_nano", false, start, end)
	if !ok1 || !ok2 || successP99 <= 0 {
		return Metric{}
	}
	return Metric{Present: true, Value: errorP99 / successP99}
}

// errorRateDeltaSignal computes Signals.ErrorRateDelta via two
// signoz_aggregate_traces calls: the incident window's error rate
// (errors/sec) and the immediately preceding window of equal length's
// error rate.
func errorRateDeltaSignal(ctx context.Context, caller MCPToolCaller, inc Incident, now time.Time) Metric {
	duration, err := slo.WindowDuration(inc.Window)
	if err != nil {
		duration = time.Hour
	}
	end := now
	start := end.Add(-duration)
	baselineEnd := start
	baselineStart := baselineEnd.Add(-duration)

	currentRate, ok1 := aggregateTracesScalar(ctx, caller, inc.Service, "rate", "", true, start.UnixMilli(), end.UnixMilli())
	baselineRate, ok2 := aggregateTracesScalar(ctx, caller, inc.Service, "rate", "", true, baselineStart.UnixMilli(), baselineEnd.UnixMilli())
	if !ok1 || !ok2 {
		return Metric{}
	}
	return Metric{Present: true, Value: currentRate - baselineRate}
}

// aggregateTracesScalar calls the SigNoz MCP signoz_aggregate_traces tool
// in scalar mode and returns its single result value. aggregateOn is
// omitted from the call when empty (e.g. for the "count"/"rate"
// aggregations, which need no field to aggregate on). ok is false for any
// tool-call error, decode error, or empty result - callers treat that as
// "signal not available", never as zero.
func aggregateTracesScalar(ctx context.Context, caller MCPToolCaller, service, aggregation, aggregateOn string, errorVal bool, startMs, endMs int64) (float64, bool) {
	args := map[string]any{
		"service":     service,
		"aggregation": aggregation,
		"error":       errorVal,
		"start":       startMs,
		"end":         endMs,
		"requestType": "scalar",
	}
	if aggregateOn != "" {
		args["aggregateOn"] = aggregateOn
	}
	result, err := caller.CallTool(ctx, "signoz_aggregate_traces", args)
	if err != nil {
		return 0, false
	}
	var payload metricScalarEnvelope
	if err := result.Decode(&payload); err != nil {
		return 0, false
	}
	for _, r := range payload.Data.Data.Results {
		if len(r.Data) > 0 && len(r.Data[0]) > 0 {
			return r.Data[0][0], true
		}
	}
	return 0, false
}
