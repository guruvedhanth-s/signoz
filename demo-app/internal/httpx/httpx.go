// Package httpx is the shared HTTP instrumentation for the demo services:
// one middleware that continues the incoming trace, records the semantic-
// convention request metrics, and writes a correlated log line.
//
// Hand-rolled rather than otelhttp, for two reasons. It keeps the dependency
// set to OTel core, and more importantly it makes the propagation explicit -
// the whole point of the demo is that a trace crosses browser, api and
// payments, so the code that continues that trace should be visible rather
// than buried in a wrapper.
package httpx

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/guruvedhanth-s/signoz/demo-app/internal/telemetry"
)

// statusRecorder captures the status code, which neither the handler nor the
// ResponseWriter will tell us afterwards.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// Instrument wraps a handler with trace continuation, metrics and logging.
// route is the low-cardinality route name; using the raw path would make
// span names and metric labels unbounded, which is the cardinality problem
// Track A has a rule for.
func Instrument(t *telemetry.Telemetry, route string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Continue the caller's trace when it sent traceparent. This is what
		// makes browser -> api -> payments one trace rather than three.
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := t.Tracer.Start(ctx, r.Method+" "+route,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.request.method", r.Method),
				attribute.String("http.route", route),
				attribute.String("url.path", r.URL.Path),
			),
		)
		defer span.End()

		recorder := &statusRecorder{ResponseWriter: w}
		started := time.Now()
		next(recorder, r.WithContext(ctx))
		elapsed := time.Since(started)

		if recorder.status == 0 {
			recorder.status = http.StatusOK
		}
		attrs := metric.WithAttributes(
			attribute.String("http.request.method", r.Method),
			attribute.String("http.route", route),
			attribute.Int("http.response.status_code", recorder.status),
		)
		t.RequestDuration.Record(ctx, elapsed.Seconds(), attrs)
		t.RequestCount.Add(ctx, 1, attrs)

		span.SetAttributes(attribute.Int("http.response.status_code", recorder.status))
		severity := "INFO"
		if recorder.status >= http.StatusInternalServerError {
			// Mark the span as an error so the RCA evidence gate sees an error
			// span, which is one of the three signals it judges sufficiency on.
			span.SetStatus(codes.Error, http.StatusText(recorder.status))
			severity = "ERROR"
		}

		// Best-effort: a failure to ship a log must not fail the request, but it
		// should be visible locally.
		if err := t.Logs.Emit(ctx, severity,
			r.Method+" "+route+" -> "+strconv.Itoa(recorder.status),
			map[string]string{
				"http.route":                route,
				"http.request.method":       r.Method,
				"http.response.status_code": strconv.Itoa(recorder.status),
				"duration_ms":               strconv.FormatInt(elapsed.Milliseconds(), 10),
			},
		); err != nil {
			slog.Warn("emit log record", "error", err, "route", route)
		}
	}
}

// JSON writes a JSON response, and is the only place the demo services set a
// content type, so it cannot drift between handlers.
func JSON(w http.ResponseWriter, status int, encode func() ([]byte, error)) {
	body, err := encode()
	if err != nil {
		http.Error(w, `{"error":"encode failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// Inject copies the current trace context into an outbound request's headers,
// so the downstream service continues the same trace.
func Inject(ctx context.Context, req *http.Request) {
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
}
