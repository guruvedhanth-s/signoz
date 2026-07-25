package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/config"
)

const watchConfig = `
notify:
  slack:
    default_channel: "#sre-sidekick"
    default_environment: prod
    session_ttl: 30m
    max_concurrent_rca: 2
`

func writeWatchConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sidekick.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestRunWatchReportsAMissingConfig(t *testing.T) {
	err := runWatch([]string{"--config", filepath.Join(t.TempDir(), "absent.yaml")})
	if err == nil {
		t.Fatal("runWatch() error = nil, want a config error")
	}
	if !strings.Contains(err.Error(), "read sidekick config") {
		t.Errorf("error = %q, want it to name the unreadable config", err)
	}
}

// A missing or swapped token must stop the process at startup, not surface
// mid-incident as an opaque Slack error.
func TestRunWatchFailsFastOnCredentials(t *testing.T) {
	path := writeWatchConfig(t, watchConfig)

	t.Setenv("SLACK_BOT_TOKEN", "")
	t.Setenv("SLACK_APP_TOKEN", "xapp-test")
	err := runWatch([]string{"--config", path})
	if err == nil || !strings.Contains(err.Error(), "SLACK_BOT_TOKEN") {
		t.Fatalf("error = %v, want the missing bot token named", err)
	}

	// The classic mistake: the app token pasted into the bot token slot.
	t.Setenv("SLACK_BOT_TOKEN", "xapp-wrong-slot")
	err = runWatch([]string{"--config", path})
	if err == nil || !strings.Contains(err.Error(), "xoxb-") {
		t.Fatalf("error = %v, want the swapped token explained", err)
	}
}

// The alert-driven webhook receiver fails closed with no secret configured
// (detect.Handler refuses every request), so a missing secret must stop
// watch at startup rather than serve a receiver that can never accept
// anything (PRD section 19).
func TestRunWatchRequiresAWebhookSecret(t *testing.T) {
	path := writeWatchConfig(t, watchConfig)

	t.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("SLACK_APP_TOKEN", "xapp-test")
	t.Setenv("OPENROUTER_API_KEY", "test-openrouter-key")
	t.Setenv("SIDEKICK_WEBHOOK_SECRET", "")

	err := runWatch([]string{"--config", path})
	if err == nil || !strings.Contains(err.Error(), "SIDEKICK_WEBHOOK_SECRET") {
		t.Fatalf("error = %v, want the missing webhook secret named", err)
	}
}

func TestRunWatchRejectsUnknownFlags(t *testing.T) {
	if err := runWatch([]string{"--nonsense"}); err == nil {
		t.Fatal("runWatch() error = nil, want a flag error")
	}
}

func TestDefaultConfigPathIsTheShippedFile(t *testing.T) {
	if _, err := os.Stat(filepath.Join("..", "..", DefaultConfigPath)); err != nil {
		t.Errorf("the default config path does not exist in the repository: %v", err)
	}
}

func TestWatchConfigDefaults(t *testing.T) {
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("SLACK_APP_TOKEN", "xapp-test")
	t.Setenv("OPENROUTER_API_KEY", "test-openrouter-key")

	cfg, err := config.Load(writeWatchConfig(t, watchConfig))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Notify.Slack.DefaultEnvironment != "prod" {
		t.Errorf("default environment = %q", cfg.Notify.Slack.DefaultEnvironment)
	}
	if cfg.Notify.Slack.MaxConcurrentRCA != 2 {
		t.Errorf("max concurrent rca = %d", cfg.Notify.Slack.MaxConcurrentRCA)
	}
}

// fakeRunner stands in for the socket connection and the receiver.
type fakeRunner struct {
	started atomic.Bool
	stopped atomic.Bool
	err     error
	// exitEarly makes the runner return immediately, standing in for a
	// connection that drops.
	exitEarly bool
}

func (f *fakeRunner) RunContext(ctx context.Context) error { return f.run(ctx) }
func (f *fakeRunner) Run(ctx context.Context) error        { return f.run(ctx) }

func (f *fakeRunner) run(ctx context.Context) error {
	f.started.Store(true)
	if f.exitEarly {
		f.stopped.Store(true)
		return f.err
	}
	<-ctx.Done()
	f.stopped.Store(true)
	if f.err != nil {
		return f.err
	}
	return ctx.Err()
}

type fakeReaper struct{ sweeps atomic.Int32 }

func (f *fakeReaper) ReapIdle(context.Context) { f.sweeps.Add(1) }

func TestSuperviseStopsEverythingOnCancel(t *testing.T) {
	socket := &fakeRunner{}
	receiver := &fakeRunner{}
	sweeper := &fakeReaper{}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervise(ctx, socket, receiver, sweeper) }()

	waitUntil(t, func() bool { return socket.started.Load() && receiver.started.Load() })
	cancel()

	select {
	case err := <-done:
		// A cancelled run is a clean shutdown, not a failure.
		if err != nil {
			t.Errorf("supervise() error = %v, want nil on cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("supervise() did not return after cancellation")
	}

	if !socket.stopped.Load() || !receiver.stopped.Load() {
		t.Error("not every component stopped")
	}
}

// If the socket dies, the receiver and the sweep must stop too, rather than
// leaving a half-running process that looks healthy.
func TestSuperviseStopsWhenTheSocketFails(t *testing.T) {
	socket := &fakeRunner{exitEarly: true, err: errors.New("invalid_auth")}
	receiver := &fakeRunner{}

	done := make(chan error, 1)
	go func() { done <- supervise(context.Background(), socket, receiver, &fakeReaper{}) }()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "invalid_auth") {
			t.Errorf("supervise() error = %v, want the socket failure reported", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("supervise() did not return after the socket failed")
	}

	if !receiver.stopped.Load() {
		t.Error("the receiver kept running after the socket died")
	}
}

func TestSuperviseStopsWhenTheReceiverFails(t *testing.T) {
	socket := &fakeRunner{}
	receiver := &fakeRunner{exitEarly: true, err: errors.New("stream closed")}

	done := make(chan error, 1)
	go func() { done <- supervise(context.Background(), socket, receiver, &fakeReaper{}) }()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "stream closed") {
			t.Errorf("supervise() error = %v, want the receiver failure reported", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("supervise() did not return after the receiver failed")
	}
	if !socket.stopped.Load() {
		t.Error("the socket kept running after the receiver died")
	}
}

// The sweep must not outlive the process: an orphaned ticker is a goroutine
// leak.
func TestSuperviseStopsTheIdleSweep(t *testing.T) {
	sweeper := &fakeReaper{}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- supervise(ctx, &fakeRunner{}, &fakeRunner{}, sweeper) }()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervise() did not return")
	}

	before := sweeper.sweeps.Load()
	time.Sleep(20 * time.Millisecond)
	if sweeper.sweeps.Load() != before {
		t.Error("the idle sweep kept running after shutdown")
	}
}

func waitUntil(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met within the timeout")
}
