package rca

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/mcp"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
)

func traceEvidence(id, note string) notify.Evidence {
	return notify.Evidence{ID: id, Kind: notify.EvidenceKindTrace, Note: note}
}

func logEvidence(id, note string) notify.Evidence {
	return notify.Evidence{ID: id, Kind: notify.EvidenceKindLog, Note: note}
}

func TestEvidenceSignals_AllSharingOperationAndException_ConcentrationNearOne(t *testing.T) {
	ev := []notify.Evidence{
		traceEvidence("e1", "operation tool.search_kb, errored: tool.search_kb timed out after 5s, duration=5s"),
		traceEvidence("e2", "operation tool.search_kb, errored: tool.search_kb timed out after 5s, duration=5s"),
		traceEvidence("e3", "operation tool.search_kb, errored: tool.search_kb timed out after 5s, duration=5s"),
		traceEvidence("e4", "operation tool.search_kb, errored: tool.search_kb timed out after 5s, duration=5s"),
		traceEvidence("e5", "operation tool.search_kb, errored: tool.search_kb timed out after 5s, duration=5s"),
	}

	count, c := evidenceSignals(ev)

	if count != 5 {
		t.Errorf("ErrorSampleCount = %d, want 5", count)
	}
	if !c.Present {
		t.Fatal("Concentration.Present = false, want true")
	}
	if c.TopOperation != "tool.search_kb" || c.TopOperationShare != 1.0 {
		t.Errorf("TopOperation/Share = %q/%v, want tool.search_kb/1.0", c.TopOperation, c.TopOperationShare)
	}
	if c.TopException != "TimeoutError" || c.TopExceptionShare != 1.0 {
		t.Errorf("TopException/Share = %q/%v, want TimeoutError/1.0", c.TopException, c.TopExceptionShare)
	}
	if got := c.DominantShare(); got != 1.0 {
		t.Errorf("DominantShare() = %v, want 1.0", got)
	}
}

// TestEvidenceSignals_CascadingFailure_ExceptionShareStillDominant covers
// the real live-demo shape: a downstream span (tool.search_kb) times out
// and its caller (agent.run) also reports a failure, so no single
// operation has a majority share, but every classifiable message shares
// the same TimeoutError signature.
func TestEvidenceSignals_CascadingFailure_ExceptionShareStillDominant(t *testing.T) {
	ev := []notify.Evidence{
		traceEvidence("e1", "operation tool.search_kb, errored: tool.search_kb timed out after 5s, duration=5s"),
		traceEvidence("e2", "operation agent.run, errored: agent run failed, duration=5.31s"),
		traceEvidence("e3", "operation tool.search_kb, errored: tool.search_kb timed out after 5s, duration=5s"),
		traceEvidence("e4", "operation agent.run, errored: agent run failed, duration=5.31s"),
	}

	count, c := evidenceSignals(ev)

	if count != 4 {
		t.Errorf("ErrorSampleCount = %d, want 4", count)
	}
	if c.TopOperationShare != 0.5 {
		t.Errorf("TopOperationShare = %v, want 0.5 (tied 2/4 operations)", c.TopOperationShare)
	}
	if c.TopException != "TimeoutError" || c.TopExceptionShare != 1.0 {
		t.Errorf("TopException/Share = %q/%v, want TimeoutError/1.0 (only classifiable messages count toward the denominator)", c.TopException, c.TopExceptionShare)
	}
	if got := c.DominantShare(); got != 1.0 {
		t.Errorf("DominantShare() = %v, want 1.0 (exception share wins over the tied operation share)", got)
	}
}

func TestEvidenceSignals_MixedUnrelatedErrors_LowerConcentration(t *testing.T) {
	ev := []notify.Evidence{
		traceEvidence("e1", "operation checkout.charge, errored: connection refused, duration=10ms"),
		traceEvidence("e2", "operation checkout.charge, errored: connection refused, duration=10ms"),
		traceEvidence("e3", "operation inventory.reserve, errored: not found, duration=5ms"),
		traceEvidence("e4", "operation shipping.quote, errored: unauthorized, duration=8ms"),
		traceEvidence("e5", "operation billing.tax, errored: rate limit exceeded, duration=2ms"),
	}

	count, c := evidenceSignals(ev)

	if count != 5 {
		t.Errorf("ErrorSampleCount = %d, want 5", count)
	}
	if got := c.DominantShare(); got >= 0.6 {
		t.Errorf("DominantShare() = %v, want < 0.6 for five unrelated failure modes", got)
	}
}

func TestEvidenceSignals_Empty_ZeroSamplesNotPresent(t *testing.T) {
	count, c := evidenceSignals(nil)

	if count != 0 {
		t.Errorf("ErrorSampleCount = %d, want 0", count)
	}
	if c.Present {
		t.Errorf("Concentration.Present = true, want false for no evidence")
	}
}

func TestEvidenceSignals_UnclassifiableGenericMessage_NotCountedAsException(t *testing.T) {
	ev := []notify.Evidence{
		traceEvidence("e1", "operation agent.run, errored: agent run failed, duration=5.31s"),
		traceEvidence("e2", "operation agent.run, errored: agent run failed, duration=5.31s"),
	}

	_, c := evidenceSignals(ev)

	if c.TopException != "" || c.TopExceptionShare != 0 {
		t.Errorf("TopException/Share = %q/%v, want empty/0 - no classifiable exception in this evidence", c.TopException, c.TopExceptionShare)
	}
	if c.TopOperation != "agent.run" || c.TopOperationShare != 1.0 {
		t.Errorf("TopOperation/Share = %q/%v, want agent.run/1.0", c.TopOperation, c.TopOperationShare)
	}
}

func TestEvidenceSignals_LogEvidence_ClassifiedByBody(t *testing.T) {
	ev := []notify.Evidence{
		logEvidence("e1", "[ERROR] request to payment-gateway timed out"),
		logEvidence("e2", "[ERROR] request to payment-gateway timed out"),
	}

	count, c := evidenceSignals(ev)

	if count != 2 {
		t.Errorf("ErrorSampleCount = %d, want 2", count)
	}
	if c.TopException != "TimeoutError" || c.TopExceptionShare != 1.0 {
		t.Errorf("TopException/Share = %q/%v, want TimeoutError/1.0", c.TopException, c.TopExceptionShare)
	}
	if c.TopOperation != "" {
		t.Errorf("TopOperation = %q, want empty - log evidence carries no span name", c.TopOperation)
	}
}

func TestEvidenceSignals_TraceWithoutErroredMessage_NotMisclassifiedFromDuration(t *testing.T) {
	// A bare status-code fallback note (formatTraceNote when there is no
	// status_message) must not have its duration text ("500ms") spuriously
	// matched against the "500" ServerError pattern.
	ev := []notify.Evidence{
		traceEvidence("e1", "operation checkout.charge, status STATUS_CODE_ERROR, duration=500ms"),
	}

	_, c := evidenceSignals(ev)

	if c.TopException != "" {
		t.Errorf("TopException = %q, want empty - duration text must not be classified as an exception", c.TopException)
	}
}

func scalarResult(value float64) mcp.ToolResult {
	text := fmt.Sprintf(`{"status":"success","data":{"data":{"results":[{"queryName":"A","data":[[%v]]}]}}}`, value)
	return textResult(text)
}

func emptyScalarResult() mcp.ToolResult {
	return textResult(`{"status":"success","data":{"data":{"results":[{"queryName":"A","columns":null,"data":[]}]}}}`)
}

// callArgs returns the args of the last call made to tool name, or nil.
func (f *fakeToolCaller) callArgs(name string) map[string]any {
	var last map[string]any
	for _, c := range f.calls {
		if c.name == name {
			last = c.args
		}
	}
	return last
}

// countCalls returns how many times tool name was called.
func (f *fakeToolCaller) countCalls(name string) int {
	n := 0
	for _, c := range f.calls {
		if c.name == name {
			n++
		}
	}
	return n
}

func TestComputeSignals_NilCaller_LeavesMCPSignalsAbsent(t *testing.T) {
	ev := []notify.Evidence{
		traceEvidence("e1", "operation tool.search_kb, errored: tool.search_kb timed out after 5s, duration=5s"),
	}
	inc := baseIncident()

	signals := ComputeSignals(context.Background(), inc, ev, nil, time.Now())

	if signals.ErrorSampleCount != 1 {
		t.Errorf("ErrorSampleCount = %d, want 1", signals.ErrorSampleCount)
	}
	if signals.LatencyShift.Present {
		t.Errorf("LatencyShift.Present = true, want false with a nil caller")
	}
	if signals.ErrorRateDelta.Present {
		t.Errorf("ErrorRateDelta.Present = true, want false with a nil caller")
	}
	if signals.Saturation.Present || signals.DeployOnset.Present {
		t.Errorf("Saturation/DeployOnset must always be absent: no generic source exists for either")
	}
}

func TestComputeSignals_LatencyShift_ComputedFromErrorAndSuccessP99(t *testing.T) {
	// This mirrors the live support-agent demo: failing requests p99 =
	// 5.31s, successful requests p99 = 250ms, ratio ~21.2.
	caller := &fakeToolCaller{results: map[string]mcp.ToolResult{}}
	calls := 0
	fake := &recordingCaller{
		fakeToolCaller: caller,
		onCall: func(args map[string]any) mcp.ToolResult {
			calls++
			if args["error"] == true {
				return scalarResult(5310000000)
			}
			return scalarResult(250000000)
		},
	}
	inc := baseIncident()

	signals := ComputeSignals(context.Background(), inc, nil, fake, time.Now())

	if !signals.LatencyShift.Present {
		t.Fatal("LatencyShift.Present = false, want true")
	}
	want := 5310000000.0 / 250000000.0
	if diff := signals.LatencyShift.Value - want; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("LatencyShift.Value = %v, want %v", signals.LatencyShift.Value, want)
	}
	if calls < 2 {
		t.Errorf("expected at least 2 signoz_aggregate_traces calls for latency shift, got %d", calls)
	}
}

func TestComputeSignals_ToolError_LeavesMetricAbsent(t *testing.T) {
	caller := &fakeToolCaller{errs: map[string]error{"signoz_aggregate_traces": fmt.Errorf("boom")}}
	inc := baseIncident()

	signals := ComputeSignals(context.Background(), inc, nil, caller, time.Now())

	if signals.LatencyShift.Present {
		t.Errorf("LatencyShift.Present = true, want false when the tool call errors")
	}
	if signals.ErrorRateDelta.Present {
		t.Errorf("ErrorRateDelta.Present = true, want false when the tool call errors")
	}
}

func TestComputeSignals_EmptyScalarResult_TreatedAsAbsentNotZero(t *testing.T) {
	caller := &fakeToolCaller{results: map[string]mcp.ToolResult{"signoz_aggregate_traces": emptyScalarResult()}}
	inc := baseIncident()

	signals := ComputeSignals(context.Background(), inc, nil, caller, time.Now())

	if signals.LatencyShift.Present {
		t.Errorf("LatencyShift.Present = true, want false for an empty scalar result")
	}
}

// recordingCaller wraps a fakeToolCaller so latency-shift tests can return
// a different canned value depending on the "error" arg of each call
// (fakeToolCaller alone only supports one canned result per tool name).
type recordingCaller struct {
	*fakeToolCaller
	onCall func(args map[string]any) mcp.ToolResult
}

func (r *recordingCaller) CallTool(ctx context.Context, name string, args map[string]any) (mcp.ToolResult, error) {
	r.fakeToolCaller.calls = append(r.fakeToolCaller.calls, fakeCall{name: name, args: args})
	return r.onCall(args), nil
}
