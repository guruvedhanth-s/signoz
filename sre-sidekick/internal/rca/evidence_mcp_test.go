package rca

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/mcp"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
)

// fakeToolCaller is a test double for MCPToolCaller: it returns canned
// mcp.ToolResult values (or errors) per tool name, and records every call
// made so tests can assert on the arguments passed.
type fakeToolCaller struct {
	results map[string]mcp.ToolResult
	errs    map[string]error
	calls   []fakeCall
}

type fakeCall struct {
	name string
	args map[string]any
}

func (f *fakeToolCaller) CallTool(_ context.Context, name string, args map[string]any) (mcp.ToolResult, error) {
	f.calls = append(f.calls, fakeCall{name: name, args: args})
	if err, ok := f.errs[name]; ok {
		return mcp.ToolResult{}, err
	}
	if result, ok := f.results[name]; ok {
		return result, nil
	}
	return mcp.ToolResult{}, fmt.Errorf("fakeToolCaller: no canned response for tool %q", name)
}

func textResult(text string) mcp.ToolResult {
	return mcp.ToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: text}}}
}

func traceRowsJSON(rows ...string) string {
	return fmt.Sprintf(`{"status":"success","data":{"data":{"results":[{"queryName":"A","rows":[%s]}]}}}`, strings.Join(rows, ","))
}

func traceRow(name, statusMessage, traceID, spanID, webURL string) string {
	return fmt.Sprintf(`{"data":{"name":%q,"service.name":"support-agent","status_code_string":"Error","status_message":%q,"duration_nano":5000000000,"trace_id":%q,"span_id":%q,"webUrl":%q,"has_error":true},"timestamp":"2026-01-01T00:00:00Z"}`,
		name, statusMessage, traceID, spanID, webURL)
}

func logRow(severity, body, traceID string) string {
	return fmt.Sprintf(`{"data":{"severity_text":%q,"body":%q,"trace_id":%q,"span_id":"span-log"},"timestamp":"2026-01-01T00:00:00Z"}`,
		severity, body, traceID)
}

func baseIncident() Incident {
	return Incident{
		CorrelationID: "corr-1",
		Service:       "support-agent",
		Environment:   "local",
		Window:        "1h",
	}
}

func TestMCPEvidenceSource_Gather_MapsTracesAndLogsWithRealLinks(t *testing.T) {
	fake := &fakeToolCaller{
		results: map[string]mcp.ToolResult{
			"signoz_search_traces": textResult(traceRowsJSON(
				traceRow("tool.search_kb", "timed out after 5s", "trace-1", "span-1", "http://signoz/trace/trace-1"),
			)),
			"signoz_get_trace_details": textResult(traceRowsJSON(
				traceRow("tool.search_kb", "timed out after 5s", "trace-1", "span-1", "http://signoz/trace/trace-1"),
			)),
			"signoz_search_logs": textResult(traceRowsJSON(
				logRow("ERROR", "agent run failed: tool.search_kb timed out", "trace-1"),
			)),
		},
	}
	source := &MCPEvidenceSource{Client: fake, Now: func() time.Time { return time.Unix(0, 0) }}

	got, err := source.Gather(context.Background(), baseIncident())
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("expected some evidence, got none")
	}

	var traceEv, logEv *notify.Evidence
	for i := range got {
		switch got[i].Kind {
		case notify.EvidenceKindTrace:
			if traceEv == nil {
				traceEv = &got[i]
			}
		case notify.EvidenceKindLog:
			if logEv == nil {
				logEv = &got[i]
			}
		}
	}
	if traceEv == nil {
		t.Fatal("expected at least one trace evidence item")
	}
	if traceEv.SignozLink != "http://signoz/trace/trace-1" {
		t.Errorf("trace SignozLink = %q, want the real webUrl", traceEv.SignozLink)
	}
	if !strings.Contains(traceEv.Note, "tool.search_kb") {
		t.Errorf("trace Note = %q, want it to mention the operation", traceEv.Note)
	}

	if logEv == nil {
		t.Fatal("expected at least one log evidence item")
	}
	if !strings.Contains(logEv.Note, "agent run failed") {
		t.Errorf("log Note = %q, want it to mention the log body", logEv.Note)
	}
	// Logs have no webUrl in this canned response, so it must fall back to
	// the scoped placeholder link rather than being left empty.
	if logEv.SignozLink == "" || logEv.SignozLink == "http://signoz/trace/trace-1" {
		t.Errorf("log SignozLink = %q, want a non-empty fallback link distinct from the trace's", logEv.SignozLink)
	}

	// IDs must be stable and sequential within the call.
	for i, e := range got {
		wantID := fmt.Sprintf("e%d", i+1)
		if e.ID != wantID {
			t.Errorf("evidence[%d].ID = %q, want %q", i, e.ID, wantID)
		}
	}
}

func TestMCPEvidenceSource_Gather_CallsTraceDetailsForTopTrace(t *testing.T) {
	fake := &fakeToolCaller{
		results: map[string]mcp.ToolResult{
			"signoz_search_traces": textResult(traceRowsJSON(
				traceRow("tool.search_kb", "timed out", "trace-top", "span-1", "http://signoz/trace/trace-top"),
			)),
			"signoz_get_trace_details": textResult(traceRowsJSON(
				traceRow("agent.run", "agent run failed", "trace-top", "span-2", "http://signoz/trace/trace-top"),
			)),
			"signoz_search_logs": textResult(traceRowsJSON()),
		},
	}
	source := &MCPEvidenceSource{Client: fake}

	if _, err := source.Gather(context.Background(), baseIncident()); err != nil {
		t.Fatalf("Gather: %v", err)
	}

	var detailsCall *fakeCall
	for i := range fake.calls {
		if fake.calls[i].name == "signoz_get_trace_details" {
			detailsCall = &fake.calls[i]
			break
		}
	}
	if detailsCall == nil {
		t.Fatal("expected signoz_get_trace_details to be called for the top failing trace")
	}
	if detailsCall.args["traceId"] != "trace-top" {
		t.Errorf("signoz_get_trace_details traceId = %v, want %q", detailsCall.args["traceId"], "trace-top")
	}
}

func TestMCPEvidenceSource_Gather_CapsAtMaxEvidenceItems(t *testing.T) {
	var rows []string
	for i := 0; i < 25; i++ {
		rows = append(rows, traceRow("op", "err", fmt.Sprintf("trace-%d", i), fmt.Sprintf("span-%d", i), fmt.Sprintf("http://signoz/trace/%d", i)))
	}
	fake := &fakeToolCaller{
		results: map[string]mcp.ToolResult{
			"signoz_search_traces":     textResult(traceRowsJSON(rows...)),
			"signoz_get_trace_details": textResult(traceRowsJSON(rows...)),
			"signoz_search_logs":       textResult(traceRowsJSON(rows...)),
		},
	}
	source := &MCPEvidenceSource{Client: fake}

	got, err := source.Gather(context.Background(), baseIncident())
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if len(got) != maxEvidenceItems {
		t.Errorf("len(got) = %d, want capped at %d", len(got), maxEvidenceItems)
	}
}

func TestMCPEvidenceSource_Gather_ToolErrorsDoNotAbortOthers(t *testing.T) {
	fake := &fakeToolCaller{
		errs: map[string]error{
			"signoz_search_traces": errors.New("boom"),
		},
		results: map[string]mcp.ToolResult{
			"signoz_search_logs": textResult(traceRowsJSON(
				logRow("ERROR", "something failed", "trace-x"),
			)),
		},
	}
	source := &MCPEvidenceSource{Client: fake}

	got, err := source.Gather(context.Background(), baseIncident())
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d evidence items, want 1 (only the logs succeeded): %+v", len(got), got)
	}
	if got[0].Kind != notify.EvidenceKindLog {
		t.Errorf("Kind = %v, want %v", got[0].Kind, notify.EvidenceKindLog)
	}
}

func TestMCPEvidenceSource_Gather_NoClientReturnsError(t *testing.T) {
	source := &MCPEvidenceSource{}
	if _, err := source.Gather(context.Background(), baseIncident()); err == nil {
		t.Fatal("expected an error when no Client is configured")
	}
}

func TestMCPEvidenceSource_Gather_MetricOptedIn(t *testing.T) {
	fake := &fakeToolCaller{
		results: map[string]mcp.ToolResult{
			"signoz_search_traces": textResult(traceRowsJSON()),
			"signoz_search_logs":   textResult(traceRowsJSON()),
			"signoz_query_metrics": textResult(`{"status":"success","data":{"data":{"results":[{"queryName":"A","data":[[500]]}]}}}`),
		},
	}
	source := &MCPEvidenceSource{Client: fake, MetricName: "signoz_calls_total"}

	got, err := source.Gather(context.Background(), baseIncident())
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if len(got) != 1 || got[0].Kind != notify.EvidenceKindMetric {
		t.Fatalf("got %+v, want exactly one metric evidence item", got)
	}
	if !strings.Contains(got[0].Note, "signoz_calls_total") {
		t.Errorf("metric Note = %q, want it to mention the metric name", got[0].Note)
	}
}

func TestMCPEvidenceSource_Gather_MetricSkippedWhenNameEmpty(t *testing.T) {
	fake := &fakeToolCaller{
		results: map[string]mcp.ToolResult{
			"signoz_search_traces": textResult(traceRowsJSON()),
			"signoz_search_logs":   textResult(traceRowsJSON()),
		},
	}
	source := &MCPEvidenceSource{Client: fake}

	if _, err := source.Gather(context.Background(), baseIncident()); err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, call := range fake.calls {
		if call.name == "signoz_query_metrics" {
			t.Fatal("signoz_query_metrics should not be called when MetricName is empty")
		}
	}
}

// TestAgent_Diagnose_UsesEvidenceSourceWhenConfigured proves the wiring in
// diagnose.go: when Agent.EvidenceSource is set, Diagnose uses it instead
// of the M1 snapshot-based gatherEvidenceFromSnapshot, without requiring
// any change to how the gate or reasoner are configured.
func TestAgent_Diagnose_UsesEvidenceSourceWhenConfigured(t *testing.T) {
	fake := &fakeToolCaller{
		results: map[string]mcp.ToolResult{
			"signoz_search_traces": textResult(traceRowsJSON(
				traceRow("op", "boom", "trace-1", "span-1", "http://signoz/trace/trace-1"),
			)),
			"signoz_get_trace_details": textResult(traceRowsJSON()),
			"signoz_search_logs":       textResult(traceRowsJSON()),
		},
	}
	gate := stubSufficientGate{}
	agent := &Agent{
		Gate:           gate,
		Reasoner:       StubReasoner{},
		EvidenceSource: &MCPEvidenceSource{Client: fake},
	}

	got, err := agent.Diagnose(context.Background(), baseIncident())
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if len(got.Evidence) != 1 {
		t.Fatalf("Evidence = %+v, want exactly one item from the MCP evidence source", got.Evidence)
	}
	if got.Evidence[0].SignozLink != "http://signoz/trace/trace-1" {
		t.Errorf("SignozLink = %q, want the real webUrl from the MCP tool result", got.Evidence[0].SignozLink)
	}
}

// stubSufficientGate always reports sufficient evidence without a real
// snapshot, to exercise the EvidenceSource-configured path in Diagnose in
// isolation from SourceEvidenceGate.
type stubSufficientGate struct{}

func (stubSufficientGate) Check(context.Context, Incident) (EvidenceResult, error) {
	return EvidenceResult{Sufficient: true}, nil
}
