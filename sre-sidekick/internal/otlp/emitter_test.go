package otlp

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/slo"
)

func TestNormalizeEndpoint(t *testing.T) {
	host, insecure, path, err := normalizeEndpoint("http://localhost:4318")
	if err != nil || host != "localhost:4318" || !insecure || path != "" {
		t.Fatalf("unexpected HTTP endpoint: %q %v %q %v", host, insecure, path, err)
	}
	host, insecure, path, err = normalizeEndpoint("localhost:4318")
	if err != nil || host != "localhost:4318" || !insecure || path != "/v1/metrics" {
		t.Fatalf("unexpected host endpoint: %q %v %q %v", host, insecure, path, err)
	}
}

// Metrics arriving in SigNoz under service.name="unknown_service:<binary>" is
// what happens with no explicit resource: the per-point attributes still carry
// the audited service, so queries work, but the emitting service shows as
// unknown in every service-scoped view. Confirmed live, then fixed.
//
// The OTLP/HTTP body is protobuf, and decoding it here would mean depending on
// the collector proto packages for one assertion. Protobuf encodes string
// values literally, so a substring check over the raw body is enough to catch
// a dropped resource - which is the only thing this guards.
func TestEmitterIdentifiesItselfAsTheEmittingService(t *testing.T) {
	bodies := make(chan []byte, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read export body: %v", err)
		}
		select {
		case bodies <- body:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	emitter, err := NewEmitter(context.Background(), server.URL+"/v1/metrics")
	if err != nil {
		t.Fatal(err)
	}
	emitter.EmitSLO(context.Background(), slo.Report{
		Name: "grounded-answers", Service: "support-agent", Environment: "local",
		Window: "5m", State: slo.StateHealthy, SLI: 1,
	})
	// Shutdown flushes the periodic reader, so the export is complete here.
	if err := emitter.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	select {
	case body := <-bodies:
		if !bytes.Contains(body, []byte("service.name")) {
			t.Fatal("export carries no service.name resource attribute")
		}
		if !bytes.Contains(body, []byte("sre-sidekick")) {
			t.Error("emitting service must identify as sre-sidekick, not unknown_service")
		}
		if bytes.Contains(body, []byte("unknown_service")) {
			t.Error("export still falls back to the SDK's unknown_service default")
		}
		// The audited service stays a per-point attribute, not the resource.
		if !bytes.Contains(body, []byte("support-agent")) {
			t.Error("the audited service must still be attached to the data point")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no OTLP export arrived")
	}
}
