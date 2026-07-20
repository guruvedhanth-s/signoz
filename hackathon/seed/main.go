// Command seed emits deterministic agent telemetry into SigNoz via OTLP so the
// SLO engine has data to evaluate.
//
// It writes two gauges that the example SLO (successful-agent-runs) reads:
//
//	agent_requests_total  (total agent runs)
//	agent_errors_total    (failed agent runs)
//
// With the defaults (10000 requests, 50 errors) the ratio SLI is
// (10000-50)/10000 = 0.995, which clears the 0.99 target, so the SLO flips from
// "indeterminate" (no data) to "healthy".
//
// Usage:
//
//	go run ./hackathon/seed --requests 10000 --errors 50 --service support-agent
//
// Point --endpoint at the collector's OTLP HTTP receiver (default localhost:4318).
package main

import (
	"context"
	"flag"
	"log"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

func main() {
	endpoint := flag.String("endpoint", "localhost:4318", "OTLP HTTP endpoint of the collector")
	service := flag.String("service", "support-agent", "service.name resource attribute")
	requests := flag.Float64("requests", 10000, "value for agent_requests_total")
	errorsCount := flag.Float64("errors", 50, "value for agent_errors_total")
	rounds := flag.Int("rounds", 6, "how many times to emit (spreads samples so range queries have points)")
	interval := flag.Duration("interval", 2*time.Second, "delay between rounds")
	flag.Parse()

	ctx := context.Background()

	exporter, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpoint(*endpoint),
		otlpmetrichttp.WithInsecure(),
	)
	if err != nil {
		log.Fatalf("create exporter: %v", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(*service)),
	)
	if err != nil {
		log.Fatalf("create resource: %v", err)
	}

	reader := sdkmetric.NewPeriodicReader(exporter,
		sdkmetric.WithInterval(1*time.Second),
	)
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(reader),
	)
	meter := provider.Meter("hackathon/seed")

	reqGauge, err := meter.Float64Gauge("agent_requests_total",
		metric.WithDescription("Total agent runs (seeded)."))
	if err != nil {
		log.Fatalf("create agent_requests_total: %v", err)
	}
	errGauge, err := meter.Float64Gauge("agent_errors_total",
		metric.WithDescription("Failed agent runs (seeded)."))
	if err != nil {
		log.Fatalf("create agent_errors_total: %v", err)
	}
	// agent_success_total is emitted directly (requests-errors) so the ratio SLI
	// can use single-vector PromQL queries and avoid instant vector matching.
	successGauge, err := meter.Float64Gauge("agent_success_total",
		metric.WithDescription("Successful agent runs (seeded)."))
	if err != nil {
		log.Fatalf("create agent_success_total: %v", err)
	}

	attrs := metric.WithAttributes(attribute.String("service.name", *service))

	for i := 0; i < *rounds; i++ {
		reqGauge.Record(ctx, *requests, attrs)
		errGauge.Record(ctx, *errorsCount, attrs)
		successGauge.Record(ctx, *requests-*errorsCount, attrs)

		if err := provider.ForceFlush(ctx); err != nil {
			log.Fatalf("force flush: %v", err)
		}
		log.Printf("round %d/%d: agent_requests_total=%.0f agent_errors_total=%.0f service=%s",
			i+1, *rounds, *requests, *errorsCount, *service)

		if i < *rounds-1 {
			time.Sleep(*interval)
		}
	}

	if err := provider.Shutdown(ctx); err != nil {
		log.Fatalf("shutdown: %v", err)
	}
	log.Printf("done. SLI should be (%.0f-%.0f)/%.0f = %.4f",
		*requests, *errorsCount, *requests, (*requests-*errorsCount)/(*requests))
}
