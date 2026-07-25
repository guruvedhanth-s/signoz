package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/config"
	sidekickslack "github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify/slack"
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

// runWatch runs the Slack adapter: it dials Slack over Socket Mode, routes
// inbound events into incident sessions, and sweeps idle ones.
//
// It deliberately does not serve HTTP. The alert webhook that starts an
// alert-driven diagnosis belongs to the detection track; when it exists, it
// calls Coordinator.Announce and everything below is unchanged.
func runWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	configPath := fs.String("config", DefaultConfigPath, "path to sidekick.yaml")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		// Loading is strict about credentials on purpose: a missing or swapped
		// token must fail here, at startup, not during an incident.
		return err
	}

	return watchWithConfig(cfg.Notify.Slack)
}

func watchWithConfig(cfg config.SlackConfig) error {
	botToken, err := cfg.BotToken()
	if err != nil {
		return err
	}
	appToken, err := cfg.AppToken()
	if err != nil {
		return err
	}

	api := slackapi.New(botToken, slackapi.OptionAppLevelToken(appToken))
	socket := socketmode.New(api)

	client, err := sidekickslack.New(cfg, sidekickslack.WithPoster(api))
	if err != nil {
		return err
	}

	ttl, err := cfg.SessionTTLDuration()
	if err != nil {
		return err
	}
	sessions := session.NewManager(session.WithTTL(ttl))

	// The analysis engine lives behind the RCA interface and is attached here
	// when it is available. Until then the adapter says so plainly rather than
	// inventing answers.
	coordinator, err := sidekickslack.NewCoordinator(
		client, sessions, sidekickslack.UnavailableRCA{},
		sidekickslack.WithDefaultEnvironment(cfg.DefaultEnvironment),
		sidekickslack.WithMaxConcurrentAnalysis(cfg.MaxConcurrentRCA),
	)
	if err != nil {
		return err
	}

	receiver, err := sidekickslack.NewReceiver(socket.Events, socket, coordinator)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("slack adapter starting",
		"channel", cfg.DefaultChannel,
		"default_environment", cfg.DefaultEnvironment,
		"session_ttl", ttl.String(),
		"max_concurrent_rca", cfg.MaxConcurrentRCA,
		// Credentials are reported by variable name only, never by value.
		"bot_token_env", cfg.BotTokenEnv,
		"app_token_env", cfg.AppTokenEnv,
	)

	return supervise(ctx, socket, receiver, coordinator)
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
