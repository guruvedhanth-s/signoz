package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/config"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
)

// fakePoster stands in for the Slack API. It records what would have been sent
// and replays a scripted sequence of results, so the delivery contract is
// verified without a Slack workspace.
type fakePoster struct {
	mu sync.Mutex

	calls   []fakeCall
	results []fakeResult
}

type fakeCall struct {
	channel string
	options []slack.MsgOption
}

type fakeResult struct {
	channel   string
	timestamp string
	err       error
}

func (f *fakePoster) PostMessageContext(
	_ context.Context, channelID string, options ...slack.MsgOption,
) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, fakeCall{channel: channelID, options: options})
	if len(f.results) == 0 {
		return channelID, "1720000000.000100", nil
	}
	result := f.results[0]
	if len(f.results) > 1 {
		f.results = f.results[1:]
	}
	if result.channel == "" {
		result.channel = channelID
	}
	return result.channel, result.timestamp, result.err
}

func (f *fakePoster) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// wireRecorder is a stand-in Slack API served over httptest. The message
// options only become a payload inside the Slack library's request builder, so
// asserting on what Slack would actually receive means letting a real
// *slack.Client build the request against this server. No network is used.
type wireRecorder struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []url.Values
}

func newWireRecorder(t *testing.T) *wireRecorder {
	t.Helper()
	recorder := &wireRecorder{}
	recorder.server = httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse form: %v", err)
			}
			recorder.mu.Lock()
			recorder.requests = append(recorder.requests, r.Form)
			recorder.mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"channel":"C0WIRE","ts":"1720000000.000100"}`))
		},
	))
	t.Cleanup(recorder.server.Close)
	return recorder
}

func (w *wireRecorder) request(t *testing.T, index int) url.Values {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if index >= len(w.requests) {
		t.Fatalf("request %d not made; got %d requests", index, len(w.requests))
	}
	return w.requests[index]
}

func (w *wireRecorder) blocks(t *testing.T, index int) []slack.Block {
	t.Helper()
	raw := w.request(t, index).Get("blocks")
	if raw == "" {
		return nil
	}
	var set slack.Blocks
	if err := json.Unmarshal([]byte(raw), &set); err != nil {
		t.Fatalf("unmarshal blocks: %v", err)
	}
	return set.BlockSet
}

// wireClient wires the adapter to a real *slack.Client pointed at the
// recorder.
func wireClient(t *testing.T, recorder *wireRecorder, opts ...Option) *Client {
	t.Helper()
	api := slack.New("xoxb-test", slack.OptionAPIURL(recorder.server.URL+"/"))
	client, err := New(testConfig(), append([]Option{
		WithPoster(api),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	}, opts...)...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return client
}

// testClient builds a client wired to a fake transport, a captured logger, and
// a clock that does not actually sleep.
func testClient(t *testing.T, fake *fakePoster, opts ...Option) (*Client, *bytes.Buffer, *[]time.Duration) {
	t.Helper()

	logs := &bytes.Buffer{}
	slept := &[]time.Duration{}
	now := time.Unix(0, 0)

	base := []Option{
		WithPoster(fake),
		WithLogger(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		withoutJitter(),
		withClock(
			func() time.Time { return now },
			func(ctx context.Context, d time.Duration) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				*slept = append(*slept, d)
				now = now.Add(d)
				return nil
			},
		),
	}

	client, err := New(testConfig(), append(base, opts...)...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return client, logs, slept
}

func testConfig() config.SlackConfig {
	return config.SlackConfig{
		BotTokenEnv:      "SLACK_BOT_TOKEN",
		SigningSecretEnv: "SLACK_SIGNING_SECRET",
		DefaultChannel:   "#sre-sidekick",
		SessionTTL:       "30m",
		MaxConcurrentRCA: 5,
	}
}

func TestClientImplementsNotifier(t *testing.T) {
	var _ notify.Notifier = (*Client)(nil)
}

// The posted blocks must be exactly what the renderer produced: the client
// carries the message, it does not reshape it.
func TestNotifyDiagnosisPostsRenderedBlocks(t *testing.T) {
	recorder := newWireRecorder(t)
	logs := &bytes.Buffer{}
	client := wireClient(t, recorder,
		WithLogger(slog.New(slog.NewTextHandler(logs, nil))),
	)

	d := diagnosedFixture()
	if err := client.NotifyDiagnosis(context.Background(), d); err != nil {
		t.Fatalf("NotifyDiagnosis() error = %v", err)
	}

	sent := recorder.request(t, 0)
	if sent.Get("channel") != "#sre-sidekick" {
		t.Errorf("channel = %q, want the configured default", sent.Get("channel"))
	}

	text, blocks := sent.Get("text"), recorder.blocks(t, 0)
	want, err := json.Marshal(slack.Blocks{BlockSet: DiagnosisBlocks(d)})
	if err != nil {
		t.Fatalf("marshal expected blocks: %v", err)
	}
	got, err := json.Marshal(slack.Blocks{BlockSet: blocks})
	if err != nil {
		t.Fatalf("marshal posted blocks: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("posted blocks differ from DiagnosisBlocks output\ngot:  %s\nwant: %s", got, want)
	}

	// Without fallback text the message arrives blank in push notifications.
	if text == "" {
		t.Error("fallback text is empty")
	}
	for _, fragment := range []string{"support-agent", "14.2x", "-3.4%"} {
		if !strings.Contains(text, fragment) {
			t.Errorf("fallback text %q missing %q", text, fragment)
		}
	}

	// Every delivery is auditable by correlation id (PRD section 20).
	if !strings.Contains(logs.String(), "corr-123") {
		t.Errorf("success was not logged with the correlation id\n%s", logs)
	}
}

func TestPostDiagnosisReturnsThreadRoot(t *testing.T) {
	fake := &fakePoster{results: []fakeResult{{channel: "C123", timestamp: "1720000000.000100"}}}
	client, _, _ := testClient(t, fake)

	ref, err := client.PostDiagnosis(context.Background(), diagnosedFixture())
	if err != nil {
		t.Fatalf("PostDiagnosis() error = %v", err)
	}
	if ref.Channel != "C123" || ref.Timestamp != "1720000000.000100" {
		t.Errorf("PostRef = %+v, want the posted channel and message ts", ref)
	}
}

func TestNotifyIndeterminatePostsIndeterminateBlocks(t *testing.T) {
	recorder := newWireRecorder(t)
	client := wireClient(t, recorder)

	reason := notify.IndeterminateReason{
		CorrelationID: "corr-9",
		Service:       "support-agent",
		Environment:   "prod",
		Window:        "30d",
		Reason:        "missing agent_success_total for support-agent over 30d",
		Timestamp:     time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
	}
	if err := client.NotifyIndeterminate(context.Background(), reason); err != nil {
		t.Fatalf("NotifyIndeterminate() error = %v", err)
	}

	text, blocks := recorder.request(t, 0).Get("text"), recorder.blocks(t, 0)
	if !strings.Contains(text, "telemetry incomplete") {
		t.Errorf("fallback text = %q, want the indeterminate summary", text)
	}
	if len(blocks) != len(IndeterminateBlocks(reason)) {
		t.Errorf("posted %d blocks, want %d", len(blocks), len(IndeterminateBlocks(reason)))
	}
}

func TestRetriesTransientFailureThenSucceeds(t *testing.T) {
	fake := &fakePoster{results: []fakeResult{
		{err: slack.StatusCodeError{Code: 503, Status: "503 Service Unavailable"}},
		{err: errors.New("dial tcp: connection reset by peer")},
		{channel: "C1", timestamp: "1720000000.000200"},
	}}
	client, _, slept := testClient(t, fake)

	if err := client.NotifyDiagnosis(context.Background(), diagnosedFixture()); err != nil {
		t.Fatalf("NotifyDiagnosis() error = %v", err)
	}
	if fake.callCount() != 3 {
		t.Errorf("post attempts = %d, want 3", fake.callCount())
	}
	want := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond}
	if len(*slept) != len(want) {
		t.Fatalf("backoff waits = %v, want %v", *slept, want)
	}
	for i := range want {
		if (*slept)[i] != want[i] {
			t.Errorf("backoff waits = %v, want exponential %v", *slept, want)
		}
	}
}

// Slack states its own rate-limit delay; guessing a different one is worse.
func TestRateLimitDelayIsHonoured(t *testing.T) {
	fake := &fakePoster{results: []fakeResult{
		{err: &slack.RateLimitedError{RetryAfter: 3 * time.Second}},
		{channel: "C1", timestamp: "1"},
	}}
	client, _, slept := testClient(t, fake)

	if err := client.NotifyDiagnosis(context.Background(), diagnosedFixture()); err != nil {
		t.Fatalf("NotifyDiagnosis() error = %v", err)
	}
	if len(*slept) != 1 || (*slept)[0] != 3*time.Second {
		t.Errorf("waits = %v, want the Retry-After of 3s", *slept)
	}
}

// A rate-limit pause longer than the budget must not stall the diagnose loop.
func TestRetryBudgetStopsAnOverlongRateLimitWait(t *testing.T) {
	fake := &fakePoster{results: []fakeResult{
		{err: &slack.RateLimitedError{RetryAfter: 60 * time.Second}},
	}}
	client, logs, slept := testClient(t, fake)

	err := client.NotifyDiagnosis(context.Background(), diagnosedFixture())
	if err == nil {
		t.Fatal("NotifyDiagnosis() error = nil, want a budget-exhausted error")
	}
	if !strings.Contains(err.Error(), "retry budget") {
		t.Errorf("error = %q, want it to name the exhausted budget", err)
	}
	if len(*slept) != 0 {
		t.Errorf("waits = %v, want none: the wait exceeded the budget", *slept)
	}
	if !strings.Contains(logs.String(), "not delivered") {
		t.Errorf("undelivered message was not logged\n%s", logs)
	}
}

// Retrying a permanent error only delays the failure report.
func TestPermanentErrorsAreNotRetried(t *testing.T) {
	permanent := []error{
		errors.New("invalid_auth"),
		errors.New("channel_not_found"),
		errors.New("not_in_channel"),
		errors.New("msg_too_long"),
		slack.StatusCodeError{Code: 403, Status: "403 Forbidden"},
	}

	for _, slackErr := range permanent {
		t.Run(slackErr.Error(), func(t *testing.T) {
			fake := &fakePoster{results: []fakeResult{{err: slackErr}}}
			client, _, slept := testClient(t, fake)

			if err := client.NotifyDiagnosis(context.Background(), diagnosedFixture()); err == nil {
				t.Fatal("NotifyDiagnosis() error = nil, want the permanent failure")
			}
			if fake.callCount() != 1 {
				t.Errorf("post attempts = %d, want exactly 1", fake.callCount())
			}
			if len(*slept) != 0 {
				t.Errorf("waits = %v, want none", *slept)
			}
		})
	}
}

func TestGivesUpAfterMaxAttempts(t *testing.T) {
	fake := &fakePoster{results: []fakeResult{
		{err: slack.StatusCodeError{Code: 500, Status: "500 Internal Server Error"}},
	}}
	client, logs, _ := testClient(t, fake, WithRetryPolicy(RetryPolicy{
		MaxAttempts:    3,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
		TotalBudget:    time.Minute,
	}))

	err := client.NotifyDiagnosis(context.Background(), diagnosedFixture())
	if err == nil {
		t.Fatal("NotifyDiagnosis() error = nil, want the transport failure")
	}
	if fake.callCount() != 3 {
		t.Errorf("post attempts = %d, want 3", fake.callCount())
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %q, want it to carry the underlying cause", err)
	}
	if !strings.Contains(logs.String(), "attempts=3") {
		t.Errorf("attempt count was not logged\n%s", logs)
	}
}

func TestBackoffIsCappedByMaxBackoff(t *testing.T) {
	fake := &fakePoster{results: []fakeResult{
		{err: slack.StatusCodeError{Code: 500, Status: "500"}},
		{err: slack.StatusCodeError{Code: 500, Status: "500"}},
		{err: slack.StatusCodeError{Code: 500, Status: "500"}},
		{channel: "C1", timestamp: "1"},
	}}
	client, _, slept := testClient(t, fake, WithRetryPolicy(RetryPolicy{
		MaxAttempts:    4,
		InitialBackoff: time.Second,
		MaxBackoff:     2 * time.Second,
		TotalBudget:    time.Minute,
	}))

	if err := client.NotifyDiagnosis(context.Background(), diagnosedFixture()); err != nil {
		t.Fatalf("NotifyDiagnosis() error = %v", err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 2 * time.Second}
	for i, d := range want {
		if (*slept)[i] != d {
			t.Fatalf("waits = %v, want %v", *slept, want)
		}
	}
}

func TestCancelledContextStopsRetrying(t *testing.T) {
	fake := &fakePoster{results: []fakeResult{
		{err: slack.StatusCodeError{Code: 500, Status: "500"}},
	}}
	client, _, _ := testClient(t, fake)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := client.NotifyDiagnosis(ctx, diagnosedFixture())
	if err == nil {
		t.Fatal("NotifyDiagnosis() error = nil, want the context error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if fake.callCount() != 0 {
		t.Errorf("post attempts = %d, want none after cancellation", fake.callCount())
	}
}

func TestContextCancelledDuringBackoffAborts(t *testing.T) {
	fake := &fakePoster{results: []fakeResult{
		{err: slack.StatusCodeError{Code: 500, Status: "500"}},
	}}
	ctx, cancel := context.WithCancel(context.Background())

	logs := &bytes.Buffer{}
	client, err := New(testConfig(),
		WithPoster(fake),
		WithLogger(slog.New(slog.NewTextHandler(logs, nil))),
		withoutJitter(),
		withClock(time.Now, func(context.Context, time.Duration) error {
			cancel()
			return context.Canceled
		}),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if err := client.NotifyDiagnosis(ctx, diagnosedFixture()); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if fake.callCount() != 1 {
		t.Errorf("post attempts = %d, want 1 before the abort", fake.callCount())
	}
}

// A Slack outage is reported, not hidden: the caller decides what to do, and
// the audit log records the undelivered diagnosis either way.
func TestFailureIsReturnedAndLogged(t *testing.T) {
	fake := &fakePoster{results: []fakeResult{{err: errors.New("invalid_auth")}}}
	client, logs, _ := testClient(t, fake)

	err := client.NotifyDiagnosis(context.Background(), diagnosedFixture())
	if err == nil {
		t.Fatal("NotifyDiagnosis() error = nil, want the failure surfaced")
	}
	record := logs.String()
	for _, want := range []string{"not delivered", "corr-123", "support-agent"} {
		if !strings.Contains(record, want) {
			t.Errorf("log record missing %q\n%s", want, record)
		}
	}
}

// A panic in rendering or transport must not kill the process that is trying
// to report an incident.
func TestPanicIsConvertedToError(t *testing.T) {
	client, logs, _ := testClient(t, &fakePoster{})
	client.api = panickingPoster{}

	err := client.NotifyDiagnosis(context.Background(), diagnosedFixture())
	if err == nil {
		t.Fatal("NotifyDiagnosis() error = nil, want the panic reported as an error")
	}
	if !strings.Contains(err.Error(), "panic") {
		t.Errorf("error = %q, want it to mention the panic", err)
	}
	if !strings.Contains(logs.String(), "panicked") {
		t.Errorf("panic was not logged\n%s", logs)
	}
}

type panickingPoster struct{}

func (panickingPoster) PostMessageContext(
	context.Context, string, ...slack.MsgOption,
) (string, string, error) {
	panic("transport exploded")
}

func TestNewRequiresChannel(t *testing.T) {
	cfg := testConfig()
	cfg.DefaultChannel = ""

	if _, err := New(cfg, WithPoster(&fakePoster{})); err == nil {
		t.Fatal("New() error = nil, want the missing-channel error")
	}
}

func TestNewRequiresTokenWhenBuildingRealTransport(t *testing.T) {
	t.Setenv("SLACK_BOT_TOKEN", "")

	_, err := New(testConfig())
	if err == nil {
		t.Fatal("New() error = nil, want the missing-token error")
	}
	if !strings.Contains(err.Error(), "SLACK_BOT_TOKEN") {
		t.Errorf("error = %q, want it to name the variable", err)
	}
}

func TestNewRejectsInvalidRetryPolicy(t *testing.T) {
	if _, err := New(testConfig(), WithPoster(&fakePoster{}), WithRetryPolicy(RetryPolicy{})); err == nil {
		t.Fatal("New() error = nil, want the invalid policy rejected")
	}
}

func TestWithChannelOverride(t *testing.T) {
	fake := &fakePoster{}
	client, _, _ := testClient(t, fake, WithChannel("#incidents"))

	if err := client.NotifyDiagnosis(context.Background(), diagnosedFixture()); err != nil {
		t.Fatalf("NotifyDiagnosis() error = %v", err)
	}
	if fake.calls[0].channel != "#incidents" {
		t.Errorf("channel = %q, want the override", fake.calls[0].channel)
	}
}

func TestDefaultJitterStaysWithinBounds(t *testing.T) {
	for i := 0; i < 200; i++ {
		got := defaultJitter(time.Second)
		if got < 800*time.Millisecond || got > 1200*time.Millisecond {
			t.Fatalf("jitter = %v, want within +/-20%% of 1s", got)
		}
	}
	if got := defaultJitter(0); got != 0 {
		t.Errorf("jitter(0) = %v, want 0", got)
	}
}
