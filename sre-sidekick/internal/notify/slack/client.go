package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"runtime/debug"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/config"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
)

// Client posts grounded diagnoses to a Slack channel. It implements
// notify.Notifier (PRD section 14).
//
// Failure policy (PRD section 25, session design edge case E17): a Slack
// outage must never take down the deterministic engine. This client therefore
// retries transient failures, logs every outcome with the correlation id for
// audit (PRD section 20), and then *returns* the error rather than hiding it.
// Returning it keeps the interface honest - an undelivered diagnosis is a real
// event on-call needs to know about - and the loop's call site is responsible
// for treating that error as non-fatal.
type Client struct {
	api     poster
	channel string
	logger  *slog.Logger
	retry   RetryPolicy

	// sleep and now are injectable so retry timing is testable without real
	// delays.
	sleep  func(ctx context.Context, d time.Duration) error
	now    func() time.Time
	jitter func(d time.Duration) time.Duration
}

// poster is the slice of the Slack API this adapter needs. *slack.Client
// satisfies it, and so does a fake in tests, which is what lets the whole
// message contract be verified without a Slack workspace (roadmap phase 6).
type poster interface {
	PostMessageContext(
		ctx context.Context, channelID string, options ...slack.MsgOption,
	) (string, string, error)

	// UpdateMessageContext rewrites an already-posted message, used to retire
	// the decision buttons once a decision has been made.
	UpdateMessageContext(
		ctx context.Context, channelID, timestamp string, options ...slack.MsgOption,
	) (string, string, string, error)

	// GetConversationRepliesContext reads a thread, used only to check whether
	// an unknown thread was one of ours before speaking in it.
	GetConversationRepliesContext(
		ctx context.Context, params *slack.GetConversationRepliesParameters,
	) ([]slack.Message, bool, string, error)
}

var _ notify.Notifier = (*Client)(nil)

// RetryPolicy bounds how hard the client tries before giving up. The point of
// retrying at all is that Slack rate limits and 5xx blips are routine, and a
// diagnosis that is silently never delivered is the worst failure mode the
// adapter has. The point of bounding it is that an unbounded retry is itself
// an outage.
type RetryPolicy struct {
	// MaxAttempts includes the first attempt.
	MaxAttempts int
	// InitialBackoff is the wait before the second attempt; it doubles from
	// there.
	InitialBackoff time.Duration
	// MaxBackoff caps a single wait.
	MaxBackoff time.Duration
	// TotalBudget caps the wall-clock time spent across all attempts and
	// waits. Slack sometimes asks for a 60s rate-limit pause; stalling the
	// diagnose loop that long is worse than reporting a failure.
	TotalBudget time.Duration
}

// DefaultRetryPolicy rides out a routine blip without letting a Slack outage
// stall the loop.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:    4,
		InitialBackoff: 250 * time.Millisecond,
		MaxBackoff:     2 * time.Second,
		TotalBudget:    10 * time.Second,
	}
}

// PostRef identifies a message that was posted. Timestamp is the Slack message
// ts of the root message, which becomes the session key (thread_ts) once the
// interactive layer lands: one incident, one thread, one session.
type PostRef struct {
	Channel   string
	Timestamp string
}

// Option customises a Client.
type Option func(*Client)

// WithLogger sets the logger used for the per-message audit records.
func WithLogger(logger *slog.Logger) Option {
	return func(c *Client) {
		if logger != nil {
			c.logger = logger
		}
	}
}

// WithRetryPolicy overrides the default retry bounds.
func WithRetryPolicy(policy RetryPolicy) Option {
	return func(c *Client) { c.retry = policy }
}

// WithChannel overrides the channel from config.
func WithChannel(channel string) Option {
	return func(c *Client) {
		if strings.TrimSpace(channel) != "" {
			c.channel = channel
		}
	}
}

// WithPoster injects the Slack transport. Tests use it to supply a fake; New
// otherwise builds a real *slack.Client.
func WithPoster(p poster) Option {
	return func(c *Client) {
		if p != nil {
			c.api = p
		}
	}
}

// withClock injects time control for tests.
func withClock(now func() time.Time, sleep func(context.Context, time.Duration) error) Option {
	return func(c *Client) {
		if now != nil {
			c.now = now
		}
		if sleep != nil {
			c.sleep = sleep
		}
	}
}

// withoutJitter makes backoff deterministic for tests.
func withoutJitter() Option {
	return func(c *Client) { c.jitter = func(d time.Duration) time.Duration { return d } }
}

// New builds a Slack client from the adapter config. The bot token is read
// from the environment variable named in the config; it is never held in the
// config struct and never logged.
func New(cfg config.SlackConfig, opts ...Option) (*Client, error) {
	channel := strings.TrimSpace(cfg.DefaultChannel)
	if channel == "" {
		return nil, errors.New("slack: default_channel is required")
	}

	client := &Client{
		channel: channel,
		logger:  slog.Default(),
		retry:   DefaultRetryPolicy(),
		sleep:   sleepContext,
		now:     time.Now,
		jitter:  defaultJitter,
	}
	for _, opt := range opts {
		opt(client)
	}

	if client.api == nil {
		token, err := cfg.BotToken()
		if err != nil {
			return nil, fmt.Errorf("slack: %w", err)
		}
		client.api = slack.New(token)
	}
	if err := client.retry.validate(); err != nil {
		return nil, fmt.Errorf("slack: %w", err)
	}
	return client, nil
}

func (p RetryPolicy) validate() error {
	if p.MaxAttempts < 1 {
		return fmt.Errorf("retry policy: max attempts must be >= 1, got %d", p.MaxAttempts)
	}
	if p.InitialBackoff < 0 || p.MaxBackoff < 0 || p.TotalBudget < 0 {
		return errors.New("retry policy: durations must not be negative")
	}
	return nil
}

// NotifyDiagnosis renders and posts a completed diagnosis.
func (c *Client) NotifyDiagnosis(ctx context.Context, d notify.Diagnosis) error {
	_, err := c.PostDiagnosis(ctx, d)
	return err
}

// PostDiagnosis is NotifyDiagnosis with the posted message reference returned,
// so the session layer can key a session on the thread root.
func (c *Client) PostDiagnosis(ctx context.Context, d notify.Diagnosis) (ref PostRef, err error) {
	defer func() {
		if err = recovered(recover(), err, c.logger, d.CorrelationID); err != nil {
			ref = PostRef{}
		}
	}()

	return c.post(ctx, message{
		correlationID: d.CorrelationID,
		kind:          string(statusOr(d.Status, notify.StatusDiagnosed)),
		service:       d.Service,
		fallback:      diagnosisFallback(d),
		blocks:        DiagnosisBlocks(d),
	})
}

// NotifyIndeterminate posts the "telemetry not trusted, no diagnosis
// attempted" message (PRD section 9.3).
func (c *Client) NotifyIndeterminate(ctx context.Context, r notify.IndeterminateReason) error {
	_, err := c.PostIndeterminate(ctx, r)
	return err
}

// PostIndeterminate is NotifyIndeterminate with the message reference
// returned.
func (c *Client) PostIndeterminate(
	ctx context.Context, r notify.IndeterminateReason,
) (ref PostRef, err error) {
	defer func() {
		if err = recovered(recover(), err, c.logger, r.CorrelationID); err != nil {
			ref = PostRef{}
		}
	}()

	return c.post(ctx, message{
		correlationID: r.CorrelationID,
		kind:          string(notify.StatusIndeterminate),
		service:       r.Service,
		fallback:      indeterminateFallback(r),
		blocks:        IndeterminateBlocks(r),
	})
}

// PostThreadReply posts a plain-text reply inside an existing thread, which is
// how the sidekick answers a follow-up without starting a new session.
func (c *Client) PostThreadReply(
	ctx context.Context, channelID, threadTS, text string,
) (PostRef, error) {
	return c.post(ctx, message{
		kind:     "thread_reply",
		channel:  channelID,
		threadTS: threadTS,
		fallback: text,
	})
}

// UpdateMessage rewrites an existing message. It is used to replace the
// approve/decline buttons with the recorded outcome, so a decided incident
// never shows a live-looking button that silently does nothing.
//
// Updates are retried on the same terms as posts, but a failure here is not
// fatal to the decision: the decision is already recorded in the session and
// the audit log, and the thread reply states it too.
func (c *Client) UpdateMessage(
	ctx context.Context, channelID, timestamp, fallback string, blocks []slack.Block,
) error {
	_, err := c.post(ctx, message{
		kind:      "update",
		channel:   channelID,
		updateTS:  timestamp,
		fallback:  fallback,
		blocks:    blocks,
		isUpdate:  true,
		suppressN: true,
	})
	return err
}

// ThreadRootPostedByUs reports whether the root message of a thread was posted
// by this bot.
//
// It exists for one narrow case: a human replies in a thread the sidekick has
// no session for, typically after a restart. Answering every unknown thread
// would make the bot speak in unrelated conversations; staying silent leaves a
// user talking to a wall. Checking the root lets it answer only where it is
// actually a participant.
func (c *Client) ThreadRootPostedByUs(ctx context.Context, channelID, threadTS string) (bool, error) {
	messages, _, _, err := c.api.GetConversationRepliesContext(
		ctx,
		&slack.GetConversationRepliesParameters{
			ChannelID: channelID,
			Timestamp: threadTS,
			Limit:     1,
			Inclusive: true,
		},
	)
	if err != nil {
		return false, fmt.Errorf("slack: read thread %s/%s: %w", channelID, threadTS, err)
	}
	if len(messages) == 0 {
		return false, nil
	}
	root := messages[0]
	if strings.TrimSpace(root.BotID) == "" {
		return false, nil
	}
	// The sidekick only ever roots a thread with its own diagnosis message, so
	// a bot-authored root in a channel it posts to is ours.
	return true, nil
}

// Channel reports the channel diagnoses are posted to.
func (c *Client) Channel() string { return c.channel }

type message struct {
	correlationID string
	kind          string
	service       string
	fallback      string
	blocks        []slack.Block

	// channel overrides the configured default channel.
	channel string
	// threadTS posts the message as a reply inside a thread.
	threadTS string
	// updateTS names the message to rewrite instead of posting a new one.
	updateTS string
	isUpdate bool
	// suppressN keeps routine updates out of the info log.
	suppressN bool
}

// post sends one message, retrying transient failures within the policy
// budget, and logs the outcome for audit either way.
func (c *Client) post(ctx context.Context, msg message) (PostRef, error) {
	started := c.now()
	var lastErr error

	for attempt := 1; attempt <= c.retry.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return PostRef{}, c.fail(msg, attempt, err)
		}

		channel, timestamp, err := c.send(ctx, msg)
		if err == nil {
			if msg.suppressN {
				return PostRef{Channel: channel, Timestamp: timestamp}, nil
			}
			c.logger.Info("slack message posted",
				"correlation_id", msg.correlationID,
				"kind", msg.kind,
				"service", msg.service,
				"channel", channel,
				"message_ts", timestamp,
				"attempts", attempt,
			)
			return PostRef{Channel: channel, Timestamp: timestamp}, nil
		}
		lastErr = err

		if !retryable(err) {
			return PostRef{}, c.fail(msg, attempt, err)
		}
		if attempt == c.retry.MaxAttempts {
			break
		}

		wait := c.backoff(attempt, err)
		// Giving up early beats stalling the diagnose loop past its budget.
		if elapsed := c.now().Sub(started); elapsed+wait > c.retry.TotalBudget {
			return PostRef{}, c.fail(msg, attempt, fmt.Errorf(
				"retry budget %s exhausted: %w", c.retry.TotalBudget, err,
			))
		}

		c.logger.Warn("slack post failed, retrying",
			"correlation_id", msg.correlationID,
			"kind", msg.kind,
			"attempt", attempt,
			"retry_in", wait.String(),
			"error", err.Error(),
		)
		if sleepErr := c.sleep(ctx, wait); sleepErr != nil {
			return PostRef{}, c.fail(msg, attempt, sleepErr)
		}
	}

	return PostRef{}, c.fail(msg, c.retry.MaxAttempts, lastErr)
}

// send performs one attempt: a post, a threaded reply, or an update.
func (c *Client) send(ctx context.Context, msg message) (string, string, error) {
	channel := msg.channel
	if strings.TrimSpace(channel) == "" {
		channel = c.channel
	}

	// The fallback text is what push notifications and screen readers show; a
	// Block Kit message without it arrives blank on mobile.
	options := []slack.MsgOption{slack.MsgOptionText(msg.fallback, false)}
	if len(msg.blocks) > 0 {
		options = append(options, slack.MsgOptionBlocks(msg.blocks...))
	}
	if msg.threadTS != "" {
		options = append(options, slack.MsgOptionTS(msg.threadTS))
	}

	if msg.isUpdate {
		updatedChannel, timestamp, _, err := c.api.UpdateMessageContext(
			ctx, channel, msg.updateTS, options...,
		)
		return updatedChannel, timestamp, err
	}
	return c.api.PostMessageContext(ctx, channel, options...)
}

// fail logs the undelivered message and wraps the error. The log record exists
// so the failure is in the audit trail regardless of what the caller does with
// the returned error (PRD section 20).
func (c *Client) fail(msg message, attempts int, err error) error {
	channel := msg.channel
	if strings.TrimSpace(channel) == "" {
		channel = c.channel
	}
	c.logger.Error("slack message not delivered",
		"correlation_id", msg.correlationID,
		"kind", msg.kind,
		"service", msg.service,
		"channel", channel,
		"attempts", attempts,
		"error", err.Error(),
	)
	return fmt.Errorf("slack: post %s for %q: %w", msg.kind, msg.service, err)
}

// backoff returns how long to wait before the next attempt. When Slack states
// a rate-limit delay we honour it exactly rather than guessing.
func (c *Client) backoff(attempt int, err error) time.Duration {
	var rateLimited *slack.RateLimitedError
	if errors.As(err, &rateLimited) && rateLimited.RetryAfter > 0 {
		return rateLimited.RetryAfter
	}

	wait := c.retry.InitialBackoff << (attempt - 1)
	if c.retry.MaxBackoff > 0 && wait > c.retry.MaxBackoff {
		wait = c.retry.MaxBackoff
	}
	return c.jitter(wait)
}

// retryable reports whether another attempt could plausibly succeed. Retrying
// a permanent error (bad token, missing channel, malformed blocks) only wastes
// the budget and delays the failure report.
func retryable(err error) bool {
	if err == nil {
		return false
	}
	// A cancelled or expired context is the caller's decision; respect it.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var rateLimited *slack.RateLimitedError
	if errors.As(err, &rateLimited) {
		return true
	}

	var statusErr slack.StatusCodeError
	if errors.As(err, &statusErr) {
		return statusErr.Code == 429 || statusErr.Code >= 500
	}

	if permanentSlackError(err.Error()) {
		return false
	}

	// Anything left is most likely a network fault, which is worth retrying.
	return true
}

// permanentSlackErrors are Slack API error strings that will not change by
// trying again: they need a human to fix configuration or code.
var permanentSlackErrors = []string{
	"invalid_auth",
	"not_authed",
	"account_inactive",
	"token_revoked",
	"token_expired",
	"no_permission",
	"missing_scope",
	"channel_not_found",
	"not_in_channel",
	"is_archived",
	"msg_too_long",
	"invalid_blocks",
	"invalid_blocks_format",
	"restricted_action",
	"team_access_not_granted",
}

func permanentSlackError(message string) bool {
	message = strings.ToLower(message)
	for _, candidate := range permanentSlackErrors {
		if strings.Contains(message, candidate) {
			return true
		}
	}
	return false
}

// recovered converts a panic in the rendering or transport path into an error.
// A malformed diagnosis must not be able to crash the process that is trying
// to report an incident (session design edge case E17).
func recovered(panicValue any, err error, logger *slog.Logger, correlationID string) error {
	if panicValue == nil {
		return err
	}
	logger.Error("slack adapter panicked",
		"correlation_id", correlationID,
		"panic", fmt.Sprint(panicValue),
		"stack", string(debug.Stack()),
	)
	return fmt.Errorf("slack: panic while posting: %v", panicValue)
}

// diagnosisFallback is the plain-text summary shown in push notifications and
// by screen readers. It carries the deterministic facts only.
func diagnosisFallback(d notify.Diagnosis) string {
	service := fallback(d.Service, "unknown service")
	if d.Status == notify.StatusIndeterminate {
		return fmt.Sprintf("Indeterminate: %s - telemetry incomplete, no diagnosis", service)
	}
	return fmt.Sprintf(
		"Diagnosis: %s - %s %s, burn rate %s, error budget %s",
		service,
		fallback(d.Grounding.SLO, "unknown SLO"),
		fallback(d.Grounding.SLOState, "state unknown"),
		formatBurnRate(d.Grounding.BurnRate),
		formatPercent(d.Grounding.ErrorBudgetRemaining),
	)
}

func indeterminateFallback(r notify.IndeterminateReason) string {
	return fmt.Sprintf(
		"Indeterminate: %s - telemetry incomplete, cannot diagnose reliably",
		fallback(r.Service, "unknown service"),
	)
}

func statusOr(status, def notify.Status) notify.Status {
	if strings.TrimSpace(string(status)) == "" {
		return def
	}
	return status
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// defaultJitter spreads retries by +/-20% so that an alert storm's retries do
// not all fire in lockstep and re-spike the API they are waiting on.
func defaultJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	spread := float64(d) * 0.2
	return time.Duration(float64(d) - spread + rand.Float64()*2*spread)
}
