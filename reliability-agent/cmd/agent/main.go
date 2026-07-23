// Command agent is the SigNoz reliability agent: a third-layer service that runs
// beside a stock SigNoz and evaluates SLOs (and, later, audits telemetry).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/guruvedhanth-s/signoz/reliability-agent/internal/otlp"
	"github.com/guruvedhanth-s/signoz/reliability-agent/internal/signoz"
	"github.com/guruvedhanth-s/signoz/reliability-agent/internal/slo"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "slo":
		if err := runSLO(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "generate":
		if err := runGenerate(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `reliability-agent - SLO and telemetry reliability for stock SigNoz

Usage:
  agent slo        --config slo.yaml --signoz-url URL --api-key KEY [--emit]
  agent generate   --signoz-url URL --api-key KEY

'generate' creates (or idempotently updates) the SLO dashboard in SigNoz.

Flags for 'slo':
  --config         path to the SLO-as-code YAML (default: slo.yaml)
  --signoz-url     SigNoz base URL (default: env SIGNOZ_URL or http://localhost:8080)
  --api-key        service-account API key (default: env SIGNOZ_API_KEY)
  --emit           emit slo.* metrics back into SigNoz over OTLP
  --otlp-endpoint  collector OTLP HTTP endpoint (default: env OTLP_ENDPOINT or localhost:4318)`)
}

func runSLO(args []string) error {
	fs := flag.NewFlagSet("slo", flag.ExitOnError)
	configPath := fs.String("config", "slo.yaml", "path to the SLO-as-code YAML")
	url := fs.String("signoz-url", envOr("SIGNOZ_URL", "http://localhost:8080"), "SigNoz base URL")
	apiKey := fs.String("api-key", os.Getenv("SIGNOZ_API_KEY"), "service-account API key")
	emit := fs.Bool("emit", false, "emit slo.* metrics back into SigNoz over OTLP")
	otlpEndpoint := fs.String("otlp-endpoint", envOr("OTLP_ENDPOINT", "localhost:4318"), "collector OTLP HTTP endpoint")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := slo.LoadConfig(*configPath)
	if err != nil {
		return err
	}

	ctx := context.Background()
	client := signoz.NewClient(*url, *apiKey)
	engine := slo.NewEngine(client, buildGate(client, cfg))
	reports := engine.Evaluate(ctx, cfg)

	printReports(cfg.Service, reports)

	if *emit {
		if err := emitReports(ctx, *otlpEndpoint, reports); err != nil {
			return err
		}
		fmt.Printf("\nemitted slo.* metrics to %s\n", *otlpEndpoint)
	}
	return nil
}

// buildGate uses a real metric-presence gate when the config lists expected
// metrics, otherwise trusts telemetry (NoopGate).
func buildGate(client *signoz.Client, cfg *slo.Config) slo.CompletenessGate {
	if cfg.Completeness != nil && len(cfg.Completeness.ExpectedMetrics) > 0 {
		return slo.NewMetricPresenceGate(client, cfg.Completeness.ExpectedMetrics)
	}
	return slo.NoopGate{}
}

func emitReports(ctx context.Context, endpoint string, reports []*slo.Report) error {
	emitter, err := otlp.NewEmitter(ctx, endpoint)
	if err != nil {
		return err
	}
	for _, r := range reports {
		emitter.Emit(ctx, r)
	}
	return emitter.Shutdown(ctx)
}

func printReports(service string, reports []*slo.Report) {
	fmt.Printf("SLO report for service %q\n\n", service)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SLO\tSTATE\tSLI\tTARGET\tBUDGET LEFT\tBURN")
	for _, r := range reports {
		sli, budget, burn := "-", "-", "-"
		if r.State != slo.StateIndeterminate {
			sli = fmt.Sprintf("%.2f%%", r.SLI*100)
			budget = fmt.Sprintf("%.2f%%", r.ErrorBudgetRemaining*100)
			burn = fmt.Sprintf("%.2fx", r.BurnRate)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%.2f%%\t%s\t%s\n",
			r.Name, r.State, sli, r.Target*100, budget, burn)
	}
	w.Flush()
}

func runGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ExitOnError)
	configPath := fs.String("config", "slo.yaml", "path to the SLO-as-code YAML")
	url := fs.String("signoz-url", envOr("SIGNOZ_URL", "http://localhost:8080"), "SigNoz base URL")
	apiKey := fs.String("api-key", os.Getenv("SIGNOZ_API_KEY"), "service-account API key")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := slo.LoadConfig(*configPath)
	if err != nil {
		return err
	}

	ctx := context.Background()
	client := signoz.NewClient(*url, *apiKey)

	// 1. Dashboard.
	id, created, err := client.GenerateDashboard(ctx, slo.BuildDashboard())
	if err != nil {
		return err
	}
	verb := "updated"
	if created {
		verb = "created"
	}
	fmt.Printf("%s SLO dashboard %q\n  open: %s/dashboard/%s\n", verb, slo.DashboardTitle, *url, id)

	// 2. Notification channel (burn-rate alerts require one).
	if err := client.EnsureChannel(ctx, slo.DefaultChannelName); err != nil {
		return err
	}

	// 3. A fast-burn alert per SLO (fires when slo_burn_rate exceeds the fast tier).
	fastThreshold := slo.DefaultBurnRateTiers[0].Threshold
	for _, def := range cfg.SLOs {
		rule := slo.BuildBurnRateRule(def.Name, fastThreshold, slo.DefaultChannelName)
		made, err := client.GenerateBurnRateAlert(ctx, slo.BurnRuleName(def.Name), rule)
		if err != nil {
			return err
		}
		status := "exists"
		if made {
			status = "created"
		}
		fmt.Printf("burn-rate alert %q: %s\n", slo.BurnRuleName(def.Name), status)
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
