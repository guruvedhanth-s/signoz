package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"go.opentelemetry.io/otel"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/act"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/config"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/detect"
	sidekickslack "github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify/slack"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/rca"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/session"
)

// DefaultConfigPath is where the sidekick looks for its configuration when no
// path is given.
const DefaultConfigPath = "configs/sidekick.yaml"

// reapInterval is how often idle sessions are swept. The sweep is a map scan
// over live sessions, so it is cheap enough to run far more often than the
// session TTL; running it frequently only bounds how late an expiry notice
// arrives.
const reapInterval = time.Minute

// webhookShutdownTimeout bounds how long the webhook server is given to
// finish in-flight requests when watch shuts down.
const webhookShutdownTimeout = 5 * time.Second

// runWatch runs the Slack adapter: it dials Slack over Socket Mode, routes
// inbound events into incident sessions, sweeps idle ones, and - the
// alert-driven detect stage (PRD section 12) - serves the SigNoz
// alertmanager webhook that starts an autonomous diagnosis.
func runWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	configPath := fs.String("config", DefaultConfigPath, "path to sidekick.yaml")
	signozURL := fs.String("signoz-url", envOr("SIGNOZ_URL", "http://localhost:8080"), "SigNoz base URL (also used as the public host rewritten into evidence links)")
	apiKey := fs.String("api-key", os.Getenv("SIGNOZ_API_KEY"), "SigNoz service-account API key")
	limit := fs.Int("limit", 200, "maximum records per signal query")
	mcpURL := fs.String("mcp-url", os.Getenv("SIGNOZ_MCP_URL"), "SigNoz MCP server URL; when set, evidence is gathered live via MCP tools instead of the M1 telemetry-source placeholder")
	signozInternalURL := fs.String("signoz-internal-url", os.Getenv("SIGNOZ_INTERNAL_URL"), "SigNoz URL the MCP server should use to reach SigNoz (sent as the X-SigNoz-URL header); required when --mcp-url is set")
	sloConfigPath := fs.String("slo-config", "examples/support-agent-slo.yaml", "SLO YAML path used to ground each diagnosis's grounding facts (SLO state, burn rate, error budget, telemetry trust)")
	window := fs.String("window", "1h", "default lookback window for evidence gathering when a human triggers a diagnosis (e.g. via /diagnose)")
	webhookListen := fs.String("webhook-listen", envOr("SIDEKICK_WEBHOOK_LISTEN", "127.0.0.1:8082"), "listen address for the SigNoz alertmanager webhook (PRD section 12)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	// One sidekick.yaml (PRD section 18) covers Slack, the LLM provider, and
	// the presentation-rule thresholds; watch needs all three.
	cfg, err := config.Load(*configPath)
	if err != nil {
		// Loading is strict about credentials on purpose: a missing or swapped
		// token must fail here, at startup, not during an incident.
		return err
	}

	return watchWithConfig(cfg, rcaConfig{
		SignozURL:         *signozURL,
		APIKey:            *apiKey,
		Limit:             *limit,
		MCPURL:            *mcpURL,
		SignozInternalURL: *signozInternalURL,
		Presentation:      cfg.Presentation,
		LLM:               cfg.LLM,
	}, *sloConfigPath, *window, *webhookListen, os.Getenv("SIDEKICK_WEBHOOK_SECRET"))
}

func watchWithConfig(fullCfg config.Config, rcaCfg rcaConfig, sloConfigPath, window, webhookListen, webhookSecret string) error {
	cfg := fullCfg.Notify.Slack

	// Metrics are optional: when no meter provider is configured the global one
	// is a no-op, so the adapter runs identically without a collector.
	metrics, err := sidekickslack.NewMetrics(otel.Meter("sre-sidekick"))
	if err != nil {
		return err
	}

	botToken, err := cfg.BotToken()
	if err != nil {
		return err
	}

	appToken, err := cfg.AppToken()
	if err != nil {
		return err
	}

	if webhookSecret == "" {
		// Fail here, at startup, exactly like a missing Slack or SigNoz
		// credential - a webhook receiver with no secret would refuse every
		// request anyway (detect.Handler fails closed), so surface the
		// misconfiguration loudly rather than serve a receiver that can never
		// accept anything (PRD section 19).
		return fmt.Errorf("SIDEKICK_WEBHOOK_SECRET is required (the alert-driven webhook receiver refuses every request without it)")
	}

	api := slackapi.New(botToken, slackapi.OptionAppLevelToken(appToken))
	socket := socketmode.New(api)

	slackClient, err := sidekickslack.New(cfg,
		sidekickslack.WithPoster(api),
		sidekickslack.WithMetrics(metrics),
	)
	if err != nil {
		return err
	}

	ttl, err := cfg.SessionTTLDuration()
	if err != nil {
		return err
	}
	managerOptions := []session.ManagerOption{session.WithTTL(ttl)}
	var durableStore session.Store
	if strings.EqualFold(fullCfg.Storage.Driver, "postgres") {
		dsn := os.Getenv(fullCfg.Storage.URLEnv)
		store, storeErr := session.NewPostgresStore(context.Background(), dsn)
		if storeErr != nil {
			return fmt.Errorf("postgres session store: %w", storeErr)
		}
		durableStore = store
		defer store.Close()
	} else if strings.EqualFold(fullCfg.Storage.Driver, "file") {
		path := strings.TrimSpace(cfg.SessionStorePath)
		if path == "" {
			return fmt.Errorf("file session store requires notify.slack.session_store_path")
		}
		store, storeErr := session.NewFileStore(path)
		if storeErr != nil {
			return fmt.Errorf("session store: %w", storeErr)
		}
		durableStore = store
	}
	if durableStore != nil {
		managerOptions = append(managerOptions, session.WithStore(durableStore))
	}
	if durableStore == nil && strings.TrimSpace(cfg.SessionStorePath) != "" {
		store, storeErr := session.NewFileStore(strings.TrimSpace(cfg.SessionStorePath))
		if storeErr != nil {
			return fmt.Errorf("session store: %w", storeErr)
		}
		managerOptions = append(managerOptions, session.WithStore(store))
	}
	sessions := session.NewManager(managerOptions...)
	var auditor session.Auditor
	if path := strings.TrimSpace(cfg.AuditLogPath); path != "" {
		fileAuditor, auditErr := session.NewFileAuditor(path)
		if auditErr != nil {
			return fmt.Errorf("audit log: %w", auditErr)
		}
		auditor = fileAuditor
	}
	if auditor == nil {
		if postgres, ok := durableStore.(*session.PostgresStore); ok {
			auditor = postgres.Auditor()
		}
	}

	// buildRCAAgent preflights the OpenRouter reasoner (and the MCP server,
	// when configured) at startup, same as `diagnose` - a missing dependency
	// must be surfaced loudly here, not as a silent "not connected" reply the
	// first time an alert fires (PRD section 19).
	agent, signozClient, err := buildRCAAgent(context.Background(), rcaCfg)
	if err != nil {
		return fmt.Errorf("build RCA agent: %w", err)
	}
	rcaAdapter := &rca.SlackAdapter{
		Agent:          agent,
		Metrics:        signozClient,
		SLOConfigPath:  sloConfigPath,
		Window:         window,
		DefaultChannel: cfg.DefaultChannel,
	}
	actuator := &act.AdvisoryActuator{}
	authorizer, err := sidekickslack.NewConfiguredAuthorizer(context.Background(), cfg.Authorization, api)
	if err != nil {
		return fmt.Errorf("build Slack authorizer: %w", err)
	}

	coordinator, err := sidekickslack.NewCoordinator(
		slackClient, sessions, rcaAdapter,
		sidekickslack.WithDefaultEnvironment(cfg.DefaultEnvironment),
		sidekickslack.WithMaxConcurrentAnalysis(cfg.MaxConcurrentRCA),
		sidekickslack.WithCoordinatorMetrics(metrics),
		sidekickslack.WithCoordinatorActuator(actuator),
		sidekickslack.WithCoordinatorAuthorizer(authorizer),
		sidekickslack.WithCoordinatorAuditor(auditor),
	)
	if err != nil {
		return err
	}

	receiver, err := sidekickslack.NewReceiver(socket.Events, socket, coordinator)
	if err != nil {
		return err
	}

	// The alert-driven detect stage (PRD section 12): a SigNoz notification
	// channel posts here, and Diagnose/Announce run through exactly the same
	// RCA pipeline and Coordinator the /diagnose slash command uses.
	webhookHandler := detect.NewHandler(webhookSecret, rcaAdapter, coordinator)
	webhookServer := &http.Server{Addr: webhookListen, Handler: webhookHandler}
	webhookErrs := make(chan error, 1)
	go func() {
		defer close(webhookErrs)
		if err := webhookServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			webhookErrs <- fmt.Errorf("webhook server: %w", err)
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), webhookShutdownTimeout)
		defer cancel()
		_ = webhookServer.Shutdown(shutdownCtx)
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("slack adapter starting",
		"channel", cfg.DefaultChannel,
		"default_environment", cfg.DefaultEnvironment,
		"session_ttl", ttl.String(),
		"max_concurrent_rca", cfg.MaxConcurrentRCA,
		"webhook_listen", webhookListen,
		// Credentials are reported by variable name only, never by value.
		"bot_token_env", cfg.BotTokenEnv,
		"app_token_env", cfg.AppTokenEnv,
	)

	err = supervise(ctx, socket, receiver, coordinator)

	// The webhook server's own goroutine has nothing else to synchronize
	// with, so drain it non-blockingly rather than making supervise (and
	// every existing test that constructs it) aware of a fifth component.
	select {
	case webhookErr, ok := <-webhookErrs:
		if ok && webhookErr != nil && err == nil {
			err = webhookErr
		}
	default:
	}
	return err
}

// socketRunner is the part of *socketmode.Client the supervisor drives, kept
// as an interface so the wiring can be tested without dialling Slack.
type socketRunner interface {
	RunContext(ctx context.Context) error
}

// receiverRunner is the part of *slack.Receiver the supervisor drives.
type receiverRunner interface {
	Run(ctx context.Context) error
}

// reaper closes idle sessions and announces it.
type reaper interface {
	ReapIdle(ctx context.Context)
}

// supervise runs the socket, the receiver and the idle sweep together, and
// stops all three when any one of them finishes.
//
// The order of shutdown matters: the socket stops first, so no new envelopes
// arrive, and the receiver then drains the work it has already acknowledged to
// Slack. Abandoning acknowledged work would silently lose a human's message.
func supervise(
	ctx context.Context, socket socketRunner, receiver receiverRunner, sweeper reaper,
) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	errs := make(chan error, 2)

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()
		if err := socket.RunContext(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errs <- fmt.Errorf("slack socket: %w", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()
		if err := receiver.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errs <- fmt.Errorf("slack receiver: %w", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(reapInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweeper.ReapIdle(ctx)
			}
		}
	}()

	wg.Wait()
	close(errs)

	slog.Info("slack adapter stopped")

	// Report the first real failure; a clean shutdown reports nothing.
	for err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
