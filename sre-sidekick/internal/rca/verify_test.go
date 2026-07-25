package rca

import (
	"context"
	"testing"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify/slack"
)

func TestSLOVerifier_Verify_ReportsHealthyRecovery(t *testing.T) {
	sloPath := writeSLOConfig(t, slackAdapterSLOConfig)
	metrics := fakeMetrics{Values: map[string]float64{"agent_success_total": 99, "agent_requests_total": 100}}

	verifier := &SLOVerifier{
		Metrics:       metrics,
		SLOConfigPath: sloPath,
		Now:           func() time.Time { return time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC) },
	}

	result, err := verifier.Verify(context.Background(), slack.VerifyRequest{
		Service: "support-agent", Environment: "local", SLO: "grounded-answers",
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if result.SLOState != "healthy" || !result.Recovered {
		t.Errorf("VerifyResult = %+v, want a healthy, recovered result", result)
	}
	if !result.TelemetryTrusted {
		t.Error("TelemetryTrusted = false, want true for a complete window")
	}
}

func TestSLOVerifier_Verify_ReportsUnhealthyAsNotRecovered(t *testing.T) {
	sloPath := writeSLOConfig(t, slackAdapterSLOConfig)
	// Below the 0.9 target in slackAdapterSLOConfig.
	metrics := fakeMetrics{Values: map[string]float64{"agent_success_total": 50, "agent_requests_total": 100}}

	verifier := &SLOVerifier{Metrics: metrics, SLOConfigPath: sloPath}

	result, err := verifier.Verify(context.Background(), slack.VerifyRequest{
		Service: "support-agent", Environment: "local", SLO: "grounded-answers",
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if result.SLOState != "unhealthy" || result.Recovered {
		t.Errorf("VerifyResult = %+v, want unhealthy and not recovered", result)
	}
}

func TestSLOVerifier_Verify_MismatchedServiceIsAnError(t *testing.T) {
	sloPath := writeSLOConfig(t, slackAdapterSLOConfig)
	metrics := fakeMetrics{Values: map[string]float64{"agent_success_total": 99, "agent_requests_total": 100}}

	verifier := &SLOVerifier{Metrics: metrics, SLOConfigPath: sloPath}

	if _, err := verifier.Verify(context.Background(), slack.VerifyRequest{
		Service: "checkout-api", Environment: "local", SLO: "grounded-answers",
	}); err == nil {
		t.Fatal("Verify() error = nil, want an error for a service the SLO config does not cover")
	}
}
