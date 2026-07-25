package rca

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/evidence"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
)

// maxEvidenceItems caps how many evidence records the Agent attaches to a
// single diagnosis, so a noisy window does not produce an unusably long
// message.
const maxEvidenceItems = 10

// Agent wires the evidence gate and reasoner together into the RCA
// pipeline: check the gate first, call the reasoner only if the gate finds
// enough signal, then render a notify.Diagnosis with deterministic fields
// injected around the reasoner's free text.
type Agent struct {
	Gate     EvidenceGate
	Reasoner Reasoner
	// EvidenceSource, when set, replaces the M1 gatherEvidence(snapshot)
	// placeholder: Diagnose calls EvidenceSource.Gather(ctx, inc) instead of
	// building evidence from the snapshot the evidence gate fetched. This is
	// the seam MCPEvidenceSource (evidence_mcp.go) plugs into. When nil (the
	// default), M1 behavior is unchanged.
	EvidenceSource EvidenceSource
	// Notifier, when set, delivers every diagnosis this Agent produces -
	// diagnosed or indeterminate - to on-call (PRD section 14) after
	// Diagnose finishes rendering it. Optional: nil (the default) delivers
	// nothing, which keeps M1/M3 callers and tests unaffected. A delivery
	// failure is logged, not returned - a Notifier outage must not make
	// Diagnose itself fail; the diagnosis was still produced correctly.
	Notifier notify.Notifier
	// Now returns the current time; overridable in tests. Defaults to
	// time.Now.
	Now func() time.Time
	// SignalsCaller, when set, lets Diagnose compute the MCP-derived
	// Signals (LatencyShift, ErrorRateDelta - see ComputeSignals in
	// signals.go) the presentation rule engine (Decide/Present,
	// presentation.go) can use, via a small bounded set of targeted
	// signoz_aggregate_traces calls. Optional: nil leaves those two signals
	// Present=false ("unknown") - ErrorSampleCount and Concentration, the
	// signal PRD 13.4 requires to always work, are still always computed
	// from the gathered evidence alone regardless of this field.
	SignalsCaller MCPToolCaller
	// Presentation configures Decide's thresholds (PRD 13.4): how many
	// error samples and how much concentration/latency shift/error-rate
	// delta/saturation it takes to move from CouldNotDetermine to
	// RankedHypotheses to Conclusion. The zero value is not used directly -
	// Decide and Present always apply withDefaults() - so leaving this unset
	// is equivalent to DefaultPresentationConfig(). Callers that load
	// sidekick.yaml (LoadPresentationConfig) should set this explicitly.
	Presentation PresentationConfig
}

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Diagnose runs the RCA pipeline for one incident:
//  1. Check the evidence gate. If it finds insufficient evidence, return an
//     indeterminate diagnosis without ever calling the reasoner - the
//     reasoner must not be asked to explain an incident it has nothing to
//     go on for.
//  2. Otherwise gather evidence from the same snapshot the gate fetched
//     (fetched exactly once per call, see checkGate), call the reasoner,
//     and render the final notify.Diagnosis.
func (a *Agent) Diagnose(ctx context.Context, inc Incident) (notify.Diagnosis, error) {
	// PRD 13.4 rule 1 / notify.Grounding.TelemetryTrusted's own contract:
	// "when false, diagnosis must be StatusIndeterminate". This is checked
	// before the (separate) RCA evidence gate and before the reasoner is
	// ever called: an SLO completeness verdict of "not trusted" means the
	// facts this incident is grounded on cannot be relied on, regardless
	// of what evidence happens to be available for the reasoner to read.
	if !inc.Grounding.TelemetryTrusted {
		diagnosis := a.untrustedTelemetry(inc)
		a.deliver(ctx, diagnosis)
		return diagnosis, nil
	}

	result, snapshot, err := a.checkGate(ctx, inc)
	if err != nil {
		return notify.Diagnosis{}, err
	}
	if !result.Sufficient {
		diagnosis := a.indeterminate(inc, result)
		a.deliver(ctx, diagnosis)
		return diagnosis, nil
	}

	ev, err := a.gatherEvidence(ctx, inc, snapshot)
	if err != nil {
		return notify.Diagnosis{}, fmt.Errorf("rca: gather evidence: %w", err)
	}
	md, err := a.Reasoner.Reason(ctx, inc, ev)
	if err != nil {
		diagnosis := a.reasonerFailed(inc, ev, err)
		a.deliver(ctx, diagnosis)
		return diagnosis, nil
	}

	// PRD 13.4: the model authored md's text, but whether that text is
	// shown as a single conclusion, ranked hypotheses, or suppressed
	// entirely ( "could not determine") is decided here by the
	// deterministic rule engine (Decide), operating on Signals computed
	// from observability data - never by the model's own confidence.
	signals := ComputeSignals(ctx, inc, ev, a.SignalsCaller, a.now())
	mode := Decide(signals, md, a.Presentation)
	diagnosis := Present(inc, ev, md, signals, mode, a.Presentation)
	a.deliver(ctx, diagnosis)
	return diagnosis, nil
}

// deliver sends d to a.Notifier, dispatching to NotifyIndeterminate or
// NotifyDiagnosis based on d.Status. A no-op when a.Notifier is nil. Any
// delivery error is logged with d's CorrelationID (PRD section 20) rather
// than propagated: the diagnosis itself already succeeded, and a chat/voice
// adapter being unreachable is not a reason to fail the diagnose call.
func (a *Agent) deliver(ctx context.Context, d notify.Diagnosis) {
	if a.Notifier == nil {
		return
	}
	var err error
	if d.Status == notify.StatusIndeterminate {
		err = a.Notifier.NotifyIndeterminate(ctx, notify.IndeterminateReason{
			CorrelationID: d.CorrelationID,
			Service:       d.Service,
			Environment:   d.Environment,
			Window:        d.Window,
			Grounding:     d.Grounding,
			Reason:        strings.Join(d.MissingEvidence, "; "),
			Timestamp:     d.Timestamp,
		})
	} else {
		err = a.Notifier.NotifyDiagnosis(ctx, d)
	}
	if err != nil {
		slog.Error("rca: notifier delivery failed", "correlationId", d.CorrelationID, "status", d.Status, "error", err)
	}
}

// checkGate fetches the gate result exactly once per Diagnose call. If the
// configured gate also exposes the snapshot it fetched (SnapshotProvider,
// as SourceEvidenceGate does), that snapshot is reused for evidence
// gathering instead of querying the telemetry source a second time. Gates
// that only implement the base EvidenceGate interface (e.g. simple test
// fakes) still work, just with no snapshot to gather evidence from.
func (a *Agent) checkGate(ctx context.Context, inc Incident) (EvidenceResult, evidence.Snapshot, error) {
	if provider, ok := a.Gate.(SnapshotProvider); ok {
		return provider.CheckWithSnapshot(ctx, inc)
	}
	result, err := a.Gate.Check(ctx, inc)
	return result, evidence.Snapshot{}, err
}

// untrustedTelemetry builds the indeterminate diagnosis for an incident
// whose SLO completeness gate marked the telemetry untrusted. Distinct
// wording from missingEvidence(EvidenceResult) - see couldNotDetermineReason
// in presentation.go for the same rationale - so on-call can tell "we never
// looked because the telemetry was untrustworthy" apart from "we looked and
// found nothing conclusive".
func (a *Agent) untrustedTelemetry(inc Incident) notify.Diagnosis {
	return notify.Diagnosis{
		CorrelationID:   inc.CorrelationID,
		Service:         inc.Service,
		Environment:     inc.Environment,
		Window:          inc.Window,
		Status:          notify.StatusIndeterminate,
		Grounding:       inc.Grounding,
		MissingEvidence: []string{"SLO completeness gate did not trust the telemetry for this window; refusing to diagnose"},
		Timestamp:       a.now(),
	}
}

// reasonerFailed builds the indeterminate diagnosis for an incident whose
// Reasoner could not produce a diagnosis: a transport failure, an LLM
// outage, or (see reasoner_llm.go's loop) a response that never became
// parseable JSON even after a retry. A reasoner failure must never
// silently produce nothing - on-call would see no message at all for a
// real incident - and must never be mistaken for a real diagnosis just
// because some ModelDiagnosis-shaped fallback text existed. The evidence
// already gathered is still attached, so a human reviewing this
// indeterminate result is not left with nothing to look at.
func (a *Agent) reasonerFailed(inc Incident, ev []notify.Evidence, err error) notify.Diagnosis {
	return notify.Diagnosis{
		CorrelationID:   inc.CorrelationID,
		Service:         inc.Service,
		Environment:     inc.Environment,
		Window:          inc.Window,
		Status:          notify.StatusIndeterminate,
		Grounding:       inc.Grounding,
		Evidence:        ev,
		MissingEvidence: []string{fmt.Sprintf("the reasoning agent could not produce a diagnosis: %v", err)},
		Timestamp:       a.now(),
	}
}

func (a *Agent) indeterminate(inc Incident, result EvidenceResult) notify.Diagnosis {
	return notify.Diagnosis{
		CorrelationID:   inc.CorrelationID,
		Service:         inc.Service,
		Environment:     inc.Environment,
		Window:          inc.Window,
		Status:          notify.StatusIndeterminate,
		Grounding:       inc.Grounding,
		MissingEvidence: missingEvidence(result),
		Timestamp:       a.now(),
	}
}

// missingEvidence names what the gate found missing, for
// notify.Diagnosis.MissingEvidence.
func missingEvidence(result EvidenceResult) []string {
	var missing []string
	if !result.HasErrorSpans {
		missing = append(missing, "error spans")
	}
	if !result.HasExceptions {
		missing = append(missing, "exceptions")
	}
	if !result.HasLogs {
		missing = append(missing, "logs")
	}
	if result.Reason != "" {
		missing = append(missing, result.Reason)
	}
	return missing
}

// gatherEvidence dispatches to a.EvidenceSource when one is configured
// (e.g. MCPEvidenceSource, which calls live SigNoz MCP tools), and falls
// back to the M1 snapshot-based placeholder otherwise. This is the seam
// that lets Milestone 3 replace M1's evidence gathering without touching
// any caller of Agent.Diagnose.
func (a *Agent) gatherEvidence(ctx context.Context, inc Incident, snapshot evidence.Snapshot) ([]notify.Evidence, error) {
	if a.EvidenceSource != nil {
		return a.EvidenceSource.Gather(ctx, inc)
	}
	return gatherEvidenceFromSnapshot(inc, snapshot), nil
}

// gatherEvidenceFromSnapshot turns a snapshot into the notify.Evidence list
// a diagnosis can cite. Evidence ids ("e1", "e2", ...) are only stable
// within one Diagnose call - Render only needs ids that resolve within the
// same diagnosis, not across calls.
//
// SignozLink is a placeholder built from the incident's
// service/environment/window, not a real deep link to the query or record
// that produced each piece of evidence: building a precise deep link
// requires the SigNoz MCP server (see MCPEvidenceSource in
// evidence_mcp.go, which does exactly that) which knows how to construct a
// link to the exact query. Documented here so it is not mistaken for a
// precise link. This function only runs when no EvidenceSource is
// configured on Agent.
func gatherEvidenceFromSnapshot(inc Incident, snapshot evidence.Snapshot) []notify.Evidence {
	var out []notify.Evidence
	add := func(kind notify.EvidenceKind, records []evidence.Record) {
		for _, r := range records {
			if len(out) >= maxEvidenceItems {
				return
			}
			out = append(out, notify.Evidence{
				ID:         fmt.Sprintf("e%d", len(out)+1),
				Kind:       kind,
				SignozLink: placeholderLink(inc),
				Note:       evidenceNote(kind, r),
			})
		}
	}
	add(notify.EvidenceKindTrace, snapshot.Traces)
	add(notify.EvidenceKindLog, snapshot.Logs)
	add(notify.EvidenceKindMetric, snapshot.Metrics)
	return out
}

// evidenceNote builds the Note for one snapshot-derived evidence item.
// r.Selector comes straight from a raw telemetry field (name, span_name,
// metric_name, operation_name - see normalizeSignal in
// internal/source/signoz/telemetry.go) written by the instrumented
// application, so it is exactly as attacker-controllable as any other
// telemetry text: this is the same class of injection risk sanitize.go
// exists to defend against for the MCP evidence path
// (evidence_mcp.go), but this M1/default path (used whenever
// Agent.EvidenceSource is unset - i.e. whenever --mcp-url is not
// configured) built its Note directly from the raw field with no
// sanitization at all. SanitizeNote strips control characters and caps
// the length, same as every other evidence Note in this package.
func evidenceNote(kind notify.EvidenceKind, r evidence.Record) string {
	if r.Selector != "" {
		return SanitizeNote(fmt.Sprintf("%s: %s", kind, r.Selector))
	}
	return string(kind)
}

// placeholderLink builds a best-effort SigNoz link scoped to the
// incident's service, environment, and window. It does not point at the
// exact query or record behind a piece of evidence - that requires the
// SigNoz MCP integration (a later milestone) which can construct a deep
// link per query. Left as a placeholder here so nobody mistakes it for a
// precise deep link.
func placeholderLink(inc Incident) string {
	return fmt.Sprintf("signoz://services/%s?environment=%s&window=%s", inc.Service, inc.Environment, inc.Window)
}
