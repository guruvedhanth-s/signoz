package slack

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// This file is the inbound door.
//
// The adapter receives events over Slack **Socket Mode**: the sidekick dials
// *out* to Slack over a WebSocket and Slack pushes envelopes down it. That
// means there is no public HTTP endpoint to expose, no TLS to terminate, no
// tunnel to run, and nothing inbound to attack. It also means there are no
// request signatures to verify - signature verification exists to authenticate
// an inbound endpoint, and there isn't one; the socket itself is authenticated
// once, by the app-level token, when it is dialled.
//
// What Socket Mode does not change is the processing discipline:
//
//   - Slack expects an **ack within three seconds** and redelivers anything
//     slower, so every envelope is acked immediately and the real work happens
//     asynchronously (session design edge case E11).
//   - Because Slack redelivers, and because a reconnect can replay, deliveries
//     are **deduplicated** by id or the same human turn is processed twice.
//   - Work runs on a **bounded pool**, so a burst cannot spawn unbounded
//     goroutines or unbounded paid analysis (edge case E8).

// Handler is the conversation logic behind the socket. It is implemented in
// the next phase; this file only delivers well-formed, deduplicated events to
// it, one goroutine per event, with a context that outlives the ack.
//
// Implementations must be safe for concurrent use.
type Handler interface {
	// OnMessage handles a human message. Messages posted by bots are filtered
	// out before this is called.
	OnMessage(ctx context.Context, msg Message)
	// OnInteraction handles a Block Kit button click: approve, decline or
	// close.
	OnInteraction(ctx context.Context, interaction Interaction)
	// OnCommand handles a slash command, which starts a new session.
	OnCommand(ctx context.Context, cmd Command)
}

// acker acknowledges an envelope. *socketmode.Client satisfies it; tests use a
// recorder. Keeping it an interface is what lets the whole receiver be tested
// without a WebSocket.
//
// Ack returns an error because Slack silently drops oversized Socket Mode
// responses; a failed ack means the envelope will be redelivered, which the
// dedup cache then absorbs.
type acker interface {
	Ack(req socketmode.Request, payload ...any) error
}

// Receiver consumes the Socket Mode event stream, acks each envelope, drops
// duplicates and dispatches the work to a bounded pool.
type Receiver struct {
	events  <-chan socketmode.Event
	ack     acker
	handler Handler
	logger  *slog.Logger

	workers     int
	queueSize   int
	workTimeout time.Duration

	dedup *dedupCache
	now   func() time.Time
}

// Defaults for the receiver.
const (
	// DefaultWorkers bounds how many events are processed at once.
	DefaultWorkers = 8
	// DefaultQueueSize bounds how many events may wait for a worker before
	// load is shed.
	DefaultQueueSize = 64
	// DefaultWorkTimeout caps one unit of work - an answer to a follow-up may
	// involve an LLM call and telemetry queries, so it is generous, but it is
	// not unbounded.
	DefaultWorkTimeout = 5 * time.Minute
	// DefaultDedupTTL is how long a delivery id is remembered. Slack's
	// redelivery window is far shorter than this.
	DefaultDedupTTL = 10 * time.Minute
	// DefaultDedupCapacity bounds the memory the dedup cache may use.
	DefaultDedupCapacity = 4096
)

// ReceiverOption customises a Receiver.
type ReceiverOption func(*Receiver)

// WithReceiverLogger sets the logger.
func WithReceiverLogger(logger *slog.Logger) ReceiverOption {
	return func(r *Receiver) {
		if logger != nil {
			r.logger = logger
		}
	}
}

// WithWorkers sets the worker pool size and queue depth.
func WithWorkers(workers, queueSize int) ReceiverOption {
	return func(r *Receiver) {
		if workers > 0 {
			r.workers = workers
		}
		if queueSize >= 0 {
			r.queueSize = queueSize
		}
	}
}

// WithWorkTimeout caps how long one dispatched unit of work may run.
func WithWorkTimeout(timeout time.Duration) ReceiverOption {
	return func(r *Receiver) {
		if timeout > 0 {
			r.workTimeout = timeout
		}
	}
}

// withReceiverClock injects a clock for tests.
func withReceiverClock(now func() time.Time) ReceiverOption {
	return func(r *Receiver) {
		if now != nil {
			r.now = now
			r.dedup.now = now
		}
	}
}

// NewReceiver builds a receiver over an existing Socket Mode event stream.
//
// The stream and the acker are injected rather than constructed here, so the
// receiver can be driven by a real *socketmode.Client in production and by a
// plain channel in tests.
func NewReceiver(
	events <-chan socketmode.Event, ack acker, handler Handler, opts ...ReceiverOption,
) (*Receiver, error) {
	if events == nil {
		return nil, fmt.Errorf("slack: socket event stream is required")
	}
	if ack == nil {
		return nil, fmt.Errorf("slack: acker is required")
	}
	if handler == nil {
		return nil, fmt.Errorf("slack: handler is required")
	}

	receiver := &Receiver{
		events:      events,
		ack:         ack,
		handler:     handler,
		logger:      slog.Default(),
		workers:     DefaultWorkers,
		queueSize:   DefaultQueueSize,
		workTimeout: DefaultWorkTimeout,
		dedup:       newDedupCache(DefaultDedupTTL, DefaultDedupCapacity, time.Now),
		now:         time.Now,
	}
	for _, opt := range opts {
		opt(receiver)
	}
	return receiver, nil
}

// Run consumes events until ctx is cancelled or the stream closes, then drains
// the work already accepted and returns.
//
// Draining rather than aborting matters: work in flight has already been acked
// to Slack, so abandoning it would silently lose a human's message.
func (r *Receiver) Run(ctx context.Context) error {
	jobs := make(chan job, r.queueSize)

	// Work must outlive the request that scheduled it and the shutdown of the
	// loop, so it hangs off a context that is deliberately detached from ctx
	// and bounded by its own timeout instead.
	workCtx := context.WithoutCancel(ctx)

	var workers sync.WaitGroup
	workers.Add(r.workers)
	for i := 0; i < r.workers; i++ {
		go func() {
			defer workers.Done()
			for next := range jobs {
				r.run(workCtx, next)
			}
		}()
	}

	defer func() {
		close(jobs)
		workers.Wait()
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, open := <-r.events:
			if !open {
				return nil
			}
			r.handle(event, jobs)
		}
	}
}

type job struct {
	kind        string
	id          string
	message     *Message
	interaction *Interaction
	command     *Command
}

// handle acks the envelope and schedules the work. Acking first is not
// optional: Slack redelivers anything unacked after three seconds, and a
// redelivered envelope is a duplicated human turn.
func (r *Receiver) handle(event socketmode.Event, jobs chan<- job) {
	switch event.Type {
	case socketmode.EventTypeConnecting:
		r.logger.Info("slack socket connecting")
		return
	case socketmode.EventTypeConnected:
		r.logger.Info("slack socket connected")
		return
	case socketmode.EventTypeDisconnect:
		r.logger.Warn("slack socket disconnected, the client will reconnect")
		return
	case socketmode.EventTypeInvalidAuth:
		// Nothing will work until a human fixes the token; say so loudly.
		r.logger.Error("slack socket rejected the app-level token")
		return
	case socketmode.EventTypeConnectionError, socketmode.EventTypeIncomingError:
		r.logger.Warn("slack socket error", "detail", fmt.Sprint(event.Data))
		return
	}

	if event.Request == nil {
		return
	}

	scheduled, ok := r.decode(event)
	if !ok {
		// Still ack: an envelope we choose not to act on is handled, and
		// leaving it unacked only invites Slack to send it again.
		r.acknowledge(*event.Request, string(event.Type))
		return
	}

	r.acknowledge(*event.Request, scheduled.kind)

	if event.Request.RetryAttempt > 0 {
		r.logger.Warn("slack redelivered an envelope",
			"kind", scheduled.kind,
			"attempt", event.Request.RetryAttempt,
			"reason", event.Request.RetryReason,
		)
	}
	if !r.dedup.add(scheduled.id) {
		r.logger.Info("dropped duplicate slack delivery", "kind", scheduled.kind, "id", scheduled.id)
		return
	}

	select {
	case jobs <- scheduled:
	default:
		// Shedding is the honest response to overload: queueing without bound
		// would trade a visible drop for an invisible backlog and unbounded
		// cost.
		r.logger.Error("slack work queue full, dropped an event",
			"kind", scheduled.kind, "id", scheduled.id, "queue_size", r.queueSize,
		)
	}
}

// acknowledge tells Slack the envelope was received. A failed ack is logged
// rather than propagated: the envelope will simply be redelivered, and the
// dedup cache stops that turning into duplicated work.
func (r *Receiver) acknowledge(request socketmode.Request, kind string) {
	if err := r.ack.Ack(request); err != nil {
		r.logger.Warn("could not acknowledge a slack envelope",
			"kind", kind, "envelope_id", request.EnvelopeID, "error", err.Error())
	}
}

func (r *Receiver) decode(event socketmode.Event) (job, bool) {
	switch event.Type {
	case socketmode.EventTypeEventsAPI:
		apiEvent, ok := event.Data.(slackevents.EventsAPIEvent)
		if !ok {
			return job{}, false
		}
		msg, ok := messageFrom(apiEvent, eventID(apiEvent, event.Request))
		if !ok {
			return job{}, false
		}
		if msg.FromBot {
			// Acting on the sidekick's own posts would make it answer itself
			// forever.
			return job{}, false
		}
		if msg.UserID == "" || msg.Text == "" {
			return job{}, false
		}
		return job{kind: "message", id: msg.EventID, message: &msg}, true

	case socketmode.EventTypeInteractive:
		callback, ok := event.Data.(slack.InteractionCallback)
		if !ok {
			return job{}, false
		}
		interaction, ok := interactionFrom(callback, event.Request.EnvelopeID)
		if !ok {
			return job{}, false
		}
		return job{
			kind:        "interaction",
			id:          dedupID(interaction.EnvelopeID, interaction.ActionID+interaction.MessageTS+interaction.UserID),
			interaction: &interaction,
		}, true

	case socketmode.EventTypeSlashCommand:
		cmd, ok := event.Data.(slack.SlashCommand)
		if !ok {
			return job{}, false
		}
		command := commandFrom(cmd, event.Request.EnvelopeID)
		return job{
			kind:    "command",
			id:      dedupID(command.EnvelopeID, command.TriggerID),
			command: &command,
		}, true
	}

	return job{}, false
}

// run executes one unit of work with its own timeout, and contains any panic
// so one malformed event cannot take down the receiver.
func (r *Receiver) run(base context.Context, next job) {
	ctx, cancel := context.WithTimeout(base, r.workTimeout)
	defer cancel()

	defer func() {
		if recovered := recover(); recovered != nil {
			r.logger.Error("slack handler panicked",
				"kind", next.kind,
				"id", next.id,
				"panic", fmt.Sprint(recovered),
				"stack", string(debug.Stack()),
			)
		}
	}()

	switch {
	case next.message != nil:
		r.handler.OnMessage(ctx, *next.message)
	case next.interaction != nil:
		r.handler.OnInteraction(ctx, *next.interaction)
	case next.command != nil:
		r.handler.OnCommand(ctx, *next.command)
	}
}

// eventID prefers Slack's own event_id, which survives a redelivery, and falls
// back to the envelope id. The event id is carried on the callback wrapper
// rather than on the decoded event, so it has to be dug out of Data.
func eventID(event slackevents.EventsAPIEvent, request *socketmode.Request) string {
	if callback, ok := event.Data.(slackevents.EventsAPICallbackEvent); ok && callback.EventID != "" {
		return callback.EventID
	}
	if callback, ok := event.Data.(*slackevents.EventsAPICallbackEvent); ok && callback != nil && callback.EventID != "" {
		return callback.EventID
	}
	if request != nil {
		return request.EnvelopeID
	}
	return ""
}

// dedupID prefers Slack's delivery id and falls back to a composite of the
// payload's own identifiers, so an envelope without an id is still deduped
// rather than silently processed twice.
func dedupID(primary, fallback string) string {
	if primary != "" {
		return primary
	}
	return fallback
}

// dedupCache remembers recently seen delivery ids.
type dedupCache struct {
	mu       sync.Mutex
	seen     map[string]time.Time
	ttl      time.Duration
	capacity int
	now      func() time.Time
}

func newDedupCache(ttl time.Duration, capacity int, now func() time.Time) *dedupCache {
	return &dedupCache{
		seen:     make(map[string]time.Time),
		ttl:      ttl,
		capacity: capacity,
		now:      now,
	}
}

// add records an id and reports whether it is new. An empty id cannot be
// deduplicated, so it is always treated as new rather than collapsing every
// unidentified delivery into one.
func (d *dedupCache) add(id string) bool {
	if id == "" {
		return true
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	now := d.now()
	if seenAt, ok := d.seen[id]; ok && now.Sub(seenAt) < d.ttl {
		return false
	}

	if len(d.seen) >= d.capacity {
		d.evictLocked(now)
	}
	d.seen[id] = now
	return true
}

// evictLocked drops expired entries, and if that is not enough, drops the
// oldest ones. Losing dedup history is preferable to unbounded memory.
func (d *dedupCache) evictLocked(now time.Time) {
	for id, seenAt := range d.seen {
		if now.Sub(seenAt) >= d.ttl {
			delete(d.seen, id)
		}
	}
	if len(d.seen) < d.capacity {
		return
	}
	oldestID, oldestAt := "", time.Time{}
	for id, seenAt := range d.seen {
		if oldestID == "" || seenAt.Before(oldestAt) {
			oldestID, oldestAt = id, seenAt
		}
	}
	delete(d.seen, oldestID)
}
