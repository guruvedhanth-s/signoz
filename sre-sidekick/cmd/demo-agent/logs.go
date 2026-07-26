package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// severityNumber maps an OTLP severity text to its numeric equivalent.
// See https://opentelemetry.io/docs/specs/otel/logs/data-model/#field-severitynumber
func severityNumber(severity string) int {
	if severity == logSeverityError {
		return 17
	}
	return 9
}

func modeName(failing bool) string {
	if failing {
		return "buggy"
	}
	return "healthy"
}

// otlpLogEmitter posts one OTLP/HTTP JSON log record per run, carrying the
// trace and span IDs of that run's agent.run span so the log correlates
// with the trace (and, on failure, describes the tool.search_kb timeout).
type otlpLogEmitter struct {
	endpoint    string
	service     string
	environment string
	client      *http.Client
}

func (e *otlpLogEmitter) Emit(ctx context.Context, entry logEntry) error {
	now := time.Now()
	logRecord := map[string]any{
		"timeUnixNano":         fmt.Sprintf("%d", now.UnixNano()),
		"observedTimeUnixNano": fmt.Sprintf("%d", now.UnixNano()),
		"severityNumber":       severityNumber(entry.Severity),
		"severityText":         entry.Severity,
		"body":                 stringValue(entry.Body),
		"attributes": []map[string]any{
			otlpAttribute("telemetry.demo.mode", stringValue(modeName(entry.Failing))),
		},
		"traceId": entry.TraceID,
		"spanId":  entry.SpanID,
	}

	payload := map[string]any{
		"resourceLogs": []any{map[string]any{
			"resource": map[string]any{
				"attributes": []any{
					otlpAttribute("service.name", stringValue(e.service)),
					otlpAttribute("deployment.environment", stringValue(e.environment)),
					otlpAttribute("service.version", stringValue("demo-1.0.0")),
				},
			},
			"scopeLogs": []any{map[string]any{
				"scope":      map[string]any{"name": "demo-agent"},
				"logRecords": []any{logRecord},
			}},
		}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode OTLP log: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create OTLP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("send OTLP log: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("OTLP collector returned %s: %s", resp.Status, string(message))
	}
	return nil
}

func otlpAttribute(key string, value map[string]any) map[string]any {
	return map[string]any{"key": key, "value": value}
}

func stringValue(value string) map[string]any {
	return map[string]any{"stringValue": value}
}
