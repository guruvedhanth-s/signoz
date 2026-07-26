// payments is the demo's downstream dependency: the service that actually
// breaks.
//
// It exists so RCA has an interesting question to answer. With a single
// service, a diagnosis can only say "the api is failing". With a dependency,
// the useful diagnosis is "the api is failing *because* payments is", which is
// what an on-call engineer actually needs and what distinguishes a grounded
// agent from a log summariser.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/guruvedhanth-s/signoz/demo-app/internal/faults"
	"github.com/guruvedhanth-s/signoz/demo-app/internal/httpx"
	"github.com/guruvedhanth-s/signoz/demo-app/internal/telemetry"
)

const (
	// normalLatency is what a healthy charge takes.
	normalLatency = 40 * time.Millisecond
	// slowLatency is what payments_latency injects: well past the api's 1s
	// latency SLO threshold, without erroring, so the latency SLO breaks while
	// the error-rate SLO stays healthy. That separation matters - it is how the
	// demo shows the two SLOs measuring different things.
	slowLatency = 1400 * time.Millisecond
	// hangLatency exceeds the api's client timeout, so the caller sees a
	// failure while this service records a long span. That asymmetry is
	// realistic and is what the trace tree makes obvious.
	hangLatency = 6 * time.Second
)

func main() {
	listen := flag.String("listen", envOr("PAYMENTS_LISTEN", "127.0.0.1:8091"), "HTTP listen address")
	otlpEndpoint := flag.String("otlp-endpoint", envOr("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4318"), "OTLP/HTTP endpoint")
	environment := flag.String("environment", envOr("DEMO_ENVIRONMENT", "local"), "deployment environment")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	t, err := telemetry.Setup(ctx, telemetry.Config{
		Service:     "payments",
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

	controller := faults.New()
	// The emitter asks the controller per record rather than being told when the
	// fault changes. That keeps one owner for the state - no race between the
	// handler writing and requests reading, and the mode's rate is sampled per
	// record like every other mode instead of collapsing to all-or-nothing.
	t.Logs.OmitTraceID = func() bool { return controller.Active(faults.LogsMissingTraceID) }

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, http.StatusOK, func() ([]byte, error) { return json.Marshal(map[string]string{"status": "ok"}) })
	})
	mux.HandleFunc("POST /charge", httpx.Instrument(t, "/charge", charge(t, controller)))
	// The api proxies fault changes here so one UI control governs both
	// services; payments owns the modes that actually break payments.
	mux.HandleFunc("POST /internal/fault", faultHandler(controller))
	mux.HandleFunc("GET /internal/fault", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, http.StatusOK, func() ([]byte, error) { return json.Marshal(controller.Snapshot()) })
	})

	server := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// Longer than hangLatency so the injected hang is the client's timeout
		// rather than the server cutting its own response short.
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	slog.Info("payments listening", "address", *listen, "environment", *environment, "version", telemetry.Version)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("payments server", "error", err)
		os.Exit(1)
	}
	slog.Info("payments stopped")
}

type chargeRequest struct {
	OrderID string  `json:"order_id"`
	Amount  float64 `json:"amount"`
}

// charge is the business operation. The injected faults live inside a child
// span so the trace tree shows *where* the time went or the error came from,
// rather than only that the request failed.
func charge(t *telemetry.Telemetry, controller *faults.Controller) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		var request chargeRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
			httpx.JSON(w, http.StatusBadRequest, func() ([]byte, error) {
				return json.Marshal(map[string]string{"error": "invalid request body"})
			})
			return
		}

		ctx, span := t.Tracer.Start(ctx, "payments.authorize",
			trace.WithAttributes(
				attribute.String("payment.order_id", request.OrderID),
				attribute.Float64("payment.amount", request.Amount),
			),
		)
		defer span.End()

		switch {
		case controller.Active(faults.PaymentsTimeout):
			span.SetAttributes(attribute.String("fault.injected", string(faults.PaymentsTimeout)))
			select {
			case <-time.After(hangLatency):
			case <-ctx.Done():
				// The caller gave up first. Record it as the error it is.
				span.SetStatus(codes.Error, "caller cancelled while payment authorization hung")
				span.RecordError(ctx.Err())
				return
			}
			span.SetStatus(codes.Error, "payment authorization timed out")
			httpx.JSON(w, http.StatusGatewayTimeout, func() ([]byte, error) {
				return json.Marshal(map[string]string{"error": "authorization timed out"})
			})

		case controller.Active(faults.PaymentsErrors):
			span.SetAttributes(attribute.String("fault.injected", string(faults.PaymentsErrors)))
			time.Sleep(normalLatency)
			err := fmt.Errorf("payment processor rejected the charge")
			span.SetStatus(codes.Error, err.Error())
			span.RecordError(err)
			httpx.JSON(w, http.StatusInternalServerError, func() ([]byte, error) {
				return json.Marshal(map[string]string{"error": err.Error()})
			})

		case controller.Active(faults.PaymentsLatency):
			span.SetAttributes(attribute.String("fault.injected", string(faults.PaymentsLatency)))
			time.Sleep(slowLatency)
			httpx.JSON(w, http.StatusOK, func() ([]byte, error) {
				return json.Marshal(map[string]any{"order_id": request.OrderID, "status": "authorized", "slow": true})
			})

		default:
			time.Sleep(normalLatency)
			httpx.JSON(w, http.StatusOK, func() ([]byte, error) {
				return json.Marshal(map[string]any{"order_id": request.OrderID, "status": "authorized"})
			})
		}
	}
}

func faultHandler(controller *faults.Controller) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Mode  string   `json:"mode"`
			Rate  *float64 `json:"rate"`
			Clear bool     `json:"clear"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&request); err != nil {
			httpx.JSON(w, http.StatusBadRequest, func() ([]byte, error) {
				return json.Marshal(map[string]string{"error": "invalid request body"})
			})
			return
		}
		if request.Clear {
			controller.Clear()
			httpx.JSON(w, http.StatusOK, func() ([]byte, error) { return json.Marshal(controller.Snapshot()) })
			return
		}
		if !faults.Valid(request.Mode) {
			httpx.JSON(w, http.StatusBadRequest, func() ([]byte, error) {
				return json.Marshal(map[string]any{"error": "unknown fault mode", "known": faults.AllModes()})
			})
			return
		}
		rate := faults.DefaultRate
		if request.Rate != nil {
			rate = *request.Rate
		}
		effective := controller.Set(faults.Mode(request.Mode), rate)
		slog.Info("fault mode changed", "mode", request.Mode, "rate", effective, "enabled", controller.Enabled())
		httpx.JSON(w, http.StatusOK, func() ([]byte, error) { return json.Marshal(controller.Snapshot()) })
	}
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
