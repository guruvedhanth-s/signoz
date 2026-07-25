package slack

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// syncBuffer is a log sink that a test goroutine can read while the receiver's
// goroutines are still writing to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// recordingAcker records which envelopes were acknowledged, and when relative
// to the handler doing its work.
type recordingAcker struct {
	mu    sync.Mutex
	acked []string
	err   error
}

func (a *recordingAcker) Ack(req socketmode.Request, _ ...any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.acked = append(a.acked, req.EnvelopeID)
	return a.err
}

func (a *recordingAcker) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.acked)
}

// recordingHandler is the test double for the phase 6 conversation logic.
type recordingHandler struct {
	mu           sync.Mutex
	messages     []Message
	interactions []Interaction
	commands     []Command

	done    chan struct{}
	started chan struct{}
	block   chan struct{}
	panic   bool
}

func newRecordingHandler() *recordingHandler {
	return &recordingHandler{
		done:    make(chan struct{}, 64),
		started: make(chan struct{}, 64),
	}
}

func (h *recordingHandler) OnMessage(_ context.Context, msg Message) {
	if h.panic {
		panic("handler exploded")
	}
	select {
	case h.started <- struct{}{}:
	default:
	}
	if h.block != nil {
		<-h.block
	}
	h.mu.Lock()
	h.messages = append(h.messages, msg)
	h.mu.Unlock()
	h.done <- struct{}{}
}

func (h *recordingHandler) OnInteraction(_ context.Context, interaction Interaction) {
	h.mu.Lock()
	h.interactions = append(h.interactions, interaction)
	h.mu.Unlock()
	h.done <- struct{}{}
}

func (h *recordingHandler) OnCommand(_ context.Context, cmd Command) {
	h.mu.Lock()
	h.commands = append(h.commands, cmd)
	h.mu.Unlock()
	h.done <- struct{}{}
}

func (h *recordingHandler) waitFor(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-h.done:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for handler call %d of %d", i+1, n)
		}
	}
}

func (h *recordingHandler) counts() (int, int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.messages), len(h.interactions), len(h.commands)
}

// startReceiver runs a receiver over a channel the test controls.
func startReceiver(t *testing.T, handler Handler, opts ...ReceiverOption) (
	chan socketmode.Event, *recordingAcker, *syncBuffer, func(),
) {
	t.Helper()

	events := make(chan socketmode.Event, 16)
	acker := &recordingAcker{}
	logs := &syncBuffer{}

	receiver, err := NewReceiver(events, acker, handler, append([]ReceiverOption{
		WithReceiverLogger(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
	}, opts...)...)
	if err != nil {
		t.Fatalf("NewReceiver() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = receiver.Run(ctx)
	}()

	stop := func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(2 * time.Second):
			t.Error("receiver did not stop")
		}
	}
	t.Cleanup(stop)
	return events, acker, logs, stop
}

func messageEvent(envelopeID, eventID, text string) socketmode.Event {
	inner := &slackevents.MessageEvent{
		Channel:         "C1",
		ThreadTimeStamp: "1720000000.0001",
		TimeStamp:       "1720000100.0002",
		User:            "U1",
		Text:            text,
	}
	return socketmode.Event{
		Type: socketmode.EventTypeEventsAPI,
		Data: slackevents.EventsAPIEvent{
			TeamID:     "T1",
			Data:       slackevents.EventsAPICallbackEvent{EventID: eventID},
			InnerEvent: slackevents.EventsAPIInnerEvent{Data: inner},
		},
		Request: &socketmode.Request{EnvelopeID: envelopeID},
	}
}

func interactionEvent(envelopeID, actionID string) socketmode.Event {
	callback := slack.InteractionCallback{
		Type: slack.InteractionTypeBlockActions,
		User: slack.User{ID: "U1", Name: "priya"},
		Team: slack.Team{ID: "T1"},
	}
	callback.Channel.ID = "C1"
	callback.Container.MessageTs = "1720000000.0001"
	callback.Message.Timestamp = "1720000000.0001"
	callback.ActionCallback.BlockActions = []*slack.BlockAction{
		{ActionID: actionID, Value: "corr-123"},
	}
	return socketmode.Event{
		Type:    socketmode.EventTypeInteractive,
		Data:    callback,
		Request: &socketmode.Request{EnvelopeID: envelopeID},
	}
}

func commandEvent(envelopeID, text string) socketmode.Event {
	return socketmode.Event{
		Type: socketmode.EventTypeSlashCommand,
		Data: slack.SlashCommand{
			Command:   "/diagnose",
			Text:      text,
			ChannelID: "C1",
			UserID:    "U1",
			UserName:  "priya",
			TeamID:    "T1",
			TriggerID: "trigger-1",
		},
		Request: &socketmode.Request{EnvelopeID: envelopeID},
	}
}

func TestMessageIsAckedAndDispatched(t *testing.T) {
	handler := newRecordingHandler()
	events, acker, _, _ := startReceiver(t, handler)

	events <- messageEvent("env-1", "Ev001", "why do you think it is the timeout?")
	handler.waitFor(t, 1)

	if acker.count() != 1 {
		t.Errorf("acks = %d, want 1", acker.count())
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	got := handler.messages[0]
	if got.Text != "why do you think it is the timeout?" {
		t.Errorf("text = %q", got.Text)
	}
	if got.ChannelID != "C1" || got.ThreadTS != "1720000000.0001" {
		t.Errorf("session key = %s/%s, want C1/1720000000.0001", got.ChannelID, got.ThreadTS)
	}
	if got.UserID != "U1" {
		t.Errorf("user = %q", got.UserID)
	}
	if !got.InThread() {
		t.Error("threaded reply was not recognised as in-thread")
	}
}

func TestInteractionIsDispatched(t *testing.T) {
	handler := newRecordingHandler()
	events, _, _, _ := startReceiver(t, handler)

	events <- interactionEvent("env-2", ActionApprove)
	handler.waitFor(t, 1)

	handler.mu.Lock()
	defer handler.mu.Unlock()
	got := handler.interactions[0]
	if got.ActionID != ActionApprove {
		t.Errorf("action = %q, want %q", got.ActionID, ActionApprove)
	}
	if got.Value != "corr-123" {
		t.Errorf("value = %q, want the correlation id", got.Value)
	}
	if got.ChannelID != "C1" || got.ThreadTS != "1720000000.0001" {
		t.Errorf("session key = %s/%s", got.ChannelID, got.ThreadTS)
	}
	if got.UserID != "U1" {
		t.Errorf("user = %q, want the clicker on the record", got.UserID)
	}
}

func TestCommandIsDispatched(t *testing.T) {
	handler := newRecordingHandler()
	events, _, _, _ := startReceiver(t, handler)

	events <- commandEvent("env-3", "support-agent")
	handler.waitFor(t, 1)

	handler.mu.Lock()
	defer handler.mu.Unlock()
	got := handler.commands[0]
	if got.Command != "/diagnose" || got.Text != "support-agent" {
		t.Errorf("command = %q %q", got.Command, got.Text)
	}
}

// Slack redelivers anything it thinks was not handled; without dedup the same
// human turn is processed twice.
func TestDuplicateDeliveryIsDroppedButStillAcked(t *testing.T) {
	handler := newRecordingHandler()
	events, _, logs, _ := startReceiver(t, handler)

	events <- messageEvent("env-1", "Ev001", "same message")
	handler.waitFor(t, 1)

	retry := messageEvent("env-1-retry", "Ev001", "same message")
	retry.Request.RetryAttempt = 1
	retry.Request.RetryReason = "timeout"
	events <- retry

	// The drop is logged after the ack, so wait for the log, not the ack.
	waitFor(t, func() bool { return strings.Contains(logs.String(), "duplicate") })

	messages, _, _ := handler.counts()
	if messages != 1 {
		t.Errorf("handler calls = %d, want 1 despite the redelivery", messages)
	}
	if !strings.Contains(logs.String(), "duplicate") {
		t.Errorf("the dropped duplicate was not logged\n%s", logs)
	}
	if !strings.Contains(logs.String(), "redelivered") {
		t.Errorf("the redelivery was not logged\n%s", logs)
	}
}

// Distinct events must not be collapsed by the dedup cache.
func TestDistinctEventsAreAllDispatched(t *testing.T) {
	handler := newRecordingHandler()
	events, _, _, _ := startReceiver(t, handler)

	events <- messageEvent("env-1", "Ev001", "first")
	events <- messageEvent("env-2", "Ev002", "second")
	handler.waitFor(t, 2)

	messages, _, _ := handler.counts()
	if messages != 2 {
		t.Errorf("handler calls = %d, want 2", messages)
	}
}

// The sidekick's own posts arrive as message events. Acting on them would make
// it answer itself in a loop.
func TestBotMessagesAreIgnored(t *testing.T) {
	handler := newRecordingHandler()
	events, acker, _, _ := startReceiver(t, handler)

	botMessage := messageEvent("env-bot", "Ev-bot", "posted by the sidekick")
	inner := botMessage.Data.(slackevents.EventsAPIEvent).InnerEvent.Data.(*slackevents.MessageEvent)
	inner.BotID = "B123"
	events <- botMessage

	// It is still acknowledged; an unacked envelope is simply redelivered.
	waitFor(t, func() bool { return acker.count() == 1 })

	messages, _, _ := handler.counts()
	if messages != 0 {
		t.Errorf("handler calls = %d, want none for a bot message", messages)
	}
}

func TestEmptyOrUserlessMessagesAreIgnored(t *testing.T) {
	handler := newRecordingHandler()
	events, acker, _, _ := startReceiver(t, handler)

	empty := messageEvent("env-empty", "Ev-empty", "")
	events <- empty

	userless := messageEvent("env-userless", "Ev-userless", "hello")
	userless.Data.(slackevents.EventsAPIEvent).InnerEvent.Data.(*slackevents.MessageEvent).User = ""
	events <- userless

	waitFor(t, func() bool { return acker.count() == 2 })

	messages, _, _ := handler.counts()
	if messages != 0 {
		t.Errorf("handler calls = %d, want none", messages)
	}
}

// Slack redelivers anything not acked within three seconds, so the ack must
// not wait for the work.
func TestAckDoesNotWaitForSlowWork(t *testing.T) {
	handler := newRecordingHandler()
	handler.block = make(chan struct{})
	events, acker, _, _ := startReceiver(t, handler)

	events <- messageEvent("env-slow", "Ev-slow", "a question needing an LLM call")

	waitFor(t, func() bool { return acker.count() == 1 })

	messages, _, _ := handler.counts()
	if messages != 0 {
		t.Error("handler finished before the ack was asserted; the test is not proving anything")
	}
	close(handler.block)
	handler.waitFor(t, 1)
}

// Work is scheduled on a context that must outlive the envelope, or every
// follow-up would be cancelled the moment it was acked.
func TestWorkContextOutlivesTheEnvelope(t *testing.T) {
	deadlines := make(chan time.Time, 1)
	handler := &contextProbeHandler{deadlines: deadlines}

	events, _, _, _ := startReceiver(t, handler, WithWorkTimeout(90*time.Second))
	events <- messageEvent("env-ctx", "Ev-ctx", "question")

	select {
	case deadline := <-deadlines:
		remaining := time.Until(deadline)
		if remaining < 30*time.Second {
			t.Errorf("work context deadline in %v, want the configured work timeout", remaining)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler was never called")
	}
}

type contextProbeHandler struct {
	deadlines chan time.Time
}

func (h *contextProbeHandler) OnMessage(ctx context.Context, _ Message) {
	if err := ctx.Err(); err != nil {
		h.deadlines <- time.Time{}
		return
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		h.deadlines <- time.Now().Add(time.Hour)
		return
	}
	h.deadlines <- deadline
}
func (h *contextProbeHandler) OnInteraction(context.Context, Interaction) {}
func (h *contextProbeHandler) OnCommand(context.Context, Command)         {}

// A burst must not spawn unbounded goroutines or unbounded paid work.
func TestWorkerPoolBoundsConcurrency(t *testing.T) {
	handler := &countingHandler{done: make(chan struct{}, 64)}
	events, _, _, _ := startReceiver(t, handler, WithWorkers(2, 64))

	for i := 0; i < 16; i++ {
		events <- messageEvent("env-"+string(rune('a'+i)), "Ev-"+string(rune('a'+i)), "question")
	}
	for i := 0; i < 16; i++ {
		select {
		case <-handler.done:
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d of 16 events were processed", i)
		}
	}

	if peak := handler.peak.Load(); peak > 2 {
		t.Errorf("peak concurrency = %d, want at most the 2 configured workers", peak)
	}
}

type countingHandler struct {
	inFlight atomic.Int32
	peak     atomic.Int32
	done     chan struct{}
}

func (h *countingHandler) OnMessage(context.Context, Message) {
	current := h.inFlight.Add(1)
	for {
		peak := h.peak.Load()
		if current <= peak || h.peak.CompareAndSwap(peak, current) {
			break
		}
	}
	time.Sleep(5 * time.Millisecond)
	h.inFlight.Add(-1)
	h.done <- struct{}{}
}
func (h *countingHandler) OnInteraction(context.Context, Interaction) {}
func (h *countingHandler) OnCommand(context.Context, Command)         {}

// Overload shows up as a logged drop rather than an invisible backlog.
func TestFullQueueShedsLoadLoudly(t *testing.T) {
	handler := newRecordingHandler()
	handler.block = make(chan struct{})
	events, _, logs, _ := startReceiver(t, handler, WithWorkers(1, 1))

	for i := 0; i < 12; i++ {
		events <- messageEvent("env-"+string(rune('a'+i)), "Ev-"+string(rune('a'+i)), "question")
	}

	waitFor(t, func() bool { return strings.Contains(logs.String(), "queue full") })
	close(handler.block)
}

// One malformed event must not take the receiver down.
func TestHandlerPanicIsContained(t *testing.T) {
	panicking := newRecordingHandler()
	panicking.panic = true
	events, acker, logs, _ := startReceiver(t, panicking)

	events <- messageEvent("env-panic", "Ev-panic", "boom")
	waitFor(t, func() bool { return strings.Contains(logs.String(), "panicked") })

	// The receiver is still alive and still acking.
	events <- interactionEvent("env-after", ActionClose)
	waitFor(t, func() bool { return acker.count() == 2 })
}

func TestConnectionLifecycleEventsAreLoggedNotDispatched(t *testing.T) {
	handler := newRecordingHandler()
	events, acker, logs, _ := startReceiver(t, handler)

	events <- socketmode.Event{Type: socketmode.EventTypeConnecting}
	events <- socketmode.Event{Type: socketmode.EventTypeConnected}
	events <- socketmode.Event{Type: socketmode.EventTypeDisconnect}
	events <- socketmode.Event{Type: socketmode.EventTypeInvalidAuth}

	waitFor(t, func() bool { return strings.Contains(logs.String(), "rejected the app-level token") })

	if acker.count() != 0 {
		t.Errorf("acks = %d, want none for lifecycle events", acker.count())
	}
	messages, interactions, commands := handler.counts()
	if messages+interactions+commands != 0 {
		t.Error("a lifecycle event reached the handler")
	}
}

// A failed ack means Slack will redeliver, which is survivable; it must not
// stop the work that was already decoded.
func TestFailedAckIsLoggedAndWorkStillRuns(t *testing.T) {
	handler := newRecordingHandler()
	events := make(chan socketmode.Event, 4)
	acker := &recordingAcker{err: errors.New("response too large")}
	logs := &syncBuffer{}

	receiver, err := NewReceiver(events, acker, handler,
		WithReceiverLogger(slog.New(slog.NewTextHandler(logs, nil))))
	if err != nil {
		t.Fatalf("NewReceiver() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = receiver.Run(ctx) }()

	events <- messageEvent("env-ack-fail", "Ev-ack-fail", "question")
	handler.waitFor(t, 1)

	if !strings.Contains(logs.String(), "could not acknowledge") {
		t.Errorf("the failed ack was not logged\n%s", logs.String())
	}
}

func TestRunStopsWhenTheStreamCloses(t *testing.T) {
	handler := newRecordingHandler()
	events := make(chan socketmode.Event)
	receiver, err := NewReceiver(events, &recordingAcker{}, handler,
		WithReceiverLogger(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))))
	if err != nil {
		t.Fatalf("NewReceiver() error = %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- receiver.Run(context.Background()) }()

	close(events)
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() error = %v, want nil on a closed stream", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return after the stream closed")
	}
}

// Work already acked to Slack must finish, or a human's message is silently
// lost on shutdown.
func TestShutdownDrainsAcceptedWork(t *testing.T) {
	handler := newRecordingHandler()
	handler.block = make(chan struct{})

	events := make(chan socketmode.Event, 4)
	receiver, err := NewReceiver(events, &recordingAcker{}, handler,
		WithReceiverLogger(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))))
	if err != nil {
		t.Fatalf("NewReceiver() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = receiver.Run(ctx)
	}()

	events <- messageEvent("env-drain", "Ev-drain", "question")
	// Cancel only once the work is genuinely in flight, or the test proves
	// nothing about draining.
	select {
	case <-handler.started:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never started")
	}

	cancel()
	// Run must not return until the in-flight work completes.
	select {
	case <-done:
		t.Fatal("Run() returned while work was still in flight")
	case <-time.After(50 * time.Millisecond):
	}

	close(handler.block)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not drain and return")
	}

	messages, _, _ := handler.counts()
	if messages != 1 {
		t.Errorf("handler calls = %d, want the accepted work to have finished", messages)
	}
}

func TestNewReceiverValidatesItsInputs(t *testing.T) {
	events := make(chan socketmode.Event)
	handler := newRecordingHandler()

	if _, err := NewReceiver(nil, &recordingAcker{}, handler); err == nil {
		t.Error("NewReceiver() with no stream returned no error")
	}
	if _, err := NewReceiver(events, nil, handler); err == nil {
		t.Error("NewReceiver() with no acker returned no error")
	}
	if _, err := NewReceiver(events, &recordingAcker{}, nil); err == nil {
		t.Error("NewReceiver() with no handler returned no error")
	}
}

func TestDedupCacheExpiresAndBoundsMemory(t *testing.T) {
	now := time.Unix(0, 0)
	cache := newDedupCache(time.Minute, 3, func() time.Time { return now })

	if !cache.add("a") {
		t.Error("first sighting of an id was reported as a duplicate")
	}
	if cache.add("a") {
		t.Error("second sighting of an id was not reported as a duplicate")
	}

	now = now.Add(2 * time.Minute)
	if !cache.add("a") {
		t.Error("an id past its TTL was still treated as a duplicate")
	}

	// An empty id cannot be deduplicated; collapsing all of them into one
	// entry would silently drop unrelated events.
	if !cache.add("") || !cache.add("") {
		t.Error("empty ids were deduplicated against each other")
	}

	for _, id := range []string{"b", "c", "d", "e", "f"} {
		cache.add(id)
	}
	if len(cache.seen) > 3 {
		t.Errorf("cache holds %d entries, want at most the capacity of 3", len(cache.seen))
	}
}

func TestMessageInThread(t *testing.T) {
	root := Message{ThreadTS: "1", MessageTS: "1"}
	if root.InThread() {
		t.Error("a thread root was reported as a threaded reply")
	}
	if (Message{MessageTS: "1"}).InThread() {
		t.Error("a top-level message was reported as a threaded reply")
	}
	if !(Message{ThreadTS: "1", MessageTS: "2"}).InThread() {
		t.Error("a threaded reply was not recognised")
	}
}

func waitFor(t *testing.T, condition func() bool) {
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
