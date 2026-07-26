package main

import "testing"

func TestParseFlagsDefaults(t *testing.T) {
	cfg, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.service != "support-agent" {
		t.Fatalf("expected default service support-agent, got %q", cfg.service)
	}
	if cfg.environment != "local" {
		t.Fatalf("expected default environment local, got %q", cfg.environment)
	}
	if cfg.buggy {
		t.Fatalf("expected buggy to default to false")
	}
	if cfg.errorRate != 0 {
		t.Fatalf("expected error rate to default to 0 in healthy mode, got %v", cfg.errorRate)
	}
	if cfg.runs != 0 {
		t.Fatalf("expected runs to default to 0 (unbounded), got %d", cfg.runs)
	}
}

func TestParseFlagsBuggyDefaultsErrorRateToOne(t *testing.T) {
	cfg, err := parseFlags([]string{"--buggy"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.buggy {
		t.Fatalf("expected buggy to be true")
	}
	if cfg.errorRate != 1.0 {
		t.Fatalf("expected error rate to default to 1.0 in buggy mode, got %v", cfg.errorRate)
	}
}

func TestParseFlagsExplicitErrorRateOverridesDefault(t *testing.T) {
	cfg, err := parseFlags([]string{"--buggy", "--error-rate", "0.3"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.errorRate != 0.3 {
		t.Fatalf("expected error rate 0.3, got %v", cfg.errorRate)
	}
}

func TestParseFlagsRejectsInvalidErrorRate(t *testing.T) {
	if _, err := parseFlags([]string{"--error-rate", "1.5"}); err == nil {
		t.Fatal("expected an error for an out-of-range error rate")
	}
}

func TestParseFlagsRejectsNonPositiveInterval(t *testing.T) {
	if _, err := parseFlags([]string{"--interval", "0s"}); err == nil {
		t.Fatal("expected an error for a non-positive interval")
	}
}

func TestParseFlagsRejectsNegativeRuns(t *testing.T) {
	if _, err := parseFlags([]string{"--runs", "-1"}); err == nil {
		t.Fatal("expected an error for negative runs")
	}
}

func TestNormalizeOTLPEndpoint(t *testing.T) {
	cases := []struct {
		name         string
		endpoint     string
		wantHost     string
		wantInsecure bool
		wantErr      bool
	}{
		{name: "bare host:port", endpoint: "localhost:4318", wantHost: "localhost:4318", wantInsecure: true},
		{name: "http URL", endpoint: "http://localhost:4318", wantHost: "localhost:4318", wantInsecure: true},
		{name: "https URL", endpoint: "https://otel.example.com", wantHost: "otel.example.com", wantInsecure: false},
		{name: "empty", endpoint: "", wantErr: true},
		{name: "invalid URL", endpoint: "http://", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, insecure, err := normalizeOTLPEndpoint(tc.endpoint)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %q", tc.endpoint)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if host != tc.wantHost || insecure != tc.wantInsecure {
				t.Fatalf("got host=%q insecure=%v, want host=%q insecure=%v", host, insecure, tc.wantHost, tc.wantInsecure)
			}
		})
	}
}

func TestLogsURL(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		want     string
		wantErr  bool
	}{
		{name: "bare host:port", endpoint: "localhost:4318", want: "http://localhost:4318/v1/logs"},
		{name: "http URL", endpoint: "http://localhost:4318", want: "http://localhost:4318/v1/logs"},
		{name: "trailing slash", endpoint: "http://localhost:4318/", want: "http://localhost:4318/v1/logs"},
		{name: "empty", endpoint: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := logsURL(tc.endpoint)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %q", tc.endpoint)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
