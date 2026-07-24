package main

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"
	"go.opentelemetry.io/otel/trace"
)

// Model call and evaluation spans are quick; only tool.search_kb varies
// between the healthy and buggy modes.
const (
	modelChatDuration      = 250 * time.Millisecond
	evaluationSpanDuration = 60 * time.Millisecond
)

// logSink emits one log record correlated to the run's trace.
type logSink interface {
	Emit(ctx context.Context, entry logEntry) error
}

// logEntry is the correlated log record produced by a run.
type logEntry struct {
	TraceID  string
	SpanID   string
	Severity string
	Body     string
	Failing  bool
}

// runner executes a single simulated agent run against real OTel SDK
// instruments: it builds the agent.run span tree (model.chat,
// tool.search_kb, evaluation.groundedness), increments the SLI counters,
// and emits the correlated log.
type runner struct {
	tracer    trace.Tracer
	evaluated metric.Int64Counter
	grounded  metric.Int64Counter
	logs      logSink
	// service and environment are attached as metric datapoint attributes
	// (service_name / environment) so the SLO engine's PromQL label matchers
	// can scope to them. OTel resource attributes alone are not exposed as
	// PromQL labels by SigNoz, so the SLI queries would match nothing.
	service     string
	environment string
}

// execute runs one simulated agent run according to plan. It returns an
// error only if the correlated log failed to send; span and metric
// recording never fail.
func (r *runner) execute(ctx context.Context, plan RunPlan) error {
	runStart := time.Now()
	ctx, root := r.tracer.Start(ctx, "agent.run", trace.WithTimestamp(runStart))

	modelStart := runStart
	modelEnd := modelStart.Add(modelChatDuration)
	_, modelSpan := r.tracer.Start(ctx, "model.chat",
		trace.WithTimestamp(modelStart),
		trace.WithAttributes(
			attribute.String("gen_ai.provider.name", plan.ModelProvider),
			attribute.String("gen_ai.request.model", plan.ModelName),
			attribute.Int64("gen_ai.usage.input_tokens", plan.InputTokens),
			attribute.Int64("gen_ai.usage.output_tokens", plan.OutputTokens),
		),
	)
	modelSpan.End(trace.WithTimestamp(modelEnd))

	toolStart := modelEnd
	toolEnd := toolStart.Add(plan.ToolDuration)
	_, toolSpan := r.tracer.Start(ctx, "tool.search_kb", trace.WithTimestamp(toolStart))
	if plan.Failing {
		toolSpan.AddEvent(semconv.ExceptionEventName,
			trace.WithTimestamp(toolEnd),
			trace.WithAttributes(
				semconv.ExceptionType(plan.ExceptionType),
				semconv.ExceptionMessage(plan.ExceptionMessage),
				semconv.ExceptionStacktrace(plan.ExceptionStack),
			),
		)
		toolSpan.SetStatus(codes.Error, plan.ExceptionMessage)
	} else {
		toolSpan.SetStatus(codes.Ok, "")
	}
	toolSpan.End(trace.WithTimestamp(toolEnd))

	evalStart := toolEnd
	evalEnd := evalStart.Add(evaluationSpanDuration)
	_, evalSpan := r.tracer.Start(ctx, "evaluation.groundedness",
		trace.WithTimestamp(evalStart),
		trace.WithAttributes(attribute.Bool("evaluation.grounded", plan.Grounded)),
	)
	evalSpan.End(trace.WithTimestamp(evalEnd))

	if plan.Failing {
		root.SetStatus(codes.Error, "agent run failed")
	} else {
		root.SetStatus(codes.Ok, "")
	}
	root.End(trace.WithTimestamp(evalEnd))

	sliAttrs := metric.WithAttributes(
		attribute.String("service_name", r.service),
		attribute.String("environment", r.environment),
	)
	r.evaluated.Add(ctx, 1, sliAttrs)
	if plan.Grounded {
		r.grounded.Add(ctx, 1, sliAttrs)
	}

	spanContext := root.SpanContext()
	return r.logs.Emit(ctx, logEntry{
		TraceID:  spanContext.TraceID().String(),
		SpanID:   spanContext.SpanID().String(),
		Severity: plan.LogSeverity,
		Body:     plan.LogBody,
		Failing:  plan.Failing,
	})
}
