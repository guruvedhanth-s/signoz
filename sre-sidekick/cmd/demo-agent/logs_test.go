package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOTLPLogEmitterCarriesTraceCorrelationAndSeverity(t *testing.T) {
	var record map[string]any
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			ResourceLogs []struct {
				ScopeLogs []struct {
					LogRecords []map[string]any `json:"logRecords"`
				} `json:"scopeLogs"`
			} `json:"resourceLogs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		record = payload.ResourceLogs[0].ScopeLogs[0].LogRecords[0]
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	emitter := &otlpLogEmitter{
		endpoint:    collector.URL,
		service:     "support-agent",
		environment: "local",
		client:      collector.Client(),
	}

	err := emitter.Emit(context.Background(), logEntry{
		TraceID:  "0123456789abcdef0123456789abcdef",
		SpanID:   "0123456789abcdef",
		Severity: logSeverityError,
		Body:     "agent run failed: tool.search_kb timed out",
		Failing:  true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if record["traceId"] != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("expected the log to carry the trace ID, got %v", record["traceId"])
	}
	if record["spanId"] != "0123456789abcdef" {
		t.Fatalf("expected the log to carry the span ID, got %v", record["spanId"])
	}
	if record["severityText"] != "ERROR" {
		t.Fatalf("expected ERROR severity, got %v", record["severityText"])
	}
	if record["severityNumber"].(float64) != 17 {
		t.Fatalf("expected severity number 17, got %v", record["severityNumber"])
	}
	body, _ := record["body"].(map[string]any)
	if !strings.Contains(body["stringValue"].(string), "timed out") {
		t.Fatalf("expected the body to describe the timeout, got %v", body)
	}
}

func TestOTLPLogEmitterReportsCollectorErrors(t *testing.T) {
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer collector.Close()

	emitter := &otlpLogEmitter{
		endpoint:    collector.URL,
		service:     "support-agent",
		environment: "local",
		client:      collector.Client(),
	}
	err := emitter.Emit(context.Background(), logEntry{Severity: logSeverityInfo, Body: "ok"})
	if err == nil {
		t.Fatal("expected an error when the collector rejects the request")
	}
}

func TestModeName(t *testing.T) {
	if modeName(true) != "buggy" {
		t.Fatalf("expected buggy, got %q", modeName(true))
	}
	if modeName(false) != "healthy" {
		t.Fatalf("expected healthy, got %q", modeName(false))
	}
}

func TestSeverityNumber(t *testing.T) {
	if severityNumber(logSeverityError) != 17 {
		t.Fatalf("expected 17 for ERROR")
	}
	if severityNumber(logSeverityInfo) != 9 {
		t.Fatalf("expected 9 for INFO")
	}
}
