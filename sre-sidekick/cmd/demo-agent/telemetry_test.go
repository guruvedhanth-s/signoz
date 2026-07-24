package main

import (
	"context"
	"math/rand"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// fakeLogSink records every log entry it is asked to emit, so tests can
// assert on trace correlation and severity without a live collector.
type fakeLogSink struct {
	entries []logEntry
}

func (f *fakeLogSink) Emit(_ context.Context, entry logEntry) error {
	f.entries = append(f.entries, entry)
	return nil
}

// testHarness wires a runner to in-memory span and metric recorders.
type testHarness struct {
	spanExporter *tracetest.InMemoryExporter
	reader       *sdkmetric.ManualReader
	logs         *fakeLogSink
	runner       *runner
}

func newTestHarness(t *testing.T) *testHarness {
	t.Helper()

	spanExporter := tracetest.NewInMemoryExporter()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spanExporter))
	t.Cleanup(func() { _ = tracerProvider.Shutdown(context.Background()) })

	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = meterProvider.Shutdown(context.Background()) })

	meter := meterProvider.Meter("test")
	evaluated, err := meter.Int64Counter("agent_evaluated_answers_total")
	if err != nil {
		t.Fatalf("create evaluated counter: %v", err)
	}
	grounded, err := meter.Int64Counter("agent_grounded_answers_total")
	if err != nil {
		t.Fatalf("create grounded counter: %v", err)
	}

	logs := &fakeLogSink{}
	return &testHarness{
		spanExporter: spanExporter,
		reader:       reader,
		logs:         logs,
		runner: &runner{
			tracer:    tracerProvider.Tracer("test"),
			evaluated: evaluated,
			grounded:  grounded,
			logs:      logs,
		},
	}
}

func (h *testHarness) counterValue(t *testing.T, name string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("metric %s has unexpected data type %T", name, m.Data)
			}
			var total int64
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}
			return total
		}
	}
	return 0
}

func spanByName(spans tracetest.SpanStubs, name string) *tracetest.SpanStub {
	for i := range spans {
		if spans[i].Name == name {
			return &spans[i]
		}
	}
	return nil
}

func attrString(t *testing.T, span *tracetest.SpanStub, key string) (string, bool) {
	t.Helper()
	for _, kv := range span.Attributes {
		if string(kv.Key) == key {
			return kv.Value.AsString(), true
		}
	}
	return "", false
}

func TestExecuteHealthyRun(t *testing.T) {
	h := newTestHarness(t)
	rng := rand.New(rand.NewSource(42))
	plan := buildRunPlan(false, 0, rng)

	if err := h.runner.execute(context.Background(), plan); err != nil {
		t.Fatalf("execute: %v", err)
	}

	spans := h.spanExporter.GetSpans()
	if len(spans) != 4 {
		t.Fatalf("expected 4 spans (root + 3 children), got %d: %+v", len(spans), spans)
	}

	root := spanByName(spans, "agent.run")
	model := spanByName(spans, "model.chat")
	tool := spanByName(spans, "tool.search_kb")
	eval := spanByName(spans, "evaluation.groundedness")
	if root == nil || model == nil || tool == nil || eval == nil {
		t.Fatalf("expected all four spans to be present, got %+v", spans)
	}

	for _, child := range []*tracetest.SpanStub{model, tool, eval} {
		if child.Parent.SpanID() != root.SpanContext.SpanID() {
			t.Fatalf("expected %s to be a child of agent.run", child.Name)
		}
	}

	if root.Status.Code != codes.Ok {
		t.Fatalf("expected root span status Ok, got %v", root.Status.Code)
	}
	if tool.Status.Code != codes.Ok {
		t.Fatalf("expected tool.search_kb status Ok, got %v", tool.Status.Code)
	}
	if len(tool.Events) != 0 {
		t.Fatalf("healthy tool.search_kb must not have an exception event, got %+v", tool.Events)
	}

	if provider, ok := attrString(t, model, "gen_ai.provider.name"); !ok || provider == "" {
		t.Fatalf("expected gen_ai.provider.name attribute, got ok=%v value=%q", ok, provider)
	}
	if reqModel, ok := attrString(t, model, "gen_ai.request.model"); !ok || reqModel == "" {
		t.Fatalf("expected gen_ai.request.model attribute, got ok=%v value=%q", ok, reqModel)
	}

	grounded, ok := false, false
	for _, kv := range eval.Attributes {
		if string(kv.Key) == "evaluation.grounded" {
			grounded, ok = kv.Value.AsBool(), true
		}
	}
	if !ok || !grounded {
		t.Fatalf("expected evaluation.grounded=true, got ok=%v value=%v", ok, grounded)
	}

	if got := h.counterValue(t, "agent_evaluated_answers_total"); got != 1 {
		t.Fatalf("expected agent_evaluated_answers_total=1, got %d", got)
	}
	if got := h.counterValue(t, "agent_grounded_answers_total"); got != 1 {
		t.Fatalf("expected agent_grounded_answers_total=1, got %d", got)
	}

	if len(h.logs.entries) != 1 {
		t.Fatalf("expected one log entry, got %d", len(h.logs.entries))
	}
	entry := h.logs.entries[0]
	if entry.Severity != logSeverityInfo {
		t.Fatalf("expected INFO log severity, got %q", entry.Severity)
	}
	if entry.TraceID == "" || entry.TraceID != root.SpanContext.TraceID().String() {
		t.Fatalf("expected log trace ID to match the root span, got %q vs %q", entry.TraceID, root.SpanContext.TraceID().String())
	}
	if entry.Failing {
		t.Fatalf("healthy run must not mark the log entry as failing")
	}
}

func TestExecuteBuggyRun(t *testing.T) {
	h := newTestHarness(t)
	rng := rand.New(rand.NewSource(7))
	plan := buildRunPlan(true, 1.0, rng)

	if err := h.runner.execute(context.Background(), plan); err != nil {
		t.Fatalf("execute: %v", err)
	}

	spans := h.spanExporter.GetSpans()
	root := spanByName(spans, "agent.run")
	tool := spanByName(spans, "tool.search_kb")
	eval := spanByName(spans, "evaluation.groundedness")
	if root == nil || tool == nil || eval == nil {
		t.Fatalf("expected all spans to be present, got %+v", spans)
	}

	if tool.Status.Code != codes.Error {
		t.Fatalf("expected tool.search_kb status Error, got %v", tool.Status.Code)
	}
	if root.Status.Code != codes.Error {
		t.Fatalf("expected agent.run status Error, got %v", root.Status.Code)
	}

	if len(tool.Events) != 1 {
		t.Fatalf("expected exactly one exception event on tool.search_kb, got %+v", tool.Events)
	}
	event := tool.Events[0]
	if event.Name != "exception" {
		t.Fatalf("expected an 'exception' span event, got %q", event.Name)
	}
	attrs := map[string]string{}
	for _, kv := range event.Attributes {
		attrs[string(kv.Key)] = kv.Value.AsString()
	}
	if attrs["exception.type"] != "TimeoutError" {
		t.Fatalf("expected exception.type=TimeoutError, got %q", attrs["exception.type"])
	}
	if attrs["exception.message"] == "" {
		t.Fatalf("expected a non-empty exception.message")
	}
	if attrs["exception.stacktrace"] == "" {
		t.Fatalf("expected a non-empty exception.stacktrace")
	}

	grounded, ok := true, false
	for _, kv := range eval.Attributes {
		if string(kv.Key) == "evaluation.grounded" {
			grounded, ok = kv.Value.AsBool(), true
		}
	}
	if !ok || grounded {
		t.Fatalf("expected evaluation.grounded=false, got ok=%v value=%v", ok, grounded)
	}

	if got := h.counterValue(t, "agent_evaluated_answers_total"); got != 1 {
		t.Fatalf("expected agent_evaluated_answers_total=1, got %d", got)
	}
	if got := h.counterValue(t, "agent_grounded_answers_total"); got != 0 {
		t.Fatalf("expected agent_grounded_answers_total=0 for a buggy run, got %d", got)
	}

	if len(h.logs.entries) != 1 {
		t.Fatalf("expected one log entry, got %d", len(h.logs.entries))
	}
	entry := h.logs.entries[0]
	if entry.Severity != logSeverityError {
		t.Fatalf("expected ERROR log severity, got %q", entry.Severity)
	}
	if !entry.Failing {
		t.Fatalf("buggy run must mark the log entry as failing")
	}
	if entry.TraceID != root.SpanContext.TraceID().String() {
		t.Fatalf("expected log trace ID to match the root span")
	}
}

func TestExecuteAcrossManyRunsCountersMatchOutcomes(t *testing.T) {
	h := newTestHarness(t)
	rng := rand.New(rand.NewSource(99))

	const runs = 25
	evaluatedWant := int64(0)
	groundedWant := int64(0)
	for i := 0; i < runs; i++ {
		plan := buildRunPlan(true, 0.4, rng)
		if err := h.runner.execute(context.Background(), plan); err != nil {
			t.Fatalf("execute run %d: %v", i, err)
		}
		evaluatedWant++
		if plan.Grounded {
			groundedWant++
		}
	}

	if got := h.counterValue(t, "agent_evaluated_answers_total"); got != evaluatedWant {
		t.Fatalf("expected agent_evaluated_answers_total=%d, got %d", evaluatedWant, got)
	}
	if got := h.counterValue(t, "agent_grounded_answers_total"); got != groundedWant {
		t.Fatalf("expected agent_grounded_answers_total=%d, got %d", groundedWant, got)
	}
	if groundedWant == evaluatedWant {
		t.Fatalf("test is not exercising both outcomes; adjust the seed or error rate")
	}
}
