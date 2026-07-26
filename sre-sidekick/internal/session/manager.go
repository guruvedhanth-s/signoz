package session

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
)

// Errors returned by the Manager. Callers are expected to match on these and
// answer the human in the thread: a message that arrives after a session ended
// must get one clear reply, not silence (session design edge case E7).
var (
	// ErrSessionClosed means the session is resolved or expired and no longer
	// accepts turns.
	ErrSessionClosed = errors.New("session: closed")
	// ErrEvidenceBudget means the session already pulled as much extra
	// evidence as it is allowed (session design edge case E9).
	ErrEvidenceBudget = errors.New("session: extra evidence budget exhausted")
	// ErrMissingThread means an open request had no Slack thread to key on.
	ErrMissingThread = errors.New("session: channel and thread timestamp are required")
)

// Defaults for the manager.
const (
	// DefaultTTL is how long a session may sit idle before the reaper closes
	// it (session design edge case E4).
	DefaultTTL = 30 * time.Minute
	// DefaultClosedRetention is how long a closed session is kept addressable
	// after it ends, so a late reply in that thread still gets a helpful
	// "this session is closed" answer instead of "unknown thread".
	DefaultClosedRetention = 24 * time.Hour
	// DefaultExtraEvidenceBudget bounds follow-up telemetry queries per
	// session (session design edge case E9).
	DefaultExtraEvidenceBudget = 10
)

// Manager owns every live session and the two indexes into them.
//
// Sessions live in memory only. A restart loses them, which is an accepted
// trade for the MVP (session design edge case E3): the inbound handler must
// answer a reply to a forgotten thread with "this session was lost, run
// /diagnose to reopen" rather than failing silently. Persistence is a later
// phase.
type Manager struct {
	mu sync.RWMutex
	// byThread routes an inbound reply or button click to its session. This
	// is the primary index: Slack hands us channel + thread_ts on every
	// inbound event, so no guessing is ever needed.
	byThread map[string]*Session
	// byFingerprint dedups a re-fired alert onto the session that already
	// exists for that incident. Only open sessions are indexed here: once a
	// session is resolved, the same alert firing again is a new incident and
	// deserves a new thread.
	byFingerprint map[string]*Session

	ttl                 time.Duration
	closedRetention     time.Duration
	extraEvidenceBudget int
	now                 func() time.Time
	store               Store
}

// ManagerOption customises a Manager.
type ManagerOption func(*Manager)

// WithStore enables session snapshot persistence. The memory store is used
// when omitted, preserving the original local behavior.
func WithStore(store Store) ManagerOption {
	return func(m *Manager) {
		if store != nil {
			m.store = store
		}
	}
}

// WithTTL sets the idle timeout used by ReapIdle.
func WithTTL(ttl time.Duration) ManagerOption {
	return func(m *Manager) {
		if ttl > 0 {
			m.ttl = ttl
		}
	}
}

// WithClosedRetention sets how long closed sessions stay addressable.
func WithClosedRetention(retention time.Duration) ManagerOption {
	return func(m *Manager) {
		if retention > 0 {
			m.closedRetention = retention
		}
	}
}

// WithExtraEvidenceBudget sets how many follow-up evidence items a single
// session may accumulate.
func WithExtraEvidenceBudget(budget int) ManagerOption {
	return func(m *Manager) {
		if budget >= 0 {
			m.extraEvidenceBudget = budget
		}
	}
}

// WithClock injects a clock, so tests can advance time without sleeping.
func WithClock(now func() time.Time) ManagerOption {
	return func(m *Manager) {
		if now != nil {
			m.now = now
		}
	}
}

// NewManager builds an empty session store.
func NewManager(opts ...ManagerOption) *Manager {
	manager := &Manager{
		byThread:            make(map[string]*Session),
		byFingerprint:       make(map[string]*Session),
		ttl:                 DefaultTTL,
		closedRetention:     DefaultClosedRetention,
		extraEvidenceBudget: DefaultExtraEvidenceBudget,
		now:                 time.Now,
		store:               NewMemoryStore(),
	}
	for _, opt := range opts {
		opt(manager)
	}
	return manager
}

// OpenRequest describes a session about to be born. A session is born from the
// message the sidekick just posted: that post is the thread root, so its
// channel and timestamp are known before Open is called.
type OpenRequest struct {
	ChannelID string
	ThreadTS  string
	Diagnosis notify.Diagnosis
	// Fingerprint may be supplied explicitly; when empty it is derived from
	// the diagnosis.
	Fingerprint string
}

// Open registers a new session, or returns the existing one when this incident
// already has a live thread.
//
// The second return value reports whether an existing session was found. Open
// is deliberately passive in that case: it does not post, does not append a
// turn, and does not touch the conversation. Whether a re-fire deserves an
// update in the thread is a policy question that depends on how long ago the
// last one was, so it belongs to the handler, not the store. What Open does
// guarantee is that the caller can skip a second, paid RCA run and a duplicate
// thread (session design edge case E2).
func (m *Manager) Open(req OpenRequest) (*Session, bool, error) {
	channel := strings.TrimSpace(req.ChannelID)
	thread := strings.TrimSpace(req.ThreadTS)
	if channel == "" || thread == "" {
		return nil, false, ErrMissingThread
	}

	fingerprint := strings.TrimSpace(req.Fingerprint)
	if fingerprint == "" {
		fingerprint = Fingerprint(
			req.Diagnosis.Service,
			req.Diagnosis.Environment,
			req.Diagnosis.Grounding.SLO,
		)
	}

	m.mu.Lock()

	if existing, ok := m.byFingerprint[fingerprint]; ok {
		m.mu.Unlock()
		return existing, true, nil
	}

	now := m.now()
	created := &Session{
		CorrelationID: req.Diagnosis.CorrelationID,
		ChannelID:     channel,
		ThreadTS:      thread,
		Fingerprint:   fingerprint,
		Service:       req.Diagnosis.Service,
		Environment:   req.Diagnosis.Environment,
		Window:        req.Diagnosis.Window,
		status:        StatusOpen,
		diagnosis:     req.Diagnosis,
		participants:  make(map[string]struct{}),
		CreatedAt:     now,
		lastActivity:  now,
	}

	m.byThread[threadKey(channel, thread)] = created
	m.byFingerprint[fingerprint] = created
	snapshot := created.viewLocked()
	m.mu.Unlock()
	// Persistence intentionally happens outside the manager lock so a slow or
	// unavailable store cannot block routing. During this small window a
	// concurrent lookup may observe the in-memory session; on persistence
	// failure the indexes are rolled back and the caller receives the error.
	if err := m.persistView(snapshot); err != nil {
		m.mu.Lock()
		delete(m.byThread, threadKey(channel, thread))
		delete(m.byFingerprint, fingerprint)
		m.mu.Unlock()
		return nil, false, fmt.Errorf("session: persist open: %w", err)
	}
	return created, false, nil
}

// ByThread routes an inbound reply or button click to its session.
func (m *Manager) ByThread(channelID, threadTS string) (*Session, bool) {
	m.mu.RLock()
	found, ok := m.byThread[threadKey(strings.TrimSpace(channelID), strings.TrimSpace(threadTS))]
	m.mu.RUnlock()
	if ok {
		return found, true
	}
	if m.store == nil {
		return nil, false
	}
	view, err := m.store.Load(strings.TrimSpace(channelID), strings.TrimSpace(threadTS))
	if err != nil {
		return nil, false
	}
	rehydrated := sessionFromView(view)
	m.mu.Lock()
	if existing, exists := m.byThread[threadKey(view.ChannelID, view.ThreadTS)]; exists {
		m.mu.Unlock()
		return existing, true
	}
	m.byThread[threadKey(view.ChannelID, view.ThreadTS)] = rehydrated
	if !rehydrated.Status().Terminal() {
		m.byFingerprint[rehydrated.Fingerprint] = rehydrated
	}
	m.mu.Unlock()
	return rehydrated, true
}

// ByFingerprint returns the live session for an incident, if there is one.
// Closed sessions are not indexed here: the same alert firing after a
// resolution is a new incident and gets a new thread.
func (m *Manager) ByFingerprint(fingerprint string) (*Session, bool) {
	m.mu.RLock()
	found, ok := m.byFingerprint[strings.TrimSpace(fingerprint)]
	m.mu.RUnlock()
	if ok {
		return found, true
	}
	if m.store == nil {
		return nil, false
	}
	view, err := m.store.LoadByFingerprint(strings.TrimSpace(fingerprint))
	if err != nil {
		return nil, false
	}
	rehydrated := sessionFromView(view)
	m.mu.Lock()
	if existing, exists := m.byFingerprint[rehydrated.Fingerprint]; exists {
		m.mu.Unlock()
		return existing, true
	}
	m.byThread[threadKey(rehydrated.ChannelID, rehydrated.ThreadTS)] = rehydrated
	m.byFingerprint[rehydrated.Fingerprint] = rehydrated
	m.mu.Unlock()
	return rehydrated, true
}

// Len reports how many sessions are addressable, open and recently closed.
func (m *Manager) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.byThread)
}

// Snapshot returns a copy of every addressable session, for tests and
// observability.
func (m *Manager) Snapshot() []View {
	m.mu.RLock()
	sessions := make([]*Session, 0, len(m.byThread))
	for _, found := range m.byThread {
		sessions = append(sessions, found)
	}
	m.mu.RUnlock()

	views := make([]View, 0, len(sessions))
	for _, found := range sessions {
		views = append(views, found.View())
	}
	return views
}

// AppendTurn records one exchange and refreshes the idle clock. A human turn
// also records the speaker as a participant.
//
// Callers should hold the session's turn lock (BeginTurn/EndTurn) around the
// whole turn, so two concurrent messages in one thread cannot interleave.
func (m *Manager) AppendTurn(s *Session, turn Turn) error {
	if s == nil {
		return ErrSessionClosed
	}

	s.mu.Lock()
	if s.status.Terminal() {
		status := s.status
		s.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrSessionClosed, status)
	}
	if turn.At.IsZero() {
		turn.At = m.now()
	}
	s.history = append(s.history, turn)
	if turn.IsHuman() {
		s.participants[string(turn.Actor)] = struct{}{}
	}
	s.lastActivity = m.now()
	snapshot := s.viewLocked()
	s.mu.Unlock()
	return m.persistView(snapshot)
}

// AddEvidence records evidence gathered after the diagnosis to answer a
// follow-up, within the session's budget.
func (m *Manager) AddEvidence(s *Session, evidence ...notify.Evidence) error {
	if s == nil {
		return ErrSessionClosed
	}

	s.mu.Lock()
	if s.status.Terminal() {
		status := s.status
		s.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrSessionClosed, status)
	}
	if len(s.extraEvidence)+len(evidence) > m.extraEvidenceBudget {
		s.mu.Unlock()
		return fmt.Errorf("%w: limit %d", ErrEvidenceBudget, m.extraEvidenceBudget)
	}
	s.extraEvidence = append(s.extraEvidence, evidence...)
	s.lastActivity = m.now()
	snapshot := s.viewLocked()
	s.mu.Unlock()
	return m.persistView(snapshot)
}

// Decide records a terminal human judgement and closes the session.
//
// The transition is single-writer: the first terminal decision wins and every
// later one is an idempotent no-op that returns the decision already on
// record, so the caller can answer "already resolved by @X at HH:MM". That
// kills the approve/decline race where two people decide at once, or one
// person double-taps (session design edge cases E5, section 6).
//
// Approval records intent only. Nothing is executed here or anywhere else in
// the MVP (PRD sections 5.6, 15).
func (m *Manager) Decide(s *Session, decision Decision) (bool, *Decision, error) {
	if s == nil {
		return false, nil, ErrSessionClosed
	}

	s.mu.Lock()
	if s.decision != nil {
		existing := *s.decision
		s.mu.Unlock()
		return false, &existing, nil
	}
	if s.status.Terminal() {
		status := s.status
		s.mu.Unlock()
		return false, nil, fmt.Errorf("%w: %s", ErrSessionClosed, status)
	}

	now := m.now()
	if decision.At.IsZero() {
		decision.At = now
	}
	recorded := decision
	s.decision = &recorded
	s.status = StatusResolved
	s.closeReason = ReasonDecided
	s.closedAt = now
	s.lastActivity = now
	if user := strings.TrimSpace(decision.UserID); user != "" {
		s.participants[user] = struct{}{}
	}
	fingerprint := s.Fingerprint
	snapshot := s.viewLocked()
	s.mu.Unlock()

	m.unindexFingerprint(fingerprint, s)
	if err := m.persistView(snapshot); err != nil {
		return false, nil, fmt.Errorf("session: persist decision: %w", err)
	}
	return true, &recorded, nil
}

// Close ends a session without a decision: a human tapped Close, the SLO
// recovered, or the reaper gave up on it.
//
// Closing an already-closed session returns ErrSessionClosed so the caller can
// reply once rather than silently doing nothing.
func (m *Manager) Close(s *Session, reason CloseReason) error {
	if s == nil {
		return ErrSessionClosed
	}

	s.mu.Lock()
	if s.status.Terminal() {
		status := s.status
		s.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrSessionClosed, status)
	}
	now := m.now()
	s.status = StatusResolved
	if reason == ReasonIdle {
		s.status = StatusExpired
	}
	s.closeReason = reason
	s.closedAt = now
	s.lastActivity = now
	fingerprint := s.Fingerprint
	snapshot := s.viewLocked()
	s.mu.Unlock()

	m.unindexFingerprint(fingerprint, s)
	return m.persistView(snapshot)
}

// Touch refreshes the idle clock without recording a turn, e.g. when an alert
// re-fires for an incident that already has a thread.
func (m *Manager) Touch(s *Session) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.status.Terminal() {
		s.lastActivity = m.now()
		snapshot := s.viewLocked()
		s.mu.Unlock()
		_ = m.persistView(snapshot)
		return
	}
	s.mu.Unlock()
}

// ReapIdle closes sessions that have been idle past the TTL and forgets closed
// sessions that are past the retention window.
//
// It returns the sessions it just expired so the caller can post "closing due
// to inactivity" into each thread. This package never posts anything itself -
// keeping it free of Slack is what makes it testable and reusable for another
// chat adapter.
//
// Call it on a ticker; the ticker's lifecycle belongs to the server, not here.
func (m *Manager) ReapIdle() []*Session {
	now := m.now()

	m.mu.RLock()
	candidates := make([]*Session, 0, len(m.byThread))
	for _, found := range m.byThread {
		candidates = append(candidates, found)
	}
	m.mu.RUnlock()

	expired := make([]*Session, 0)
	forget := make([]*Session, 0)

	for _, candidate := range candidates {
		candidate.mu.Lock()
		switch {
		case candidate.status.Terminal():
			if now.Sub(candidate.closedAt) >= m.closedRetention {
				forget = append(forget, candidate)
			}
		case now.Sub(candidate.lastActivity) >= m.ttl:
			candidate.status = StatusExpired
			candidate.closeReason = ReasonIdle
			candidate.closedAt = now
			expired = append(expired, candidate)
		}
		candidate.mu.Unlock()
		if candidate.Status().Terminal() {
			_ = m.persistView(candidate.View())
		}
	}

	if len(expired) > 0 || len(forget) > 0 {
		m.mu.Lock()
		for _, session := range expired {
			if m.byFingerprint[session.Fingerprint] == session {
				delete(m.byFingerprint, session.Fingerprint)
			}
		}
		for _, session := range forget {
			delete(m.byThread, threadKey(session.ChannelID, session.ThreadTS))
			if m.byFingerprint[session.Fingerprint] == session {
				delete(m.byFingerprint, session.Fingerprint)
			}
		}
		m.mu.Unlock()
		for _, session := range forget {
			if m.store != nil {
				_ = m.store.Delete(session.ChannelID, session.ThreadTS)
			}
		}
	}

	return expired
}

func (m *Manager) persistView(snapshot View) error {
	if m.store == nil {
		return nil
	}
	return m.store.Save(snapshot)
}

func sessionFromView(v View) *Session {
	participants := make(map[string]struct{}, len(v.Participants))
	for _, p := range v.Participants {
		participants[p] = struct{}{}
	}
	var decision *Decision
	if v.Decision != nil {
		d := *v.Decision
		decision = &d
	}
	return &Session{CorrelationID: v.CorrelationID, ChannelID: v.ChannelID, ThreadTS: v.ThreadTS,
		Fingerprint: v.Fingerprint, Service: v.Service, Environment: v.Environment, Window: v.Window,
		status: v.Status, diagnosis: v.Diagnosis, extraEvidence: append([]notify.Evidence(nil), v.ExtraEvidence...),
		history: append([]Turn(nil), v.History...), participants: participants, decision: decision,
		closeReason: v.CloseReason, CreatedAt: v.CreatedAt, lastActivity: v.LastActivity, closedAt: v.ClosedAt}
}

func (m *Manager) unindexFingerprint(fingerprint string, s *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byFingerprint[fingerprint] == s {
		delete(m.byFingerprint, fingerprint)
	}
}

func threadKey(channelID, threadTS string) string {
	return channelID + "\x00" + threadTS
}
