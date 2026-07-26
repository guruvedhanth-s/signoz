package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/mcp"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/profile"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/source"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/source/signoz"
)

// preflightCheck is one named dependency check, and why it failed if it
// did. PRD section 19: on startup the sidekick checks SigNoz reachability,
// the MCP server, the OpenRouter key, the Slack token, and that the
// demo-agent is emitting telemetry, printing a clear warning for anything
// missing. There is no fallback path by design - a missing dependency is
// surfaced loudly rather than silently degraded.
type preflightCheck struct {
	Name string
	Err  error
}

func runPreflight(args []string) error {
	fs := flag.NewFlagSet("preflight", flag.ContinueOnError)
	signozURL := fs.String("signoz-url", envOr("SIGNOZ_URL", "http://localhost:8080"), "SigNoz base URL to check reachability against")
	apiKey := fs.String("api-key", os.Getenv("SIGNOZ_API_KEY"), "SigNoz service-account API key")
	mcpURL := fs.String("mcp-url", os.Getenv("SIGNOZ_MCP_URL"), "SigNoz MCP server URL")
	signozInternalURL := fs.String("signoz-internal-url", os.Getenv("SIGNOZ_INTERNAL_URL"), "SigNoz URL the MCP server uses to reach SigNoz")
	profilePath := fs.String("profile", "examples/demo-agent.yaml", "telemetry profile used to check the demo-agent is emitting telemetry")
	lookback := fs.Duration("lookback", 5*time.Minute, "how far back to look for demo-agent telemetry")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	checks := []preflightCheck{
		{"SigNoz", checkSignozReachable(ctx, *signozURL)},
		{"SigNoz MCP server", checkMCP(ctx, *mcpURL, *apiKey, *signozInternalURL)},
		// Presence only: this environment cannot reach openrouter.ai or
		// slack.com to validate the key/tokens themselves, and burning a
		// real OpenRouter or Slack API call on every startup would be a
		// cost/rate-limit surprise nobody asked for. This matches "is it
		// configured", which is what a missing-dependency preflight is
		// actually guarding against (PRD section 19).
		{"OpenRouter API key", checkEnvSet("OPENROUTER_API_KEY")},
		{"Slack bot token", checkEnvPrefixed("SLACK_BOT_TOKEN", "xoxb-")},
		{"Slack app token", checkEnvPrefixed("SLACK_APP_TOKEN", "xapp-")},
		{"demo-agent telemetry", checkDemoAgentTelemetry(ctx, *profilePath, *signozURL, *apiKey, *lookback)},
	}

	failed := 0
	for _, c := range checks {
		if c.Err != nil {
			failed++
			fmt.Printf("[FAIL] %-20s %v\n", c.Name, c.Err)
		} else {
			fmt.Printf("[ OK ] %-20s\n", c.Name)
		}
	}

	if failed > 0 {
		return fmt.Errorf("preflight: %d of %d checks failed; fix these before running watch or diagnose - there is no fallback path by design", failed, len(checks))
	}
	fmt.Println("preflight: all checks passed")
	return nil
}

func checkSignozReachable(ctx context.Context, signozURL string) error {
	url := strings.TrimRight(signozURL, "/") + "/api/v1/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("unreachable at %s: %w", signozURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unhealthy at %s: HTTP %d", signozURL, resp.StatusCode)
	}
	return nil
}

func checkMCP(ctx context.Context, mcpURL, apiKey, signozInternalURL string) error {
	if mcpURL == "" {
		return fmt.Errorf("SIGNOZ_MCP_URL (or --mcp-url) is not set")
	}
	client := mcp.NewClient(mcpURL, apiKey, signozInternalURL, nil)
	if err := client.Initialize(ctx); err != nil {
		return fmt.Errorf("initialize handshake failed against %s: %w", mcpURL, err)
	}
	return nil
}

func checkEnvSet(name string) error {
	if strings.TrimSpace(os.Getenv(name)) == "" {
		return fmt.Errorf("%s is not set", name)
	}
	return nil
}

func checkEnvPrefixed(name, prefix string) error {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fmt.Errorf("%s is not set", name)
	}
	if !strings.HasPrefix(value, prefix) {
		return fmt.Errorf("%s does not look like a %q token", name, prefix)
	}
	return nil
}

// checkDemoAgentTelemetry loads the demo-agent's own telemetry profile and
// takes a live snapshot scoped to its service/environment: if nothing in
// the profile's declared signals has been seen within lookback, the demo
// has nothing to diagnose and the preflight says so explicitly rather than
// letting the demo silently fail on an empty incident later.
func checkDemoAgentTelemetry(ctx context.Context, profilePath, signozURL, apiKey string, lookback time.Duration) error {
	p, err := profile.LoadFile(profilePath)
	if err != nil {
		return fmt.Errorf("load profile %q: %w", profilePath, err)
	}

	client := signoz.NewClient(signozURL, apiKey)
	telemetrySource := signoz.NewTelemetrySource(client, 50)

	now := time.Now()
	target := source.Target{
		Service:     p.Metadata.Service,
		Environment: p.Metadata.Environment,
		Start:       now.Add(-lookback),
		End:         now,
	}

	snapshot, err := telemetrySource.Snapshot(ctx, p, target)
	if err != nil {
		return fmt.Errorf("query SigNoz for %s/%s: %w", p.Metadata.Service, p.Metadata.Environment, err)
	}
	if len(snapshot.LastSeen) == 0 {
		return fmt.Errorf("no telemetry seen for %s/%s in the last %s - is demo-agent running?", p.Metadata.Service, p.Metadata.Environment, lookback)
	}
	return nil
}
