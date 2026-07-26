// Package telemetry wires OpenTelemetry for the demo services: traces,
// metrics, and correlated logs, all over OTLP/HTTP to the collector the
// sidekick reads from.
//
// Three things here exist deliberately rather than by convenience:
//
//   - The resource always carries service.name, deployment.environment and
//     service.version. Omitting it is easy and the failure is silent: the SDK
//     falls back to "unknown_service:<binary>", which is exactly the defect
//     fixed in the sidekick's own emitter (#41).
//   - Logs are emitted as hand-rolled OTLP/HTTP JSON rather than through a log
//     SDK, matching cmd/demo-agent in sre-sidekick. It keeps the dependency
//     set small and makes the trace correlation explicit: every log record
//     carries the trace and span id of the request that produced it, which is
//     what Track A's required_field rules check for.
//   - A deploy marker span is emitted on startup carrying service.version, so
//     "what changed?" has something to correlate against later.
package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"
	"go.opentelemetry.io/otel/trace"
)

// Version is the service version reported in the resource and in the deploy
// marker. Override at build time with -ldflags "-X ...telemetry.Version=abc123"
// so a real deploy is distinguishable; the default is fine for local runs.
var Version = "dev"

// Config describes one service's telemetry.
type Config struct {
	Service     string
	Environment string
	// Endpoint is the OTLP/HTTP base, e.g. "localhost:4318" or
	// "http://otel-collector:4318". Signals are posted to /v1/{traces,metrics,logs}.
	Endpoint string
}

// Telemetry is the live pipeline for one service.
type Telemetry struct {
	Tracer trace.Tracer
	Logs   *LogEmitter

	// RequestDuration and RequestCount follow the OTel HTTP semantic
	// conventions so an off-the-shelf profile (see
	// sre-sidekick/examples/checkout-api.yaml) applies to this service without
	// bespoke metric names.
	RequestDuration metric.Float64Histogram
	RequestCount    metric.Int64Counter

	tracerProvider *sdktrace.TracerProvider
	meterProvider  *sdkmetric.MeterProvider
}

// Setup builds the providers, installs the W3C trace-context propagator, and
// emits the deploy marker. The returned Telemetry must be shut down to flush.
func Setup(ctx context.Context, cfg Config) (*Telemetry, error) {
	host, insecure, err := normalizeEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}

	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceNameKey.String(cfg.Service),
		semconv.ServiceVersionKey.String(Version),
		semconv.DeploymentEnvironmentNameKey.String(cfg.Environment),
		// Three spellings of the same fact, deliberately.
		//
		// semconv v1.34 renamed this attribute to deployment.environment.name,
		// which is what the key above emits. But SigNoz's own spanmetrics series
		// and every existing SLO config in sre-sidekick/examples filter on the
		// older deployment.environment, and some queries use a bare
		// environment. Emitting one spelling means a filter written against
		// either of the others silently matches nothing - which is exactly how
		// the first version of demo-checkout-slo.yaml reported
		// "0 of 2 dependencies have data" while the metrics were plainly there.
		attribute.String("deployment.environment", cfg.Environment),
		attribute.String("environment", cfg.Environment),
	)

	traceOptions := []otlptracehttp.Option{otlptracehttp.WithEndpoint(host)}
	metricOptions := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpoint(host)}
	if insecure {
		traceOptions = append(traceOptions, otlptracehttp.WithInsecure())
		metricOptions = append(metricOptions, otlpmetrichttp.WithInsecure())
	}

	traceExporter, err := otlptracehttp.New(ctx, traceOptions...)
	if err != nil {
		return nil, fmt.Errorf("create OTLP trace exporter: %w", err)
	}
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(traceExporter, sdktrace.WithBatchTimeout(time.Second)),
	)

	metricExporter, err := otlpmetrichttp.New(ctx, metricOptions...)
	if err != nil {
		return nil, fmt.Errorf("create OTLP metric exporter: %w", err)
	}
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter, sdkmetric.WithInterval(time.Second))),
		// The SDK's default histogram boundaries are the millisecond-shaped set
		// (5, 10, 25, ... 10000). http.server.request.duration is defined in
		// *seconds*, so those defaults put every realistic request in the first
		// bucket and place the next boundary at 5 seconds.
		//
		// That silently breaks any latency SLO: a 1s threshold needs an le=1
		// boundary to compare against, and the default set has none, so the SLI
		// cannot be computed at all and the SLO reports indeterminate while the
		// data looks present. Confirmed live before fixing.
		//
		// These are the boundaries the HTTP semantic conventions recommend.
		sdkmetric.WithView(sdkmetric.NewView(
			sdkmetric.Instrument{Name: "http.server.request.duration"},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
				Boundaries: []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10},
			}},
		)),
	)

	// Global registration so http propagation and any library instrumentation
	// picks these up, and so trace context crosses service boundaries.
	otel.SetTracerProvider(tracerProvider)
	otel.SetMeterProvider(meterProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	meter := meterProvider.Meter(cfg.Service)
	duration, err := meter.Float64Histogram(
		"http.server.request.duration",
		metric.WithDescription("Duration of inbound HTTP requests"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("create request duration histogram: %w", err)
	}
	count, err := meter.Int64Counter(
		"http.server.request.count",
		metric.WithDescription("Number of inbound HTTP requests"),
	)
	if err != nil {
		return nil, fmt.Errorf("create request counter: %w", err)
	}

	t := &Telemetry{
		Tracer:          tracerProvider.Tracer(cfg.Service),
		Logs:            NewLogEmitter(cfg, host, insecure),
		RequestDuration: duration,
		RequestCount:    count,
		tracerProvider:  tracerProvider,
		meterProvider:   meterProvider,
	}
	t.emitDeployMarker(ctx, cfg)
	return t, nil
}

// emitDeployMarker records a short span naming the version that just started.
// It is the cheapest possible change event: no CI integration, no extra
// infrastructure, and it lands in the same trace store the RCA agent already
// reads, so deploy correlation can query it later.
func (t *Telemetry) emitDeployMarker(ctx context.Context, cfg Config) {
	_, span := t.Tracer.Start(ctx, "deploy",
		trace.WithAttributes(
			attribute.String("deploy.service", cfg.Service),
			attribute.String("deploy.version", Version),
			attribute.String("deploy.environment", cfg.Environment),
			attribute.Bool("deploy.marker", true),
		),
	)
	span.End()
}

// Shutdown flushes both pipelines. Errors are joined so one failing exporter
// does not hide the other.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	var problems []string
	// Logs first: draining the queue may still need the network, and there is
	// no point flushing traces before discarding pending log records.
	t.Logs.Close()
	if err := t.tracerProvider.Shutdown(ctx); err != nil {
		problems = append(problems, "traces: "+err.Error())
	}
	if err := t.meterProvider.Shutdown(ctx); err != nil {
		problems = append(problems, "metrics: "+err.Error())
	}
	if len(problems) > 0 {
		return fmt.Errorf("telemetry shutdown: %s", strings.Join(problems, "; "))
	}
	return nil
}

// LogEmitter posts OTLP/HTTP JSON log records. See the package comment for why
// this is hand-rolled rather than an SDK.
type LogEmitter struct {
	endpoint    string
	service     string
	environment string
	client      *http.Client

	// OmitTraceID decides, per record, whether to drop trace correlation. It
	// exists so the demo can break telemetry quality on purpose and give Track
	// A's required_field rule something real to catch.
	//
	// A function rather than a bool, deliberately. An earlier version cached a
	// bool here that the fault controller wrote and every request goroutine
	// read, which was three bugs in one: an unsynchronized data race, a rate
	// that collapsed to 100% because any nonzero rate set the flag, and two
	// copies of the same state that could disagree. Asking the owner of the
	// state per record fixes all three - the controller is already mutex-guarded
	// and already samples the rate.
	//
	// Nil means never omit.
	OmitTraceID func() bool

	// emit is the async queue. Shipping a log record is an HTTP call, and doing
	// it inline held the request open for up to the client timeout, inflating
	// both the span duration and the latency the browser sees relative to the
	// metric the SLO is computed from. Distorting the latency being measured is
	// a poor property for a demo about measuring latency.
	emit chan logJob
	// dropped counts records discarded because the queue was full. Visible in
	// the shutdown log rather than silent.
	dropped atomic.Int64
	wg      sync.WaitGroup
	// closeOnce guards the channel close: Shutdown is deferred in each service's
	// main, and a second call would otherwise panic on a closed channel.
	closeOnce sync.Once
}

// logJob is a record already rendered, so the worker does no formatting and
// holds no request state.
type logJob struct {
	payload []byte
}

const (
	// logQueueDepth bounds memory when the collector is slow. Small on purpose:
	// dropping demo logs is better than growing without limit.
	logQueueDepth = 256
	// logWorkers is how many records ship concurrently.
	logWorkers = 4
)

func NewLogEmitter(cfg Config, host string, insecure bool) *LogEmitter {
	scheme := "https"
	if insecure {
		scheme = "http"
	}
	emitter := &LogEmitter{
		endpoint:    fmt.Sprintf("%s://%s/v1/logs", scheme, host),
		service:     cfg.Service,
		environment: cfg.Environment,
		client:      &http.Client{Timeout: 5 * time.Second},
		emit:        make(chan logJob, logQueueDepth),
	}
	for range logWorkers {
		emitter.wg.Add(1)
		go emitter.ship()
	}
	return emitter
}

// ship drains the queue. Each record gets its own bounded context: the request
// that produced it is likely already finished, and its cancellation must not
// discard the log describing it.
func (e *LogEmitter) ship() {
	defer e.wg.Done()
	for job := range e.emit {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := e.post(ctx, job.payload); err != nil {
			slog.Warn("ship log record", "error", err)
		}
		cancel()
	}
}

// Close drains the queue and stops the workers. Idempotent.
func (e *LogEmitter) Close() {
	e.closeOnce.Do(func() {
		close(e.emit)
		e.wg.Wait()
		if dropped := e.dropped.Load(); dropped > 0 {
			slog.Warn("log records dropped because the queue was full", "count", dropped)
		}
	})
}

// Emit renders one log record, correlated to the span in ctx when there is one,
// and queues it. It does not block on the network: the caller is usually in a
// request path, and a slow collector must not become slow requests.
func (e *LogEmitter) Emit(ctx context.Context, severity, body string, attrs map[string]string) error {
	now := time.Now()
	record := map[string]any{
		"timeUnixNano":         fmt.Sprintf("%d", now.UnixNano()),
		"observedTimeUnixNano": fmt.Sprintf("%d", now.UnixNano()),
		"severityNumber":       severityNumber(severity),
		"severityText":         severity,
		"body":                 stringValue(body),
	}
	attributes := make([]map[string]any, 0, len(attrs))
	for key, value := range attrs {
		attributes = append(attributes, otlpAttribute(key, stringValue(value)))
	}
	if len(attributes) > 0 {
		record["attributes"] = attributes
	}
	omit := e.OmitTraceID != nil && e.OmitTraceID()
	if span := trace.SpanContextFromContext(ctx); span.IsValid() && !omit {
		record["traceId"] = span.TraceID().String()
		record["spanId"] = span.SpanID().String()
	}

	payload := map[string]any{"resourceLogs": []any{map[string]any{
		"resource": map[string]any{"attributes": []any{
			otlpAttribute("service.name", stringValue(e.service)),
			otlpAttribute("deployment.environment", stringValue(e.environment)),
			otlpAttribute("environment", stringValue(e.environment)),
			otlpAttribute("service.version", stringValue(Version)),
		}},
		"scopeLogs": []any{map[string]any{
			"scope":      map[string]any{"name": e.service},
			"logRecords": []any{record},
		}},
	}}}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode OTLP log: %w", err)
	}
	select {
	case e.emit <- logJob{payload: encoded}:
		return nil
	default:
		// Full queue means the collector cannot keep up. Drop and count rather
		// than block a request or grow without bound.
		e.dropped.Add(1)
		return nil
	}
}

// post ships one rendered record.
func (e *LogEmitter) post(ctx context.Context, payload []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create OTLP log request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("send OTLP log: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return fmt.Errorf("OTLP collector returned %s: %s", response.Status, string(message))
	}
	return nil
}

// RelayEndpoint is the collector URL for a given signal, used by the API to
// forward browser spans without exposing the collector to the browser (which
// would need CORS on a deployment we must not modify - PRD non-goal 2).
func RelayEndpoint(endpoint, signal string) (string, error) {
	host, insecure, err := normalizeEndpoint(endpoint)
	if err != nil {
		return "", err
	}
	scheme := "https"
	if insecure {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s/v1/%s", scheme, host, signal), nil
}

func severityNumber(severity string) int {
	switch strings.ToUpper(severity) {
	case "TRACE":
		return 1
	case "DEBUG":
		return 5
	case "INFO":
		return 9
	case "WARN":
		return 13
	case "ERROR":
		return 17
	case "FATAL":
		return 21
	default:
		return 9
	}
}

func otlpAttribute(key string, value map[string]any) map[string]any {
	return map[string]any{"key": key, "value": value}
}

func stringValue(value string) map[string]any {
	return map[string]any{"stringValue": value}
}

// normalizeEndpoint accepts "host:port" or a full URL and returns the host the
// OTLP exporters want plus whether to use plaintext.
func normalizeEndpoint(endpoint string) (host string, insecure bool, err error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", false, fmt.Errorf("OTLP endpoint is required")
	}
	if !strings.Contains(endpoint, "://") {
		return strings.TrimRight(endpoint, "/"), true, nil
	}
	trimmed := strings.TrimRight(endpoint, "/")
	switch {
	case strings.HasPrefix(trimmed, "http://"):
		return strings.TrimPrefix(trimmed, "http://"), true, nil
	case strings.HasPrefix(trimmed, "https://"):
		return strings.TrimPrefix(trimmed, "https://"), false, nil
	default:
		return "", false, fmt.Errorf("invalid OTLP endpoint %q", endpoint)
	}
}
