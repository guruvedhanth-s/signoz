// Package session holds the conversation state for one incident.
//
// The model, from `hackathon/slack-session-design.md`:
//
//	one incident = one Slack thread = one session
//
// The Slack thread timestamp (`thread_ts`) is the session key. Slack tells us
// which thread an inbound reply belongs to, so an incident never has to be
// guessed at: two incidents are two threads and therefore two fully
// independent sessions, and two people talking in one thread share one
// session. A user is never a session; an incident is.
//
// This package deliberately knows nothing about Slack, HTTP or LLMs. It is a
// pure in-memory state machine over sessions, which keeps it fast to test and
// lets the Slack adapter import it without an import cycle. Anything that
// needs to *say* something (post an expiry notice, reply "already resolved")
// is the caller's job; this package only reports what happened.
package session

import (
	"strings"
	"sync"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
)

// Status is where a session is in its lifecycle.
//
// There is deliberately no "awaiting decision" state: every open session with
// a proposed fix is implicitly awaiting one, so a separate state would be a
// second source of truth for the same fact.
type Status string

const (
	// StatusOpen means the conversation is live and accepting turns.
	StatusOpen Status = "open"
	// StatusResolved means a human made a terminal decision, or explicitly
	// closed the session.
	StatusResolved Status = "resolved"
	// StatusExpired means the idle reaper closed the session (session design
	// edge case E4).
	StatusExpired Status = "expired"
)

// Terminal reports whether no further turns are accepted.
func (s Status) Terminal() bool { return s == StatusResolved || s == StatusExpired }

// DecisionKind is the terminal human judgement on the proposed fix.
type DecisionKind string

const (
	// DecisionApproved records that a human approved the proposed fix.
	//
	// The MVP is advisory: this records intent and nothing executes (PRD
	// sections 5.6, 15). The record is shaped so a future executing adapter
	// can consume it unchanged.
	DecisionApproved DecisionKind = "approved"
	// DecisionDeclined records that a human rejected the proposed fix.
	DecisionDeclined DecisionKind = "declined"
)

// CloseReason explains why a session stopped, for the audit trail and for the
// message the caller posts into the thread.
type CloseReason string

const (
	// ReasonDecided means a human approved or declined.
	ReasonDecided CloseReason = "decided"
	// ReasonClosedByUser means a human tapped Close without deciding.
	ReasonClosedByUser CloseReason = "closed_by_user"
	// ReasonRecovered means the underlying SLO went healthy again, so the
	// system closed the session itself.
	ReasonRecovered CloseReason = "recovered"
	// ReasonIdle means the TTL reaper closed an abandoned session.
	ReasonIdle CloseReason = "idle_timeout"
)

// Decision is the audited record of a terminal human judgement. It always
// names the Slack user who made it: a shared channel means anyone who can see
// the thread can decide, so the audit log must always answer "who said go"
// (session design section 6, PRD section 20).
type Decision struct {
	Kind   DecisionKind
	UserID string
	// Authorization records the click-time policy verdict for audit and
	// rehydration. It is a string to keep the session package independent of
	// the Slack adapter's policy types.
	Authorization     string
	AuthorizationRole string
	// Note is any free text that accompanied the decision.
	Note string
	At   time.Time
}

// Actor identifies who produced a Turn.
type Actor string

const (
	// ActorSidekick marks a turn written by the bot.
	ActorSidekick Actor = "sidekick"
	// ActorSystem marks a turn recorded by the system rather than a human,
	// e.g. an auto-close notice.
	ActorSystem Actor = "system"
)

// Turn is one exchange in the conversation, kept so a follow-up question can
// be answered with the thread's context. Human turns carry the Slack user id
// as the actor.
type Turn struct {
	Actor Actor
	Text  string
	At    time.Time
}

// IsHuman reports whether the turn came from a Slack user rather than the bot
// or the system.
func (t Turn) IsHuman() bool {
	return t.Actor != ActorSidekick && t.Actor != ActorSystem && strings.TrimSpace(string(t.Actor)) != ""
}

// Session is one incident conversation.
//
// Locking, which matters because inbound Slack events arrive concurrently:
//
//   - mu guards this struct's fields. It is held only for short, non-blocking
//     updates, and never across a network or LLM call.
//   - The turn lock (BeginTurn/EndTurn) serialises a whole human turn,
//     including the slow RCA call it triggers, so two people typing at once in
//     one thread are handled one after another and the history cannot be
//     corrupted by two half-finished updates (session design edge cases E5,
//     E15).
//
// Lock ordering is always Manager.mu -> Session.mu, never the reverse.
type Session struct {
	mu   sync.Mutex
	turn sync.Mutex

	// CorrelationID ties the session to the diagnosis and to every message,
	// approval and action that follows it (PRD section 20).
	CorrelationID string
	ChannelID     string
	// ThreadTS is the Slack timestamp of the root message: the session key.
	ThreadTS string
	// Fingerprint is the dedup key for the underlying incident, so a re-fired
	// alert updates this thread instead of opening a second one (session
	// design edge case E2).
	Fingerprint string

	Service     string
	Environment string
	Window      string

	status Status

	// diagnosis is frozen at birth. Its deterministic grounding facts are
	// pinned into every follow-up so the model never regenerates them (PRD
	// section 7).
	diagnosis notify.Diagnosis
	// extraEvidence holds evidence pulled *after* the diagnosis to answer a
	// follow-up. It is budgeted so a chatty thread cannot run up unbounded
	// query cost (session design edge case E9).
	extraEvidence []notify.Evidence

	history      []Turn
	participants map[string]struct{}
	decision     *Decision
	closeReason  CloseReason

	CreatedAt    time.Time
	lastActivity time.Time
	closedAt     time.Time
}

// BeginTurn takes the turn lock. Callers must pair it with EndTurn, and should
// hold it for the whole turn - classify, answer, reply - so that concurrent
// messages in one thread are processed one at a time.
func (s *Session) BeginTurn() { s.turn.Lock() }

// EndTurn releases the turn lock.
func (s *Session) EndTurn() { s.turn.Unlock() }

// Status returns the current lifecycle state.
func (s *Session) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// Diagnosis returns the frozen diagnosis this session is about.
func (s *Session) Diagnosis() notify.Diagnosis {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.diagnosis
}

// History returns a copy of the conversation so far.
func (s *Session) History() []Turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Turn(nil), s.history...)
}

// ExtraEvidence returns a copy of the evidence gathered after the diagnosis.
func (s *Session) ExtraEvidence() []notify.Evidence {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]notify.Evidence(nil), s.extraEvidence...)
}

// Decision returns a copy of the terminal decision, or nil if none was made.
func (s *Session) Decision() *Decision {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.decision == nil {
		return nil
	}
	decision := *s.decision
	return &decision
}

// Participants returns the Slack user ids that have spoken in this thread.
// Incident response is a team activity, so the audit trail records who was in
// the room, not only who decided (session design section 6).
func (s *Session) Participants() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	users := make([]string, 0, len(s.participants))
	for user := range s.participants {
		users = append(users, user)
	}
	return users
}

// LastActivity is when this session last saw a turn or a decision. The reaper
// uses it (session design edge case E4).
func (s *Session) LastActivity() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastActivity
}

// CloseReason returns why the session ended, or "" if it is still open.
func (s *Session) CloseReason() CloseReason {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeReason
}

// ClosedAt returns when the session ended, or the zero time if it is open.
func (s *Session) ClosedAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closedAt
}

// View is an immutable snapshot of a session, safe to copy, log and assert on.
// Session itself contains mutexes and must always be passed by pointer.
type View struct {
	CorrelationID string
	ChannelID     string
	ThreadTS      string
	Fingerprint   string
	Service       string
	Environment   string
	Window        string
	Status        Status
	Diagnosis     notify.Diagnosis
	ExtraEvidence []notify.Evidence
	History       []Turn
	Participants  []string
	Decision      *Decision
	CloseReason   CloseReason
	CreatedAt     time.Time
	LastActivity  time.Time
	ClosedAt      time.Time
}

// View snapshots the session.
func (s *Session) View() View {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.viewLocked()
}

func (s *Session) viewLocked() View {
	view := View{
		CorrelationID: s.CorrelationID,
		ChannelID:     s.ChannelID,
		ThreadTS:      s.ThreadTS,
		Fingerprint:   s.Fingerprint,
		Service:       s.Service,
		Environment:   s.Environment,
		Window:        s.Window,
		Status:        s.status,
		Diagnosis:     s.diagnosis,
		ExtraEvidence: append([]notify.Evidence(nil), s.extraEvidence...),
		History:       append([]Turn(nil), s.history...),
		CloseReason:   s.closeReason,
		CreatedAt:     s.CreatedAt,
		LastActivity:  s.lastActivity,
		ClosedAt:      s.closedAt,
	}
	for user := range s.participants {
		view.Participants = append(view.Participants, user)
	}
	if s.decision != nil {
		decision := *s.decision
		view.Decision = &decision
	}
	return view
}

// Fingerprint is the incident dedup key: the same service, environment and SLO
// firing again is the same incident, not a new one (session design edge cases
// E2, E19).
//
// The evaluation window is deliberately excluded. A window that shifts between
// evaluations would make every re-fire look like a new incident and silently
// defeat deduplication - the exact failure this key exists to prevent.
func Fingerprint(service, environment, slo string) string {
	normalise := func(value string) string {
		return strings.ToLower(strings.TrimSpace(value))
	}
	return strings.Join([]string{
		normalise(service),
		normalise(environment),
		normalise(slo),
	}, "|")
}
