package rca

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/evidence"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/profile"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/slo"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/source"
)

// EvidenceResult reports whether the telemetry available for an incident's
// window carries enough signal to attempt a root cause. This is a
// different concept from the SLO completeness gate (internal/slo.Gate): a
// window can be "complete" enough to trust an SLI (e.g. the counters used
// to compute it are present and fresh), yet contain none of the error
// spans, exceptions, or logs a human would need to explain *why* the SLO
// failed. The RCA evidence gate answers that second question.
type EvidenceResult struct {
	HasErrorSpans bool
	HasExceptions bool
	HasLogs       bool
	// Sufficient is true when there is enough signal to attempt a root
	// cause. M1 heuristic: any one of HasErrorSpans, HasExceptions, or
	// HasLogs is enough to try - this is deliberately permissive.
	// Tightening it (e.g. requiring corroboration across signals, or a
	// minimum record count) is left for a later milestone once real
	// incidents show what actually helps.
	Sufficient bool
	// Partial reports that the snapshot behind this result was truncated
	// (evidence.Snapshot.Partial - e.g. a trace or log query hit its row
	// limit): Sufficient may still be true when real evidence was found
	// despite the truncation, but a diagnosis built on it should say so.
	Partial bool
	// Reason is a short human-readable explanation of the Sufficient
	// verdict, e.g. what evidence was found or what was missing.
	Reason string
}

// EvidenceGate decides whether enough evidence exists to attempt a root
// cause for an incident. It is the RCA-specific evidence gate: it never
// computes an SLO verdict, score, or threshold - only whether there is
// telemetry worth reasoning about.
type EvidenceGate interface {
	Check(ctx context.Context, inc Incident) (EvidenceResult, error)
}

// SnapshotProvider is an optional capability of an EvidenceGate: gates that
// query a source.TelemetrySource can expose the evidence.Snapshot they
// fetched, so a caller (the Agent) can reuse it for evidence gathering
// instead of querying the telemetry source a second time. SourceEvidenceGate
// implements this.
type SnapshotProvider interface {
	CheckWithSnapshot(ctx context.Context, inc Incident) (EvidenceResult, evidence.Snapshot, error)
}

// SourceEvidenceGate implements EvidenceGate against a
// source.TelemetrySource (e.g. the SigNoz-backed adapter in
// internal/source/signoz, or source.MemorySource in tests).
type SourceEvidenceGate struct {
	Source source.TelemetrySource
	// Now returns the current time; overridable in tests. Defaults to
	// time.Now.
	Now func() time.Time
}

func (g *SourceEvidenceGate) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// Check implements EvidenceGate. It fetches a snapshot and discards it;
// callers that need the snapshot (the Agent, to gather evidence without a
// second query) should use CheckWithSnapshot instead.
func (g *SourceEvidenceGate) Check(ctx context.Context, inc Incident) (EvidenceResult, error) {
	result, _, err := g.CheckWithSnapshot(ctx, inc)
	return result, err
}

// CheckWithSnapshot fetches the snapshot for the incident's
// service/environment/window exactly once and returns both the
// EvidenceResult computed from it and the snapshot itself.
func (g *SourceEvidenceGate) CheckWithSnapshot(ctx context.Context, inc Incident) (EvidenceResult, evidence.Snapshot, error) {
	if g.Source == nil {
		return EvidenceResult{}, evidence.Snapshot{}, fmt.Errorf("rca: evidence gate has no telemetry source")
	}
	duration, err := slo.WindowDuration(inc.Window)
	if err != nil {
		return EvidenceResult{}, evidence.Snapshot{}, fmt.Errorf("rca: %w", err)
	}
	end := g.now()
	target := source.Target{
		Service:     inc.Service,
		Environment: inc.Environment,
		Start:       end.Add(-duration),
		End:         end,
	}
	snapshot, err := g.Source.Snapshot(ctx, evidenceProfile(inc.Service, inc.Environment), target)
	if err != nil {
		return EvidenceResult{}, evidence.Snapshot{}, fmt.Errorf("rca: fetch snapshot: %w", err)
	}
	return evaluateSnapshot(snapshot), snapshot, nil
}

// evidenceProfile builds a synthetic profile.Profile that asks a
// TelemetrySource for every signal (traces, metrics, logs). The RCA agent
// does not operate against a declared Track A audit profile the way
// audit-watch does - it just needs whatever telemetry exists for the
// incident's service/environment - so it synthesizes a profile whose
// Signals are all non-empty, which is the only thing TelemetrySource
// implementations (see signoz.TelemetrySource.profileUsesSignal) use to
// decide which signals to query.
func evidenceProfile(service, environment string) profile.Profile {
	return profile.Profile{
		Metadata: profile.Metadata{Service: service, Environment: environment},
		Spec: profile.Spec{
			Signals: profile.Signals{
				Traces:  profile.SignalSpec{RootSpan: "root"},
				Logs:    profile.SignalSpec{Fields: []profile.FieldSpec{{Path: "body"}}},
				Metrics: profile.SignalSpec{Fields: []profile.FieldSpec{{Path: "value"}}},
			},
		},
	}
}

func evaluateSnapshot(snapshot evidence.Snapshot) EvidenceResult {
	// A query that did not complete (QueryComplete=false - the source
	// failed, timed out, or otherwise could not run the query) means we do
	// not actually know what evidence exists for this window: whatever
	// records happen to be in the snapshot are not a reliable answer to
	// "is there enough signal here", and treating them as sufficient would
	// let the reasoner run on evidence that might be entirely missing.
	// PRD invariant: missing or failed evidence must resolve to
	// indeterminate, never to a pass - fail safe here rather than trusting
	// a snapshot the source itself could not finish fetching.
	if !snapshot.QueryComplete {
		return EvidenceResult{
			Reason: "the telemetry query for this window did not complete; treating evidence as insufficient rather than reasoning over an unreliable snapshot",
		}
	}

	result := EvidenceResult{
		HasLogs:       len(snapshot.Logs) > 0,
		HasErrorSpans: anyRecordHasError(snapshot.Traces),
		HasExceptions: anyRecordHasException(snapshot.Traces) || anyRecordHasException(snapshot.Logs),
		Partial:       snapshot.Partial,
	}
	result.Sufficient = result.HasErrorSpans || result.HasExceptions || result.HasLogs
	result.Reason = evidenceReason(snapshot, result)
	return result
}

func evidenceReason(snapshot evidence.Snapshot, result EvidenceResult) string {
	if result.Sufficient {
		var found []string
		if result.HasErrorSpans {
			found = append(found, "error spans")
		}
		if result.HasExceptions {
			found = append(found, "exceptions")
		}
		if result.HasLogs {
			found = append(found, "logs")
		}
		reason := "found " + joinWithAnd(found) + " for the incident window"
		if result.Partial {
			// A truncated query still returned real evidence; the
			// reasoner may proceed, but the diagnosis should say the
			// evidence set was not the full picture (e.g. a query hit
			// its row limit).
			reason += " (the query was truncated - a row limit was hit - so this may not be the complete picture)"
		}
		return reason
	}
	if !snapshot.SignalAvailable("traces") {
		return "traces are unavailable from the telemetry source, and no logs or exceptions were found for the incident window"
	}
	return "no error spans, exceptions, or logs were found for the incident window"
}

// anyRecordHasError reports whether any record has a status/error-ish
// field whose value indicates a failure. See recordHasError for the
// heuristic.
func anyRecordHasError(records []evidence.Record) bool {
	for _, r := range records {
		if recordHasError(r) {
			return true
		}
	}
	return false
}

// recordHasError is a deliberately simple heuristic: it looks at every
// field whose name contains "status" or "error" (case-insensitive) - this
// covers common shapes like "status_code", "http.status_code",
// "status_message", "otel_status_code", "error", "hasError" - and treats
// the record as an error span if any such field's value looks like a
// failure (see isErrorValue). Real SigNoz responses vary by
// instrumentation; hardening this against production traces is left for a
// later milestone.
func recordHasError(r evidence.Record) bool {
	for key, value := range r.Fields {
		lower := strings.ToLower(key)
		if !strings.Contains(lower, "status") && !strings.Contains(lower, "error") {
			continue
		}
		if isErrorValue(value) {
			return true
		}
	}
	return false
}

// isErrorValue interprets a status/error-ish field value:
//   - a true boolean is an error,
//   - a numeric HTTP-style status code >= 400 is an error,
//   - the OTel span status code 2 ("STATUS_CODE_ERROR") is an error,
//   - any other non-empty string other than a short list of known "ok"
//     spellings is treated as an error (covers status_message text and
//     free-form error strings).
func isErrorValue(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return isErrorString(v)
	case float64:
		return v >= 400 || v == 2
	case int:
		return v >= 400 || v == 2
	default:
		return false
	}
}

func isErrorString(s string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(s))
	if trimmed == "" {
		return false
	}
	switch trimmed {
	case "ok", "unset", "success", "status_code_ok", "status_code_unset", "no_error", "none":
		return false
	case "error", "err", "failed", "failure", "status_code_error":
		return true
	}
	if n, err := strconv.ParseFloat(trimmed, 64); err == nil {
		return n >= 400 || n == 2
	}
	// Any other non-empty status/error-ish string is treated as an error.
	return true
}

// anyRecordHasException reports whether any record has a field whose name
// contains "exception" (case-insensitive) and a non-empty value. This
// covers OTel exception event attributes such as "exception.message" and
// "exception.type".
func anyRecordHasException(records []evidence.Record) bool {
	for _, r := range records {
		for key, value := range r.Fields {
			if strings.Contains(strings.ToLower(key), "exception") && isNonEmpty(value) {
				return true
			}
		}
	}
	return false
}

func isNonEmpty(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(v) != ""
	case bool:
		return v
	default:
		return true
	}
}

func joinWithAnd(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + ", and " + parts[len(parts)-1]
	}
}
