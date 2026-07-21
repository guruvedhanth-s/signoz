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
  agent slo   --config slo.yaml --signoz-url URL --api-key KEY

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
	engine := slo.NewEngine(client, slo.NoopGate{})
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
