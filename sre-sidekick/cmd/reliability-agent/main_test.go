package main

import (
	"strings"
	"testing"
)

// runAudit is the one-shot Track A command; like diagnose/watch, it must
// fail fast on a missing credential rather than send an unauthenticated
// query and misreport the result as "no telemetry".
func TestRunAuditRequiresAnAPIKey(t *testing.T) {
	t.Setenv("SIGNOZ_API_KEY", "")
	err := runAudit([]string{"--profile", "../../examples/support-agent.yaml", "--signoz-url", "http://localhost:8080"})
	if err == nil || !strings.Contains(err.Error(), "SIGNOZ_API_KEY") {
		t.Fatalf("error = %v, want the missing API key named", err)
	}
}

func TestIsLoopbackListen(t *testing.T) {
	for _, test := range []struct {
		address string
		want    bool
	}{
		{address: "127.0.0.1:8081", want: true},
		{address: "localhost:8081", want: true},
		{address: "[::1]:8081", want: true},
		{address: ":8081", want: false},
		{address: "0.0.0.0:8081", want: false},
	} {
		if got := isLoopbackListen(test.address); got != test.want {
			t.Fatalf("isLoopbackListen(%q) = %v, want %v", test.address, got, test.want)
		}
	}
}
