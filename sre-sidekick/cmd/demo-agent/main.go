// Command demo-agent emits realistic OpenTelemetry traces, metrics, and
// logs for a simulated "support-agent" AI agent, so the rest of
// SRE Sidekick (the SLO engine, telemetry audit, and RCA agent) has a
// live, controllable workload to reason about.
//
// Each loop iteration simulates one agent run and emits:
//
//   - a trace: agent.run -> {model.chat, tool.search_kb, evaluation.groundedness}
//   - two counters: agent_evaluated_answers_total, agent_grounded_answers_total
//   - one log record correlated to the run's trace ID
//
// In healthy mode (the default) every run succeeds and is grounded. In
// --buggy mode, tool.search_kb times out: the span gets an Error status
// and a TimeoutError exception event, the run is not grounded, and
// agent_grounded_answers_total does not increment - this is exactly the
// SLO-burning failure the RCA agent is meant to diagnose.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type config struct {
	endpoint    string
	service     string
	environment string
	interval    time.Duration
	buggy       bool
	errorRate   float64
	runs        int
}

func main() {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	telemetry, err := newTelemetry(ctx, cfg)
	if err != nil {
		slog.Error("initialize telemetry", "error", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := telemetry.Shutdown(shutdownCtx); err != nil {
			slog.Error("shutdown telemetry", "error", err)
		}
	}()

	slog.Info("demo-agent starting",
		"service", cfg.service,
		"environment", cfg.environment,
		"endpoint", cfg.endpoint,
		"interval", cfg.interval,
		"buggy", cfg.buggy,
		"error_rate", cfg.errorRate,
		"runs", cfg.runs)

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()

	count := 0
	for {
		plan := buildRunPlan(cfg.buggy, cfg.errorRate, rng)
		if err := telemetry.runner.execute(ctx, plan); err != nil {
			slog.Error("emit run telemetry", "error", err, "failing", plan.Failing)
		} else {
			count++
			slog.Info("agent run completed", "run", count, "failing", plan.Failing, "grounded", plan.Grounded)
		}

		if cfg.runs > 0 && count >= cfg.runs {
			slog.Info("demo-agent reached run cap", "runs", count)
			return
		}

		select {
		case <-ctx.Done():
			slog.Info("demo-agent shutting down", "runs_completed", count)
			return
		case <-ticker.C:
		}
	}
}

func parseFlags(args []string) (config, error) {
	fs := flag.NewFlagSet("demo-agent", flag.ContinueOnError)
	endpoint := fs.String("endpoint", envOr("OTLP_ENDPOINT", "http://localhost:4318"), "OTLP/HTTP base endpoint")
	service := fs.String("service", "support-agent", "OpenTelemetry service.name")
	environment := fs.String("environment", "local", "deployment.environment")
	interval := fs.Duration("interval", 2*time.Second, "interval between simulated agent runs")
	buggy := fs.Bool("buggy", false, "simulate the tool.search_kb timeout failure mode")
	errorRate := fs.Float64("error-rate", -1, "fraction (0..1) of runs that fail in buggy mode (default 1.0 when --buggy, 0.0 otherwise)")
	runs := fs.Int("runs", 0, "number of runs before exiting (0 = run until interrupted)")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	resolvedErrorRate, err := resolveErrorRate(*errorRate, *buggy)
	if err != nil {
		return config{}, err
	}
	if *interval <= 0 {
		return config{}, fmt.Errorf("interval must be positive, got %s", *interval)
	}
	if *runs < 0 {
		return config{}, fmt.Errorf("runs must be zero or positive, got %d", *runs)
	}

	return config{
		endpoint:    *endpoint,
		service:     *service,
		environment: *environment,
		interval:    *interval,
		buggy:       *buggy,
		errorRate:   resolvedErrorRate,
		runs:        *runs,
	}, nil
}

// telemetry bundles the SDK providers and the runner so main can shut
// everything down (flushing pending spans and metrics) on exit.
type telemetry struct {
	tracerProvider *sdktrace.TracerProvider
	meterProvider  *sdkmetric.MeterProvider
	runner         *runner
}

func newTelemetry(ctx context.Context, cfg config) (*telemetry, error) {
	res := resource.NewSchemaless(
		attribute.String("service.name", cfg.service),
		attribute.String("deployment.environment", cfg.environment),
		attribute.String("service.version", "demo-1.0.0"),
	)

	host, insecure, err := normalizeOTLPEndpoint(cfg.endpoint)
	if err != nil {
		return nil, err
	}

	traceOptions := []otlptracehttp.Option{otlptracehttp.WithEndpoint(host)}
	if insecure {
		traceOptions = append(traceOptions, otlptracehttp.WithInsecure())
	}
	traceExporter, err := otlptracehttp.New(ctx, traceOptions...)
	if err != nil {
		return nil, fmt.Errorf("create OTLP trace exporter: %w", err)
	}
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(traceExporter, sdktrace.WithBatchTimeout(time.Second)),
	)

	metricOptions := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpoint(host)}
	if insecure {
		metricOptions = append(metricOptions, otlpmetrichttp.WithInsecure())
	}
	metricExporter, err := otlpmetrichttp.New(ctx, metricOptions...)
	if err != nil {
		return nil, fmt.Errorf("create OTLP metrics exporter: %w", err)
	}
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter, sdkmetric.WithInterval(time.Second))),
	)

	meter := meterProvider.Meter("sre-sidekick/demo-agent")
	evaluated, err := meter.Int64Counter("agent_evaluated_answers_total",
		metric.WithDescription("Number of agent runs evaluated"))
	if err != nil {
		return nil, fmt.Errorf("create agent_evaluated_answers_total counter: %w", err)
	}
	grounded, err := meter.Int64Counter("agent_grounded_answers_total",
		metric.WithDescription("Number of agent runs that produced a grounded answer"))
	if err != nil {
		return nil, fmt.Errorf("create agent_grounded_answers_total counter: %w", err)
	}

	logsEndpoint, err := logsURL(cfg.endpoint)
	if err != nil {
		return nil, err
	}
	logs := &otlpLogEmitter{
		endpoint:    logsEndpoint,
		service:     cfg.service,
		environment: cfg.environment,
		client:      &http.Client{Timeout: httpClientTimeout},
	}

	return &telemetry{
		tracerProvider: tracerProvider,
		meterProvider:  meterProvider,
		runner: &runner{
			tracer:      tracerProvider.Tracer("sre-sidekick/demo-agent"),
			evaluated:   evaluated,
			grounded:    grounded,
			logs:        logs,
			service:     cfg.service,
			environment: cfg.environment,
		},
	}, nil
}

func (t *telemetry) Shutdown(ctx context.Context) error {
	var errs []error
	if err := t.tracerProvider.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("shutdown trace provider: %w", err))
	}
	if err := t.meterProvider.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("shutdown meter provider: %w", err))
	}
	return errors.Join(errs...)
}

var httpClientTimeout = 5 * time.Second

// normalizeOTLPEndpoint splits an OTLP/HTTP endpoint into the host:port
// pair and TLS setting expected by the OTel SDK exporter options. A bare
// host:port pair (no scheme) is treated as plaintext, matching
// internal/otlp's convention for this codebase.
func normalizeOTLPEndpoint(endpoint string) (host string, insecure bool, err error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", false, fmt.Errorf("OTLP endpoint is required")
	}
	if !strings.Contains(endpoint, "://") {
		return strings.TrimRight(endpoint, "/"), true, nil
	}
	parsed, parseErr := url.Parse(endpoint)
	if parseErr != nil || parsed.Host == "" {
		return "", false, fmt.Errorf("invalid OTLP endpoint %q", endpoint)
	}
	return parsed.Host, parsed.Scheme != "https", nil
}

// logsURL builds the /v1/logs endpoint used by the hand-rolled OTLP/HTTP
// log emitter, from the same base endpoint used for traces and metrics.
func logsURL(endpoint string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", fmt.Errorf("OTLP endpoint is required")
	}
	if !strings.Contains(endpoint, "://") {
		return "http://" + strings.TrimRight(endpoint, "/") + "/v1/logs", nil
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("invalid OTLP endpoint %q", endpoint)
	}
	parsed.Path = "/v1/logs"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
