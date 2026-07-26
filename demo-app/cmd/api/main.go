// api is the demo's backend: it serves the frontend, handles checkout by
// calling payments, relays browser spans to the collector, and owns the fault
// controls the UI drives.
//
// The trace relay deserves a note. The browser could post spans straight to the
// collector, and OTLP/HTTP accepts JSON, but the collector would then need CORS
// headers - and configuring that means editing the Foundry-generated SigNoz
// deployment, which PRD non-goal 2 forbids. Relaying through this service is
// same-origin, needs no collector change, and keeps the browser's spans real
// OTLP rather than something bespoke.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/guruvedhanth-s/signoz/demo-app/internal/faults"
	"github.com/guruvedhanth-s/signoz/demo-app/internal/httpx"
	"github.com/guruvedhanth-s/signoz/demo-app/internal/telemetry"
)

// paymentsTimeout is deliberately shorter than the hang payments injects, so
// the timeout fault produces a caller-side failure and a long downstream span.
const paymentsTimeout = 3 * time.Second

func main() {
	listen := flag.String("listen", envOr("API_LISTEN", "127.0.0.1:8090"), "HTTP listen address")
	paymentsURL := flag.String("payments-url", envOr("PAYMENTS_URL", "http://127.0.0.1:8091"), "payments service base URL")
	otlpEndpoint := flag.String("otlp-endpoint", envOr("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4318"), "OTLP/HTTP endpoint")
	environment := flag.String("environment", envOr("DEMO_ENVIRONMENT", "local"), "deployment environment")
	webDir := flag.String("web-dir", envOr("WEB_DIR", "web"), "directory containing the frontend")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	t, err := telemetry.Setup(ctx, telemetry.Config{
		Service:     "checkout-api",
		Environment: *environment,
		Endpoint:    *otlpEndpoint,
	})
	if err != nil {
		slog.Error("setup telemetry", "error", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := t.Shutdown(shutdownCtx); err != nil {
			slog.Warn("telemetry shutdown", "error", err)
		}
	}()

	traceRelay, err := telemetry.RelayEndpoint(*otlpEndpoint, "traces")
	if err != nil {
		slog.Error("resolve trace relay endpoint", "error", err)
		os.Exit(1)
	}

	controller := faults.New()
	controller.OnChange(func(mode faults.Mode, rate float64) {
		if mode == faults.LogsMissingTraceID {
			t.Logs.SetOmitTraceID(rate > 0)
		}
	})

	// Business counters for the availability SLO. The engine's ratio SLI needs
	// two distinct metric names (good and total), which cannot be derived from
	// one status-labelled counter - and an order-level SLI is what the business
	// actually cares about, not HTTP status codes.
	meter := otel.Meter("checkout-api")
	ordersTotal, err := meter.Int64Counter("checkout_orders_total",
		metric.WithDescription("Checkout attempts"))
	if err != nil {
		slog.Error("create orders counter", "error", err)
		os.Exit(1)
	}
	ordersConfirmed, err := meter.Int64Counter("checkout_orders_confirmed_total",
		metric.WithDescription("Checkouts that completed successfully"))
	if err != nil {
		slog.Error("create confirmed orders counter", "error", err)
		os.Exit(1)
	}

	client := &http.Client{Timeout: paymentsTimeout}
	relayClient := &http.Client{Timeout: 5 * time.Second}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, http.StatusOK, func() ([]byte, error) { return json.Marshal(map[string]string{"status": "ok"}) })
	})
	mux.HandleFunc("POST /api/checkout", httpx.Instrument(t, "/api/checkout",
		checkout(t, controller, client, *paymentsURL, ordersTotal, ordersConfirmed)))
	mux.HandleFunc("GET /api/fault", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, http.StatusOK, func() ([]byte, error) {
			return json.Marshal(map[string]any{"modes": faults.AllModes(), "state": controller.Snapshot()})
		})
	})
	mux.HandleFunc("POST /api/fault", faultHandler(controller, client, *paymentsURL))
	mux.HandleFunc("POST /api/traces", relayTraces(relayClient, traceRelay))
	mux.Handle("GET /", http.FileServer(http.Dir(*webDir)))

	server := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	slog.Info("checkout-api listening",
		"address", *listen, "payments", *paymentsURL, "environment", *environment, "version", telemetry.Version)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("api server", "error", err)
		os.Exit(1)
	}
	slog.Info("checkout-api stopped")
}

// checkout is the operation the SLOs are written against: it calls payments and
// reports whether the order went through.
func checkout(
	t *telemetry.Telemetry,
	controller *faults.Controller,
	client *http.Client,
	paymentsURL string,
	ordersTotal metric.Int64Counter,
	ordersConfirmed metric.Int64Counter,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		// Counted before anything can fail, so "total" really is every attempt.
		// Counting it late would quietly exclude the failures and make the SLI
		// look better exactly when it should look worse.
		defer func() { ordersTotal.Add(ctx, 1) }()
		var request struct {
			OrderID string  `json:"order_id"`
			Amount  float64 `json:"amount"`
		}
		// A missing body is fine - the UI posts one, curl users may not.
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request)
		if request.OrderID == "" {
			request.OrderID = "order-" + time.Now().UTC().Format("150405.000")
		}
		if request.Amount == 0 {
			request.Amount = 42.50
		}

		ctx, span := t.Tracer.Start(ctx, "checkout.process",
			trace.WithAttributes(attribute.String("order.id", request.OrderID)),
		)
		defer span.End()

		payload, err := json.Marshal(map[string]any{"order_id": request.OrderID, "amount": request.Amount})
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			httpx.JSON(w, http.StatusInternalServerError, func() ([]byte, error) {
				return json.Marshal(map[string]string{"error": "encode payment request"})
			})
			return
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, paymentsURL+"/charge", bytes.NewReader(payload))
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			httpx.JSON(w, http.StatusInternalServerError, func() ([]byte, error) {
				return json.Marshal(map[string]string{"error": "build payment request"})
			})
			return
		}
		req.Header.Set("Content-Type", "application/json")
		// This is the line that makes payments part of the same trace.
		httpx.Inject(ctx, req)

		response, err := client.Do(req)
		if err != nil {
			// The timeout fault lands here: payments is still hanging, and this
			// is the caller giving up.
			span.SetStatus(codes.Error, "payments unreachable: "+err.Error())
			span.RecordError(err)
			span.SetAttributes(attribute.String("failure.dependency", "payments"))
			httpx.JSON(w, http.StatusBadGateway, func() ([]byte, error) {
				return json.Marshal(map[string]any{
					"error": "payment authorization failed", "dependency": "payments", "order_id": request.OrderID,
				})
			})
			return
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))

		if response.StatusCode >= http.StatusInternalServerError {
			// Attribute the failure to the dependency, not to this service.
			// This attribute is what makes "api is failing because payments is"
			// visible in the evidence rather than inferred.
			span.SetStatus(codes.Error, "payments returned "+response.Status)
			span.SetAttributes(
				attribute.String("failure.dependency", "payments"),
				attribute.Int("payments.status_code", response.StatusCode),
			)
			httpx.JSON(w, http.StatusBadGateway, func() ([]byte, error) {
				return json.Marshal(map[string]any{
					"error": "payment authorization failed", "dependency": "payments",
					"order_id": request.OrderID, "payments_status": response.StatusCode,
				})
			})
			return
		}

		ordersConfirmed.Add(ctx, 1)
		httpx.JSON(w, http.StatusOK, func() ([]byte, error) {
			return json.Marshal(map[string]any{
				"order_id": request.OrderID, "status": "confirmed", "payments_response": json.RawMessage(body),
			})
		})
	}
}

// faultHandler applies a fault locally and forwards it to payments, so the UI
// has one control surface. Payments owns the modes that break payments; the api
// applies the telemetry-quality mode to its own logs too.
func faultHandler(controller *faults.Controller, client *http.Client, paymentsURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<16))
		if err != nil {
			httpx.JSON(w, http.StatusBadRequest, func() ([]byte, error) {
				return json.Marshal(map[string]string{"error": "read request body"})
			})
			return
		}
		var request struct {
			Mode  string   `json:"mode"`
			Rate  *float64 `json:"rate"`
			Clear bool     `json:"clear"`
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			httpx.JSON(w, http.StatusBadRequest, func() ([]byte, error) {
				return json.Marshal(map[string]string{"error": "invalid request body"})
			})
			return
		}

		switch {
		case request.Clear:
			controller.Clear()
		case faults.Valid(request.Mode):
			rate := faults.DefaultRate
			if request.Rate != nil {
				rate = *request.Rate
			}
			controller.Set(faults.Mode(request.Mode), rate)
		default:
			httpx.JSON(w, http.StatusBadRequest, func() ([]byte, error) {
				return json.Marshal(map[string]any{"error": "unknown fault mode", "known": faults.AllModes()})
			})
			return
		}

		// Forward to payments. A failure here must be visible: silently
		// diverging fault state between the two services would be baffling to
		// debug mid-demo.
		forwardErr := forwardFault(r.Context(), client, paymentsURL, raw)
		if forwardErr != nil {
			slog.Warn("forward fault to payments", "error", forwardErr)
		}
		slog.Info("fault state", "enabled", controller.Enabled())

		httpx.JSON(w, http.StatusOK, func() ([]byte, error) {
			out := map[string]any{"state": controller.Snapshot()}
			if forwardErr != nil {
				out["payments_error"] = forwardErr.Error()
			}
			return json.Marshal(out)
		})
	}
}

func forwardFault(ctx context.Context, client *http.Client, paymentsURL string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, paymentsURL+"/internal/fault", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return errors.New("payments returned " + response.Status)
	}
	return nil
}

// relayTraces forwards a browser OTLP/JSON payload to the collector unchanged.
// It is deliberately dumb: no parsing, no enrichment. The browser produces
// valid OTLP or the collector rejects it, and the error surfaces to the caller
// rather than being swallowed into a fake success.
func relayTraces(client *http.Client, endpoint string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			http.Error(w, "build relay request", http.StatusInternalServerError)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			slog.Warn("relay browser spans", "error", err)
			http.Error(w, "relay failed", http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		relayed, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(relayed)
	}
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
