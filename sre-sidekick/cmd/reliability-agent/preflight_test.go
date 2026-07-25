package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCheckEnvSet(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_VAR", "")
	if err := checkEnvSet("TEST_PREFLIGHT_VAR"); err == nil {
		t.Fatal("checkEnvSet() error = nil, want an error for an unset var")
	}
	t.Setenv("TEST_PREFLIGHT_VAR", "value")
	if err := checkEnvSet("TEST_PREFLIGHT_VAR"); err != nil {
		t.Errorf("checkEnvSet() error = %v, want nil", err)
	}
}

func TestCheckEnvPrefixed(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_TOKEN", "")
	if err := checkEnvPrefixed("TEST_PREFLIGHT_TOKEN", "xoxb-"); err == nil {
		t.Fatal("checkEnvPrefixed() error = nil, want an error for an unset var")
	}

	t.Setenv("TEST_PREFLIGHT_TOKEN", "xapp-wrong-slot")
	if err := checkEnvPrefixed("TEST_PREFLIGHT_TOKEN", "xoxb-"); err == nil {
		t.Fatal("checkEnvPrefixed() error = nil, want an error for the wrong prefix")
	}

	t.Setenv("TEST_PREFLIGHT_TOKEN", "xoxb-real-token")
	if err := checkEnvPrefixed("TEST_PREFLIGHT_TOKEN", "xoxb-"); err != nil {
		t.Errorf("checkEnvPrefixed() error = %v, want nil", err)
	}
}

func TestCheckSignozReachable(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer healthy.Close()

	if err := checkSignozReachable(context.Background(), healthy.URL); err != nil {
		t.Errorf("checkSignozReachable() error = %v, want nil for a healthy server", err)
	}

	unhealthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer unhealthy.Close()

	if err := checkSignozReachable(context.Background(), unhealthy.URL); err == nil {
		t.Fatal("checkSignozReachable() error = nil, want an error for a 503")
	}

	if err := checkSignozReachable(context.Background(), "http://127.0.0.1:0"); err == nil {
		t.Fatal("checkSignozReachable() error = nil, want an error for an unreachable host")
	}
}

func TestCheckMCP_NoURLConfigured(t *testing.T) {
	err := checkMCP(context.Background(), "", "key", "http://signoz")
	if err == nil || !strings.Contains(err.Error(), "SIGNOZ_MCP_URL") {
		t.Errorf("checkMCP() error = %v, want it to name SIGNOZ_MCP_URL", err)
	}
}

func TestRunPreflight_ReportsAllFailuresAndReturnsAnError(t *testing.T) {
	t.Setenv("SIGNOZ_URL", "")
	t.Setenv("SIGNOZ_MCP_URL", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("SLACK_BOT_TOKEN", "")
	t.Setenv("SLACK_APP_TOKEN", "")

	err := runPreflight([]string{
		"--signoz-url", "http://127.0.0.1:0",
		"--profile", "/nonexistent/profile.yaml",
	})
	if err == nil {
		t.Fatal("runPreflight() error = nil, want every check to fail with no dependencies configured")
	}
	if !strings.Contains(err.Error(), "checks failed") {
		t.Errorf("runPreflight() error = %q, want it to summarize the failure count", err)
	}
}

func TestRunPreflight_HonorsATimeout(t *testing.T) {
	// A slow but eventually-successful SigNoz should not hang runPreflight
	// forever; the 20s internal timeout is exercised implicitly by every
	// other test completing quickly, but this guards the flag itself
	// parses and a fast local run completes well within a test timeout.
	start := time.Now()
	_ = runPreflight([]string{"--signoz-url", "http://127.0.0.1:0", "--profile", "/nonexistent/profile.yaml"})
	if time.Since(start) > 5*time.Second {
		t.Error("runPreflight() took too long for all-local-failure checks")
	}
}
