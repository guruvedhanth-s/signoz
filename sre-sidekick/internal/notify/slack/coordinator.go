package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/session"
)

// Coordinator is the conversation logic: it turns a diagnosis into a thread,
// routes every inbound event back to that thread's session, and records
// decisions.
//
// It implements Handler, so the Socket Mode receiver hands it work directly.
//
// Two boundaries it never crosses:
//
//   - It does not reason. Answers come from the RCA interface; this type only
//     assembles context and posts text.
//   - It does not act. An approval is recorded with who approved it and when,
//     and the human performs the fix by hand (PRD sections 5.6, 15).
type Coordinator struct {
	client     *Client
	sessions   *session.Manager
	rca        RCA
	actuator   Actuator
	authorizer Authorizer
	auditor    session.Auditor
	logger     *slog.Logger
	metrics    *Metrics

	defaultEnvironment string

	// analysis bounds concurrent RCA runs. Button clicks and lookups are
	// cheap and are deliberately not gated by it; the analysis is what costs
	// money and time (session design edge case E8).
	analysis chan struct{}

	// notified remembers which closed threads have already been told they are
	// closed, so a stale thread gets one reply rather than one per message
	// (edge case E7).
	mu       sync.Mutex
	notified map[string]bool
}

// CoordinatorOption customises a Coordinator.
type CoordinatorOption func(*Coordinator)

// WithCoordinatorLogger sets the logger.
func WithCoordinatorLogger(logger *slog.Logger) CoordinatorOption {
	return func(c *Coordinator) {
		if logger != nil {
			c.logger = logger
		}
	}
}

// WithCoordinatorMetrics reports incidents, decisions and follow-ups to
// OpenTelemetry, so the loop appears on a SigNoz dashboard (PRD section 17).
func WithCoordinatorMetrics(metrics *Metrics) CoordinatorOption {
	return func(c *Coordinator) { c.metrics = metrics }
}

// WithCoordinatorActuator attaches the Act-stage adapter (PRD sections 15,
// 18). Until attached, an approval is recorded but nothing is logged for
// audit beyond the decision itself (NoopActuator).
func WithCoordinatorActuator(actuator Actuator) CoordinatorOption {
	return func(c *Coordinator) {
		if actuator != nil {
			c.actuator = actuator
		}
	}
}

// WithCoordinatorAuthorizer attaches the click-time authorization policy.
func WithCoordinatorAuthorizer(authorizer Authorizer) CoordinatorOption {
	return func(c *Coordinator) {
		if authorizer != nil {
			c.authorizer = authorizer
		}
	}
}

// WithCoordinatorAuditor records durable lifecycle events without coupling
// the Slack coordinator to a database implementation.
func WithCoordinatorAuditor(auditor session.Auditor) CoordinatorOption {
	return func(c *Coordinator) { c.auditor = auditor }
}

// WithMaxConcurrentAnalysis caps concurrent RCA runs.
func WithMaxConcurrentAnalysis(limit int) CoordinatorOption {
	return func(c *Coordinator) {
		if limit > 0 {
			c.analysis = make(chan struct{}, limit)
		}
	}
}

// WithDefaultEnvironment sets the environment assumed when a slash command
// does not name one.
func WithDefaultEnvironment(environment string) CoordinatorOption {
	return func(c *Coordinator) {
		if strings.TrimSpace(environment) != "" {
			c.defaultEnvironment = strings.TrimSpace(environment)
		}
	}
}

var _ Handler = (*Coordinator)(nil)

// NewCoordinator wires the poster, the session store and the analysis engine
// together.
func NewCoordinator(
	client *Client, sessions *session.Manager, rca RCA, opts ...CoordinatorOption,
) (*Coordinator, error) {
	if client == nil {
		return nil, errors.New("slack: client is required")
	}
	if sessions == nil {
		return nil, errors.New("slack: session manager is required")
	}
	if rca == nil {
		// An explicit stub is better than a nil check on every call path.
		rca = UnavailableRCA{}
	}

	coordinator := &Coordinator{
		client:             client,
		sessions:           sessions,
		rca:                rca,
		actuator:           NoopActuator{},
		authorizer:         PermissiveAuthorizer{},
		logger:             slog.Default(),
		defaultEnvironment: "prod",
		analysis:           make(chan struct{}, 5),
		notified:           make(map[string]bool),
	}
	for _, opt := range opts {
		opt(coordinator)
	}
	return coordinator, nil
}

// Announce posts a diagnosis and opens the session on the thread it creates.
// This is the Slack-side entry point for the alert-driven path: whoever
// detects the alert and produces the Diagnosis calls this and nothing else.
//
// When the incident already has a live thread, the existing session is reused:
// the thread is updated instead of a second one being opened, and the caller
// is told so it can skip a duplicate analysis (session design edge case E2).
func (c *Coordinator) Announce(ctx context.Context, d notify.Diagnosis) (*session.Session, error) {
	fingerprint := session.Fingerprint(d.Service, d.Environment, d.Grounding.SLO)

	if existing, ok := c.sessions.ByFingerprint(fingerprint); ok {
		c.sessions.Touch(existing)
		c.reply(ctx, existing, fmt.Sprintf(
			"This alert fired again at %s. Still tracking it in this thread.",
			time.Now().UTC().Format("15:04:05 MST"),
		))
		return existing, nil
	}

	ref, err := c.client.PostDiagnosis(ctx, d)
	if err != nil {
		return nil, err
	}

	opened, existing, err := c.sessions.Open(session.OpenRequest{
		ChannelID:   ref.Channel,
		ThreadTS:    ref.Timestamp,
		Diagnosis:   d,
		Fingerprint: fingerprint,
	})
	if err != nil {
		return nil, fmt.Errorf("slack: open session: %w", err)
	}
	if !existing {
		c.audit(session.AuditEvent{Type: "diagnosis_announced", CorrelationID: d.CorrelationID, Service: d.Service, Environment: d.Environment})
		c.metrics.IncidentAnnounced(ctx, d.Service, d.Environment, string(statusOr(d.Status, notify.StatusDiagnosed)))
		c.logger.Info("incident session opened",
			"correlation_id", d.CorrelationID,
			"service", d.Service,
			"channel", ref.Channel,
			"thread_ts", ref.Timestamp,
		)
	}
	return opened, nil
}

// OnInteraction handles a button click: approve, decline or close.
//
// Buttons are the only way a decision is recorded. A click carries a verified
// user id, the channel and the thread, so there is nothing to parse and
// nothing to misread - which is why free text is never treated as an approval.
func (c *Coordinator) OnInteraction(ctx context.Context, in Interaction) {
	found, ok := c.sessions.ByThread(in.ChannelID, in.ThreadTS)
	if !ok {
		// The button exists, so this thread was certainly ours; the session
		// is simply gone, most likely because the sidekick restarted.
		c.postReply(ctx, in.ChannelID, in.ThreadTS,
			"This session was lost, so the click was not recorded. Run `/diagnose` to start a new one.")
		return
	}

	switch in.ActionID {
	case ActionApprove:
		c.decide(ctx, found, in, session.DecisionApproved)
	case ActionDecline:
		c.decide(ctx, found, in, session.DecisionDeclined)
	case ActionClose:
		c.closeSession(ctx, found, in)
	case ActionVerify:
		c.verify(ctx, found, in)
	default:
		c.logger.Warn("unknown slack action", "action_id", in.ActionID, "user", in.UserID)
	}
}

func (c *Coordinator) decide(
	ctx context.Context, s *session.Session, in Interaction, kind session.DecisionKind,
) {
	authorization, ok := c.authorize(ctx, s, in, RoleApprover)
	if !ok {
		return
	}
	accepted, existing, err := c.sessions.Decide(s, session.Decision{
		Kind:              kind,
		UserID:            in.UserID,
		Authorization:     "allowed",
		AuthorizationRole: string(authorization.Role),
	})
	if err != nil {
		c.postReply(ctx, in.ChannelID, in.ThreadTS,
			"This session is already closed, so the decision was not recorded.")
		return
	}
	if !accepted {
		// Two people decided at once, or one person double-tapped. The first
		// decision stands (session design edge case E5).
		c.postReply(ctx, in.ChannelID, in.ThreadTS, fmt.Sprintf(
			"Already %s by <@%s> at %s.",
			existing.Kind, existing.UserID, existing.At.UTC().Format("15:04:05 MST"),
		))
		return
	}
	c.audit(session.AuditEvent{Type: "decision_recorded", CorrelationID: s.CorrelationID, Service: s.Service, Environment: s.Environment, Actor: in.UserID, Payload: fmt.Sprintf(`{"decision":%q}`, kind)})

	verb := "approved"
	detail := "Recorded as approved. Nothing was executed: apply the fix by hand, then verify."
	if kind == session.DecisionDeclined {
		verb = "declined"
		detail = "Recorded as declined. No action will be taken."
	}

	c.metrics.DecisionRecorded(ctx, s.Service, s.Environment, string(kind))
	c.logger.Info("decision recorded",
		"correlation_id", s.CorrelationID,
		"decision", string(kind),
		"user", in.UserID,
		"service", s.Service,
		"thread_ts", s.ThreadTS,
	)

	if kind == session.DecisionApproved {
		c.act(ctx, s)
	}

	notice := fmt.Sprintf("*%s by <@%s>* at %s", strings.ToUpper(verb[:1])+verb[1:], in.UserID,
		existing.At.UTC().Format("2006-01-02 15:04:05 MST"))
	if kind == session.DecisionApproved {
		// Only an approval has something worth going to verify - a decline
		// takes no action, so there is nothing to check recovery on.
		c.retireButtonsWithVerify(ctx, s, in, notice)
	} else {
		c.retireButtons(ctx, s, in, notice)
	}
	c.postReply(ctx, in.ChannelID, in.ThreadTS, detail)
}

// act hands an approved proposal to the Actuator (PRD sections 15, 18) and
// records the outcome. It runs automatically once approval is recorded -
// there is no separate button, because approving *is* the human-in-the-loop
// gate the Actuator is waiting for. A failure here is logged but never
// surfaces as a decision failure: the decision itself already succeeded.
func (c *Coordinator) act(ctx context.Context, s *session.Session) {
	d := s.Diagnosis()
	result, err := c.actuator.Act(ctx, ActionRequest{
		CorrelationID: d.CorrelationID,
		Service:       s.Service,
		Environment:   s.Environment,
		ProposedFix:   d.ProposedFix,
		Reversible:    d.Reversible,
	})
	if err != nil {
		c.logger.Error("actuator failed",
			"correlation_id", s.CorrelationID, "service", s.Service, "error", err.Error())
		return
	}
	c.metrics.ActionRecorded(ctx, s.Service, s.Environment, "advisory", result.Outcome)
}

// verify handles the "Verify recovery" button (PRD section 16): it
// re-evaluates the SLO the incident was grounded on and reports the
// current state in the thread. It never mutates the session's decision -
// unlike Approve/Decline, it can be clicked repeatedly while a human waits
// for a fix to take effect.
func (c *Coordinator) verify(ctx context.Context, s *session.Session, in Interaction) {
	if _, ok := c.authorize(ctx, s, in, RoleApprover); !ok {
		return
	}
	d := s.Diagnosis()
	if strings.TrimSpace(d.Grounding.SLO) == "" {
		c.postReply(ctx, in.ChannelID, in.ThreadTS, "This incident has no SLO to re-check.")
		return
	}

	result, err := c.rca.Verify(ctx, VerifyRequest{
		Service:     s.Service,
		Environment: s.Environment,
		SLO:         d.Grounding.SLO,
	})
	if err != nil {
		if errors.Is(err, ErrVerifierUnavailable) {
			c.postReply(ctx, in.ChannelID, in.ThreadTS,
				"The verify engine is not connected yet, so recovery cannot be checked automatically.")
			return
		}
		c.logger.Error("verify failed",
			"correlation_id", s.CorrelationID, "service", s.Service, "error", err.Error())
		c.postReply(ctx, in.ChannelID, in.ThreadTS, "The recovery check failed: "+err.Error())
		return
	}

	c.metrics.VerifyChecked(ctx, s.Service, s.Environment, result.SLOState)
	c.audit(session.AuditEvent{Type: "verification_completed", CorrelationID: s.CorrelationID, Service: s.Service, Environment: s.Environment, Actor: in.UserID})
	c.logger.Info("verify checked",
		"correlation_id", s.CorrelationID,
		"service", s.Service,
		"slo_state", result.SLOState,
		"burn_rate", result.BurnRate,
		"recovered", result.Recovered,
	)

	c.postReply(ctx, in.ChannelID, in.ThreadTS, verifyMessage(result))
}

// authorize is the security boundary for interactive actions. It runs before
// session mutation and before any RCA or Actuator call. The attempt is audited
// for both outcomes, so a denied click is never indistinguishable from a
// broken Slack interaction.
func (c *Coordinator) authorize(ctx context.Context, s *session.Session, in Interaction, required Role) (AuthorizationResult, bool) {
	result := c.authorizer.Authorize(ctx, AuthorizationRequest{
		UserID: in.UserID, ChannelID: in.ChannelID, ThreadTS: in.ThreadTS,
		ActionID: in.ActionID, Required: required,
	})
	payload := fmt.Sprintf(`{"action":%q,"required_role":%q,"allowed":%t,"matched_role":%q,"reason":%q}`, in.ActionID, required, result.Allowed, result.Role, result.Reason)
	c.audit(session.AuditEvent{
		Type: "authorization_attempt", CorrelationID: s.CorrelationID,
		Service: s.Service, Environment: s.Environment, Actor: in.UserID, Payload: payload,
	})
	c.logger.Info("slack authorization attempt", "correlation_id", s.CorrelationID,
		"action", in.ActionID, "user", in.UserID, "required_role", required,
		"allowed", result.Allowed, "matched_role", result.Role, "reason", result.Reason)
	if result.Allowed {
		return result, true
	}
	reason := result.Reason
	if reason == "" {
		reason = "the required role is not assigned to your Slack user"
	}
	c.logger.Warn("slack interaction denied", "correlation_id", s.CorrelationID, "action", in.ActionID, "user", in.UserID, "reason", reason)
	c.postReply(ctx, in.ChannelID, in.ThreadTS, fmt.Sprintf("You are not authorized to perform this action: %s. Ask an authorized approver or operator.", reason))
	return result, false
}

// verifyMessage renders a VerifyResult as plain text: the deterministic
// facts and numbers come straight from the SLO engine, so there is nothing
// here for an LLM to phrase (PRD sections 7, 16).
func verifyMessage(result VerifyResult) string {
	if !result.TelemetryTrusted {
		return fmt.Sprintf(
			"*Recovery check: indeterminate.* Telemetry is not trusted for this window, so recovery cannot be confirmed. SLO state: `%s`.",
			result.SLOState,
		)
	}
	if result.Recovered {
		return fmt.Sprintf(
			"*Recovery check: recovered.* SLO state `%s`, burn rate %.2fx, error budget remaining %.1f%%.",
			result.SLOState, result.BurnRate, result.ErrorBudgetRemaining*100,
		)
	}
	return fmt.Sprintf(
		"*Recovery check: not yet recovered.* SLO state `%s`, burn rate %.2fx, error budget remaining %.1f%%.",
		result.SLOState, result.BurnRate, result.ErrorBudgetRemaining*100,
	)
}

func (c *Coordinator) closeSession(ctx context.Context, s *session.Session, in Interaction) {
	if err := c.sessions.Close(s, session.ReasonClosedByUser); err != nil {
		c.postReply(ctx, in.ChannelID, in.ThreadTS, "This session was already closed.")
		return
	}
	c.metrics.DecisionRecorded(ctx, s.Service, s.Environment, "closed")
	c.audit(session.AuditEvent{Type: "session_closed", CorrelationID: s.CorrelationID, Service: s.Service, Environment: s.Environment, Actor: in.UserID})
	c.logger.Info("session closed",
		"correlation_id", s.CorrelationID, "user", in.UserID, "thread_ts", s.ThreadTS)

	c.retireButtons(ctx, s, in, fmt.Sprintf("*Closed by <@%s>* without a decision", in.UserID))
	c.postReply(ctx, in.ChannelID, in.ThreadTS,
		"Session closed. Note that closing the conversation does not silence a still-firing alert.")
}

// retireButtons rewrites the original message so the decision buttons are
// replaced by the outcome. A failure here is logged but never propagated: the
// decision is already recorded in the session and the audit log, and the
// thread reply states it too.
func (c *Coordinator) retireButtons(
	ctx context.Context, s *session.Session, in Interaction, notice string,
) {
	timestamp := in.MessageTS
	if strings.TrimSpace(timestamp) == "" {
		timestamp = s.ThreadTS
	}

	d := s.Diagnosis()
	blocks := ResolvedBlocks(DiagnosisBlocks(d), notice)
	if err := c.client.UpdateMessage(ctx, in.ChannelID, timestamp, diagnosisFallback(d), blocks); err != nil {
		c.logger.Warn("could not retire the decision buttons",
			"correlation_id", s.CorrelationID, "error", err.Error())
	}
}

// retireButtonsWithVerify is retireButtons for the approved outcome: the
// Approve/Decline buttons still retire into the notice, but a "Verify
// recovery" button is added so the human has a way to check the fix took
// effect once they have applied it (PRD section 16).
func (c *Coordinator) retireButtonsWithVerify(
	ctx context.Context, s *session.Session, in Interaction, notice string,
) {
	timestamp := in.MessageTS
	if strings.TrimSpace(timestamp) == "" {
		timestamp = s.ThreadTS
	}

	d := s.Diagnosis()
	blocks := ResolvedBlocksWithVerify(DiagnosisBlocks(d), notice, d.CorrelationID)
	if err := c.client.UpdateMessage(ctx, in.ChannelID, timestamp, diagnosisFallback(d), blocks); err != nil {
		c.logger.Warn("could not retire the decision buttons",
			"correlation_id", s.CorrelationID, "error", err.Error())
	}
}

// OnMessage handles a human message in a thread.
//
// Only threaded replies are considered: a top-level message in the channel
// belongs to no incident, and answering it would make the bot barge into
// unrelated conversation.
func (c *Coordinator) OnMessage(ctx context.Context, msg Message) {
	if !msg.InThread() {
		return
	}

	found, ok := c.sessions.ByThread(msg.ChannelID, msg.ThreadTS)
	if !ok {
		c.handleUnknownThread(ctx, msg)
		return
	}

	if found.Status().Terminal() {
		c.noticeClosedOnce(ctx, found, msg)
		return
	}

	// One turn at a time per thread, held across the slow analysis call, so
	// two people typing at once cannot interleave their turns (session design
	// edge cases E5, E15).
	found.BeginTurn()
	defer found.EndTurn()

	// Re-check: the session may have been decided or reaped while waiting.
	if found.Status().Terminal() {
		c.noticeClosedOnce(ctx, found, msg)
		return
	}

	// Build the request before the question is appended: History is the
	// conversation *so far* and Question is the new turn, so carrying the same
	// text in both would waste context and read as if it had been asked twice.
	request := followupRequest(found, msg.Text, msg.UserID)

	if err := c.sessions.AppendTurn(found, session.Turn{
		Actor: session.Actor(msg.UserID),
		Text:  msg.Text,
		At:    time.Now().UTC(),
	}); err != nil {
		c.noticeClosedOnce(ctx, found, msg)
		return
	}
	c.audit(session.AuditEvent{Type: "followup_requested", CorrelationID: found.CorrelationID, Service: found.Service, Environment: found.Environment, Actor: msg.UserID})

	// Free text is never a decision. When someone types "approve" we say where
	// the button is rather than recording an approval from parsed prose: a
	// misread "sure, but..." must never count as go (session design section 7).
	if isTerminalWord(msg.Text) {
		c.reply(ctx, found, "To decide, use the buttons on the message above: *Approve*, *Decline* or *Close session*.")
		return
	}

	answer, err := c.answer(ctx, request)
	if err != nil {
		c.metrics.FollowupRecorded(ctx, found.Service, found.Environment, followupOutcome(err))
		if errors.Is(err, ErrRCAUnavailable) {
			c.reply(ctx, found,
				"The analysis engine is not connected, so I cannot answer follow-up questions yet. "+
					"The facts in the message above still stand: they come from the SLO engine, not from a model.")
			return
		}
		if errors.Is(err, errAnalysisBusy) {
			c.reply(ctx, found,
				"Too many analyses are running right now. Ask again in a moment.")
			return
		}
		c.logger.Error("follow-up failed",
			"correlation_id", found.CorrelationID, "error", err.Error())
		c.reply(ctx, found, "I could not answer that: "+err.Error())
		return
	}

	c.metrics.FollowupRecorded(ctx, found.Service, found.Environment, FollowupAnswered)
	if err := c.sessions.AppendTurn(found, session.Turn{
		Actor: session.ActorSidekick,
		Text:  answer,
		At:    time.Now().UTC(),
	}); err != nil {
		c.logger.Warn("could not record the answer",
			"correlation_id", found.CorrelationID, "error", err.Error())
	}
	c.reply(ctx, found, answer)
}

func (c *Coordinator) answer(ctx context.Context, request FollowupRequest) (string, error) {
	release, err := c.acquireAnalysis(ctx)
	if err != nil {
		return "", err
	}
	defer release()

	return c.rca.AnswerFollowup(ctx, request)
}

// handleUnknownThread deals with a reply in a thread that has no session.
//
// The bot answers only if it posted the thread root. Replying to every unknown
// thread would make it speak in conversations it was never part of; staying
// silent everywhere would leave a user talking to a wall after a restart
// (session design edge case E3). On any lookup error it stays silent, because
// speaking wrongly is worse than saying nothing.
func (c *Coordinator) handleUnknownThread(ctx context.Context, msg Message) {
	ours, err := c.client.ThreadRootPostedByUs(ctx, msg.ChannelID, msg.ThreadTS)
	if err != nil {
		c.logger.Warn("could not check thread ownership",
			"channel", msg.ChannelID, "thread_ts", msg.ThreadTS, "error", err.Error())
		return
	}
	if !ours {
		return
	}
	if !c.markNotified(msg.ChannelID, msg.ThreadTS) {
		return
	}
	c.postReply(ctx, msg.ChannelID, msg.ThreadTS,
		"This session is no longer active, so I have lost its context. Run `/diagnose` to start a new one.")
}

// noticeClosedOnce answers a message in a closed thread exactly once, rather
// than repeating on every message or dropping it silently.
func (c *Coordinator) noticeClosedOnce(ctx context.Context, s *session.Session, msg Message) {
	if !c.markNotified(msg.ChannelID, msg.ThreadTS) {
		return
	}
	reason := s.CloseReason()
	text := "This session is closed, so I am no longer tracking it. Run `/diagnose` to start a new one."
	if reason == session.ReasonIdle {
		text = "This session expired after a period of inactivity. Run `/diagnose` to start a new one."
	}
	c.postReply(ctx, msg.ChannelID, msg.ThreadTS, text)
}

func (c *Coordinator) markNotified(channelID, threadTS string) bool {
	key := channelID + "\x00" + threadTS
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.notified[key] {
		return false
	}
	c.notified[key] = true
	return true
}

// OnCommand handles `/diagnose <service> [environment]`, the human-driven way
// to start a session.
//
// Slash commands can only *start* a session: Slack does not tell us which
// thread a command was typed near, so a command can never identify an existing
// conversation. That is why ending a session is a button (session design
// section 2.1, edge case E1).
func (c *Coordinator) OnCommand(ctx context.Context, cmd Command) {
	if cmd.Command != DiagnoseCommand {
		// Slack only ever routes commands this app is registered for, but a
		// misconfigured app manifest (or a renamed command) must not fall
		// through to "diagnose whatever text follows" - that would let an
		// unrelated command silently trigger a paid RCA run.
		c.postReply(ctx, cmd.ChannelID, "",
			fmt.Sprintf("Unrecognized command %q.", cmd.Command))
		return
	}

	service, environment := parseDiagnoseArgs(cmd.Text, c.defaultEnvironment)
	if service == "" {
		c.postReply(ctx, cmd.ChannelID, "",
			"Usage: `/diagnose <service> [environment]`, for example `/diagnose support-agent`.")
		return
	}

	c.postReply(ctx, cmd.ChannelID, "", fmt.Sprintf(
		"Diagnosing *%s* in *%s*, asked by <@%s>. I will post the result here.",
		service, environment, cmd.UserID,
	))

	release, err := c.acquireAnalysis(ctx)
	if err != nil {
		c.postReply(ctx, cmd.ChannelID, "",
			"Too many analyses are running right now. Try `/diagnose` again in a moment.")
		return
	}
	defer release()

	diagnosis, err := c.rca.Diagnose(ctx, DiagnoseRequest{
		Service:     service,
		Environment: environment,
		ChannelID:   cmd.ChannelID,
		RequestedBy: cmd.UserID,
	})
	if err != nil {
		if errors.Is(err, ErrRCAUnavailable) {
			c.postReply(ctx, cmd.ChannelID, "",
				"The analysis engine is not connected yet, so I cannot diagnose on demand. "+
					"Alert-driven diagnoses still arrive here when the engine posts them.")
			return
		}
		c.logger.Error("diagnose failed",
			"service", service, "environment", environment, "error", err.Error())
		c.postReply(ctx, cmd.ChannelID, "", "The analysis failed: "+err.Error())
		return
	}

	if _, err := c.Announce(ctx, diagnosis); err != nil {
		c.logger.Error("could not announce the diagnosis",
			"service", service, "error", err.Error())
	}
}

// ReapIdle closes abandoned sessions and says so in their threads. The ticker
// that calls this belongs to the server's lifecycle, not here.
func (c *Coordinator) ReapIdle(ctx context.Context) {
	for _, expired := range c.sessions.ReapIdle() {
		c.metrics.DecisionRecorded(ctx, expired.Service, expired.Environment, "expired")
		c.logger.Info("session expired",
			"correlation_id", expired.CorrelationID, "thread_ts", expired.ThreadTS)
		c.postReply(ctx, expired.ChannelID, expired.ThreadTS,
			"Closing this session after a period of inactivity. "+
				"This does not silence a still-firing alert; run `/diagnose` to pick it up again.")
	}
}

func followupOutcome(err error) string {
	switch {
	case errors.Is(err, ErrRCAUnavailable):
		return FollowupUnavailable
	case errors.Is(err, errAnalysisBusy):
		return FollowupBusy
	default:
		return FollowupFailed
	}
}

var errAnalysisBusy = errors.New("slack: analysis concurrency limit reached")

// acquireAnalysis takes a slot in the analysis budget, or reports that the
// system is saturated. It never queues indefinitely: a storm of alerts must
// not turn into a storm of paid analysis (session design edge case E8).
func (c *Coordinator) acquireAnalysis(ctx context.Context) (func(), error) {
	select {
	case c.analysis <- struct{}{}:
		return func() { <-c.analysis }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return nil, errAnalysisBusy
	}
}

func (c *Coordinator) reply(ctx context.Context, s *session.Session, text string) {
	c.postReply(ctx, s.ChannelID, s.ThreadTS, text)
}

func (c *Coordinator) postReply(ctx context.Context, channelID, threadTS, text string) {
	if _, err := c.client.PostThreadReply(ctx, channelID, threadTS, text); err != nil {
		c.logger.Error("could not post a reply",
			"channel", channelID, "thread_ts", threadTS, "error", err.Error())
	}
}

func (c *Coordinator) audit(event session.AuditEvent) {
	if c.auditor == nil {
		return
	}
	if err := c.auditor.Append(event); err != nil {
		c.logger.Error("audit event not persisted", "type", event.Type, "error", err)
	}
}

// terminalWords are the phrases people type when they mean to decide. They are
// never acted on - they only trigger a pointer to the buttons.
var terminalWords = map[string]bool{
	"approve": true, "approved": true, "approve it": true,
	"decline": true, "declined": true, "reject": true, "rejected": true,
	"close": true, "closed": true, "close it": true,
	"done": true, "resolve": true, "resolved": true, "end": true,
	"lgtm": true, "ship it": true, "go ahead": true, "yes do it": true,
}

// isTerminalWord reports whether a message is a bare attempt to decide. Only
// short messages qualify: "approve" is an attempt, while "approve if you think
// the timeout theory holds" is a question, and treating it as a decision is
// exactly the misread this design avoids.
func isTerminalWord(text string) bool {
	normalised := strings.ToLower(strings.TrimSpace(text))
	normalised = strings.Trim(normalised, ".!?,")
	if normalised == "" || len(strings.Fields(normalised)) > 3 {
		return false
	}
	return terminalWords[normalised]
}

// parseDiagnoseArgs reads `<service> [environment]`, falling back to the
// configured default environment.
func parseDiagnoseArgs(text, defaultEnvironment string) (string, string) {
	fields := strings.Fields(strings.TrimSpace(text))
	switch len(fields) {
	case 0:
		return "", defaultEnvironment
	case 1:
		return fields[0], defaultEnvironment
	default:
		return fields[0], fields[1]
	}
}

// compile-time guard that the blocks helper keeps working with Slack's types.
var _ = func() []slack.Block { return ResolvedBlocks(nil, "") }
