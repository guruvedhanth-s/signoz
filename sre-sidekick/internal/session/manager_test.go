package session

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
)

// testClock is an injectable clock so TTL behaviour is asserted without
// sleeping.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *testClock {
	return &testClock{now: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func diagnosis(service, environment, slo, correlationID string) notify.Diagnosis {
	return notify.Diagnosis{
		CorrelationID: correlationID,
		Service:       service,
		Environment:   environment,
		Window:        "30d",
		Status:        notify.StatusDiagnosed,
		Grounding:     notify.Grounding{SLO: slo, SLOState: "unhealthy"},
		RootCause:     "search_kb timeout",
		ProposedFix:   "raise the timeout",
	}
}

func openSession(t *testing.T, m *Manager, thread string, d notify.Diagnosis) *Session {
	t.Helper()
	s, existing, err := m.Open(OpenRequest{ChannelID: "C1", ThreadTS: thread, Diagnosis: d})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if existing {
		t.Fatalf("Open() reported an existing session, want a new one")
	}
	return s
}

func TestOpenIndexesByThreadAndFingerprint(t *testing.T) {
	m := NewManager()
	d := diagnosis("support-agent", "prod", "agent-success-rate", "corr-1")

	s := openSession(t, m, "1720000000.0001", d)

	if s.Status() != StatusOpen {
		t.Errorf("status = %q, want open", s.Status())
	}
	if s.Diagnosis().CorrelationID != "corr-1" {
		t.Error("diagnosis was not frozen onto the session")
	}

	found, ok := m.ByThread("C1", "1720000000.0001")
	if !ok || found != s {
		t.Error("ByThread did not route to the session")
	}
	if _, ok := m.ByFingerprint(Fingerprint("support-agent", "prod", "agent-success-rate")); !ok {
		t.Error("ByFingerprint did not find the session")
	}
}

func TestOpenRequiresChannelAndThread(t *testing.T) {
	m := NewManager()
	d := diagnosis("support-agent", "prod", "slo", "corr-1")

	for _, req := range []OpenRequest{
		{ChannelID: "", ThreadTS: "1", Diagnosis: d},
		{ChannelID: "C1", ThreadTS: "", Diagnosis: d},
	} {
		if _, _, err := m.Open(req); !errors.Is(err, ErrMissingThread) {
			t.Errorf("Open(%+v) error = %v, want ErrMissingThread", req, err)
		}
	}
}

// Two different incidents are two threads and never interfere: this is the
// whole reason a session is keyed on the thread rather than the user.
func TestTwoIncidentsAreIndependentSessions(t *testing.T) {
	m := NewManager()

	a := openSession(t, m, "1720000000.0001", diagnosis("support-agent", "prod", "success-rate", "corr-a"))
	b := openSession(t, m, "1720000050.0007", diagnosis("checkout-api", "prod", "latency", "corr-b"))

	if a == b {
		t.Fatal("two incidents shared one session")
	}
	if err := m.AppendTurn(a, Turn{Actor: "U1", Text: "why the timeout?"}); err != nil {
		t.Fatalf("AppendTurn() error = %v", err)
	}
	if len(b.History()) != 0 {
		t.Error("a turn in one session leaked into the other")
	}
}

// A re-fired alert must reuse the live thread rather than opening a second
// session and paying for a second RCA run.
func TestReFiredAlertReturnsExistingSession(t *testing.T) {
	m := NewManager()
	d := diagnosis("support-agent", "prod", "success-rate", "corr-1")
	first := openSession(t, m, "1720000000.0001", d)

	again := d
	again.CorrelationID = "corr-2"
	second, existing, err := m.Open(OpenRequest{
		ChannelID: "C1", ThreadTS: "1720000900.0002", Diagnosis: again,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if !existing {
		t.Fatal("re-fired alert opened a second session")
	}
	if second != first {
		t.Error("re-fire did not return the original session")
	}
	// Open is passive: it must not touch the conversation.
	if len(first.History()) != 0 {
		t.Error("Open appended a turn; it should leave the conversation alone")
	}
	if first.Diagnosis().CorrelationID != "corr-1" {
		t.Error("the frozen diagnosis was overwritten by the re-fire")
	}
}

// Once an incident is resolved, the same alert firing again is a new incident
// and deserves its own thread.
func TestReFireAfterResolutionOpensNewSession(t *testing.T) {
	m := NewManager()
	d := diagnosis("support-agent", "prod", "success-rate", "corr-1")
	first := openSession(t, m, "1720000000.0001", d)

	if _, _, err := m.Decide(first, Decision{Kind: DecisionDeclined, UserID: "U1"}); err != nil {
		t.Fatalf("Decide() error = %v", err)
	}

	second, existing, err := m.Open(OpenRequest{
		ChannelID: "C1", ThreadTS: "1720003600.0009", Diagnosis: d,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if existing {
		t.Fatal("a resolved session was reused, want a fresh one")
	}
	if second == first {
		t.Fatal("the resolved session was returned again")
	}
	if second.Status() != StatusOpen {
		t.Errorf("new session status = %q, want open", second.Status())
	}
}

// The window is deliberately not part of the fingerprint, or a shifting
// evaluation window would defeat deduplication entirely.
func TestFingerprintIgnoresCaseSpacingAndWindow(t *testing.T) {
	a := Fingerprint("support-agent", "prod", "success-rate")
	b := Fingerprint("  Support-Agent ", "PROD", "Success-Rate")
	if a != b {
		t.Errorf("fingerprints differ by case or spacing: %q vs %q", a, b)
	}
	if c := Fingerprint("support-agent", "staging", "success-rate"); a == c {
		t.Error("fingerprint ignores the environment; prod and staging would collide")
	}
	if c := Fingerprint("support-agent", "prod", "latency"); a == c {
		t.Error("fingerprint ignores the SLO; two SLOs on one service would collide")
	}
}

func TestAppendTurnTracksParticipantsAndActivity(t *testing.T) {
	clock := newClock()
	m := NewManager(WithClock(clock.Now))
	s := openSession(t, m, "1720000000.0001", diagnosis("support-agent", "prod", "slo", "corr-1"))

	clock.Advance(time.Minute)
	if err := m.AppendTurn(s, Turn{Actor: "U1", Text: "why the timeout?"}); err != nil {
		t.Fatalf("AppendTurn() error = %v", err)
	}
	if err := m.AppendTurn(s, Turn{Actor: ActorSidekick, Text: "78% of failing spans time out"}); err != nil {
		t.Fatalf("AppendTurn() error = %v", err)
	}
	if err := m.AppendTurn(s, Turn{Actor: "U2", Text: "could be the KB service"}); err != nil {
		t.Fatalf("AppendTurn() error = %v", err)
	}

	if len(s.History()) != 3 {
		t.Errorf("history length = %d, want 3", len(s.History()))
	}
	// The bot is not a participant; humans are.
	participants := s.Participants()
	if len(participants) != 2 {
		t.Errorf("participants = %v, want the two humans only", participants)
	}
	if s.LastActivity() != clock.Now() {
		t.Error("last activity was not refreshed by a turn")
	}
	if s.History()[0].At.IsZero() {
		t.Error("turn timestamp was not filled in")
	}
}

// A message arriving after the session ended must fail loudly enough for the
// handler to reply once, rather than being silently dropped.
func TestTurnsAfterCloseAreRejected(t *testing.T) {
	m := NewManager()
	s := openSession(t, m, "1720000000.0001", diagnosis("support-agent", "prod", "slo", "corr-1"))

	if err := m.Close(s, ReasonClosedByUser); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := m.AppendTurn(s, Turn{Actor: "U1", Text: "still there?"}); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("AppendTurn() error = %v, want ErrSessionClosed", err)
	}
	// The thread stays addressable so the handler can answer "this session is
	// closed" instead of "unknown thread".
	if _, ok := m.ByThread("C1", "1720000000.0001"); !ok {
		t.Error("closed session is no longer addressable by thread")
	}
}

func TestCloseIsNotSilentlyIdempotent(t *testing.T) {
	m := NewManager()
	s := openSession(t, m, "1720000000.0001", diagnosis("support-agent", "prod", "slo", "corr-1"))

	if err := m.Close(s, ReasonClosedByUser); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := m.Close(s, ReasonClosedByUser); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("second Close() error = %v, want ErrSessionClosed", err)
	}
}

func TestDecideRecordsWhoDecided(t *testing.T) {
	clock := newClock()
	m := NewManager(WithClock(clock.Now))
	s := openSession(t, m, "1720000000.0001", diagnosis("support-agent", "prod", "slo", "corr-1"))

	accepted, decision, err := m.Decide(s, Decision{
		Kind: DecisionApproved, UserID: "U1", Note: "go ahead",
	})
	if err != nil {
		t.Fatalf("Decide() error = %v", err)
	}
	if !accepted {
		t.Fatal("first decision was not accepted")
	}
	if decision.UserID != "U1" || decision.Kind != DecisionApproved {
		t.Errorf("decision = %+v, want an approval by U1", decision)
	}
	if decision.At.IsZero() {
		t.Error("decision timestamp was not filled in")
	}
	if s.Status() != StatusResolved {
		t.Errorf("status = %q, want resolved", s.Status())
	}
	if s.CloseReason() != ReasonDecided {
		t.Errorf("close reason = %q, want decided", s.CloseReason())
	}
	// The decider is on the record as a participant even if they never typed.
	if len(s.Participants()) != 1 {
		t.Errorf("participants = %v, want the decider", s.Participants())
	}
	// A resolved incident leaves the dedup index, so a later alert is new.
	if _, ok := m.ByFingerprint(s.Fingerprint); ok {
		t.Error("resolved session is still in the fingerprint index")
	}
}

// First terminal decision wins; a later one is an idempotent no-op that hands
// back the decision already on record.
func TestSecondDecisionIsANoOp(t *testing.T) {
	m := NewManager()
	s := openSession(t, m, "1720000000.0001", diagnosis("support-agent", "prod", "slo", "corr-1"))

	if _, _, err := m.Decide(s, Decision{Kind: DecisionApproved, UserID: "U1"}); err != nil {
		t.Fatalf("Decide() error = %v", err)
	}

	accepted, existing, err := m.Decide(s, Decision{Kind: DecisionDeclined, UserID: "U2"})
	if err != nil {
		t.Fatalf("second Decide() error = %v", err)
	}
	if accepted {
		t.Error("second decision was accepted; the first must win")
	}
	if existing == nil || existing.UserID != "U1" || existing.Kind != DecisionApproved {
		t.Errorf("existing decision = %+v, want the original approval by U1", existing)
	}
	if got := s.Decision(); got.UserID != "U1" {
		t.Errorf("recorded decision = %+v, want the original", got)
	}
}

// Two people deciding at once, or one person double-tapping, must produce
// exactly one recorded decision.
func TestConcurrentDecisionsElectOneWinner(t *testing.T) {
	m := NewManager()
	s := openSession(t, m, "1720000000.0001", diagnosis("support-agent", "prod", "slo", "corr-1"))

	const racers = 32
	var accepted atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			kind := DecisionApproved
			if i%2 == 0 {
				kind = DecisionDeclined
			}
			ok, _, err := m.Decide(s, Decision{Kind: kind, UserID: "U"})
			if err != nil {
				t.Errorf("Decide() error = %v", err)
			}
			if ok {
				accepted.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if got := accepted.Load(); got != 1 {
		t.Errorf("accepted decisions = %d, want exactly 1", got)
	}
}

func TestDecideOnClosedSessionFails(t *testing.T) {
	m := NewManager()
	s := openSession(t, m, "1720000000.0001", diagnosis("support-agent", "prod", "slo", "corr-1"))

	if err := m.Close(s, ReasonRecovered); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, _, err := m.Decide(s, Decision{Kind: DecisionApproved, UserID: "U1"}); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("Decide() error = %v, want ErrSessionClosed", err)
	}
}

func TestExtraEvidenceIsBudgeted(t *testing.T) {
	m := NewManager(WithExtraEvidenceBudget(2))
	s := openSession(t, m, "1720000000.0001", diagnosis("support-agent", "prod", "slo", "corr-1"))

	if err := m.AddEvidence(s, notify.Evidence{ID: "x1"}, notify.Evidence{ID: "x2"}); err != nil {
		t.Fatalf("AddEvidence() error = %v", err)
	}
	if err := m.AddEvidence(s, notify.Evidence{ID: "x3"}); !errors.Is(err, ErrEvidenceBudget) {
		t.Errorf("AddEvidence() error = %v, want ErrEvidenceBudget", err)
	}
	if len(s.ExtraEvidence()) != 2 {
		t.Errorf("extra evidence = %d items, want 2", len(s.ExtraEvidence()))
	}
}

// An abandoned session must not linger forever holding memory and context.
func TestReapIdleExpiresAbandonedSessions(t *testing.T) {
	clock := newClock()
	m := NewManager(WithClock(clock.Now), WithTTL(30*time.Minute))

	idle := openSession(t, m, "1720000000.0001", diagnosis("support-agent", "prod", "slo", "corr-1"))
	active := openSession(t, m, "1720000050.0007", diagnosis("checkout-api", "prod", "latency", "corr-2"))

	clock.Advance(20 * time.Minute)
	if err := m.AppendTurn(active, Turn{Actor: "U1", Text: "still looking"}); err != nil {
		t.Fatalf("AppendTurn() error = %v", err)
	}

	clock.Advance(15 * time.Minute) // idle: 35m, active: 15m
	expired := m.ReapIdle()

	if len(expired) != 1 || expired[0] != idle {
		t.Fatalf("expired = %v, want only the idle session", expired)
	}
	if idle.Status() != StatusExpired {
		t.Errorf("idle status = %q, want expired", idle.Status())
	}
	if idle.CloseReason() != ReasonIdle {
		t.Errorf("close reason = %q, want idle_timeout", idle.CloseReason())
	}
	if active.Status() != StatusOpen {
		t.Errorf("active status = %q, want open", active.Status())
	}
	// The caller posts the notice; the store never speaks for itself.
	if len(idle.History()) != 0 {
		t.Error("ReapIdle wrote into the conversation")
	}
	// An expired incident must be re-openable if it fires again.
	if _, ok := m.ByFingerprint(idle.Fingerprint); ok {
		t.Error("expired session is still in the fingerprint index")
	}
}

func TestReapIdleIsIdempotent(t *testing.T) {
	clock := newClock()
	m := NewManager(WithClock(clock.Now), WithTTL(time.Minute))
	openSession(t, m, "1720000000.0001", diagnosis("support-agent", "prod", "slo", "corr-1"))

	clock.Advance(2 * time.Minute)
	if len(m.ReapIdle()) != 1 {
		t.Fatal("first reap did not expire the session")
	}
	if got := m.ReapIdle(); len(got) != 0 {
		t.Errorf("second reap returned %v, want nothing", got)
	}
}

// Closed sessions stay addressable for a while so a late reply gets a helpful
// answer, then are forgotten so memory does not grow without bound.
func TestClosedSessionsAreForgottenAfterRetention(t *testing.T) {
	clock := newClock()
	m := NewManager(
		WithClock(clock.Now),
		WithTTL(30*time.Minute),
		WithClosedRetention(time.Hour),
	)
	s := openSession(t, m, "1720000000.0001", diagnosis("support-agent", "prod", "slo", "corr-1"))

	if err := m.Close(s, ReasonClosedByUser); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	clock.Advance(30 * time.Minute)
	m.ReapIdle()
	if _, ok := m.ByThread("C1", "1720000000.0001"); !ok {
		t.Error("closed session was forgotten before the retention window elapsed")
	}

	clock.Advance(31 * time.Minute)
	m.ReapIdle()
	if _, ok := m.ByThread("C1", "1720000000.0001"); ok {
		t.Error("closed session was not forgotten after the retention window")
	}
	if m.Len() != 0 {
		t.Errorf("manager holds %d sessions, want 0", m.Len())
	}
}

func TestTouchRefreshesIdleClockWithoutATurn(t *testing.T) {
	clock := newClock()
	m := NewManager(WithClock(clock.Now), WithTTL(30*time.Minute))
	s := openSession(t, m, "1720000000.0001", diagnosis("support-agent", "prod", "slo", "corr-1"))

	clock.Advance(25 * time.Minute)
	m.Touch(s)
	clock.Advance(10 * time.Minute)

	if expired := m.ReapIdle(); len(expired) != 0 {
		t.Errorf("expired = %v, want none after a touch", expired)
	}
	if len(s.History()) != 0 {
		t.Error("Touch recorded a turn; it should only move the clock")
	}
}

func TestSnapshotIsACopy(t *testing.T) {
	m := NewManager()
	s := openSession(t, m, "1720000000.0001", diagnosis("support-agent", "prod", "slo", "corr-1"))
	if err := m.AppendTurn(s, Turn{Actor: "U1", Text: "hello"}); err != nil {
		t.Fatalf("AppendTurn() error = %v", err)
	}

	views := m.Snapshot()
	if len(views) != 1 {
		t.Fatalf("snapshot has %d sessions, want 1", len(views))
	}
	views[0].History[0].Text = "tampered"

	if s.History()[0].Text != "hello" {
		t.Error("mutating a snapshot changed the live session")
	}
}

func TestNilSessionIsRejectedNotPanicking(t *testing.T) {
	m := NewManager()

	if err := m.AppendTurn(nil, Turn{}); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("AppendTurn(nil) error = %v", err)
	}
	if err := m.AddEvidence(nil); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("AddEvidence(nil) error = %v", err)
	}
	if err := m.Close(nil, ReasonClosedByUser); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("Close(nil) error = %v", err)
	}
	if _, _, err := m.Decide(nil, Decision{}); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("Decide(nil) error = %v", err)
	}
	m.Touch(nil) // must not panic
}

// Concurrent readers and writers across many sessions must not corrupt the
// indexes. Run with -race to make this meaningful.
func TestConcurrentUseIsSafe(t *testing.T) {
	m := NewManager(WithTTL(time.Millisecond))

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d := diagnosis("service", "prod", "slo", "corr")
			d.Service = string(rune('a' + i))
			s, _, err := m.Open(OpenRequest{
				ChannelID: "C1",
				ThreadTS:  time.Now().Format(time.RFC3339Nano) + string(rune('a'+i)),
				Diagnosis: d,
				// Distinct fingerprints, so every goroutine really opens one.
				Fingerprint: "fp-" + string(rune('a'+i)),
			})
			if err != nil {
				t.Errorf("Open() error = %v", err)
				return
			}
			_ = m.AppendTurn(s, Turn{Actor: "U1", Text: "hi"})
			_, _ = m.ByThread(s.ChannelID, s.ThreadTS)
			_, _, _ = m.Decide(s, Decision{Kind: DecisionApproved, UserID: "U1"})
			m.Snapshot()
			m.ReapIdle()
		}(i)
	}
	wg.Wait()

	if m.Len() != 16 {
		t.Errorf("manager holds %d sessions, want 16", m.Len())
	}
}

// A whole turn is serialised by the turn lock, so two concurrent replies in
// one thread cannot interleave their slow work.
func TestTurnLockSerialisesConcurrentTurns(t *testing.T) {
	m := NewManager()
	s := openSession(t, m, "1720000000.0001", diagnosis("support-agent", "prod", "slo", "corr-1"))

	var inFlight, maxInFlight atomic.Int32
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.BeginTurn()
			defer s.EndTurn()

			current := inFlight.Add(1)
			for {
				peak := maxInFlight.Load()
				if current <= peak || maxInFlight.CompareAndSwap(peak, current) {
					break
				}
			}
			time.Sleep(time.Millisecond) // stand-in for the slow RCA call
			_ = m.AppendTurn(s, Turn{Actor: "U1", Text: "question"})
			inFlight.Add(-1)
		}()
	}
	wg.Wait()

	if got := maxInFlight.Load(); got != 1 {
		t.Errorf("max concurrent turns = %d, want 1", got)
	}
	if len(s.History()) != 8 {
		t.Errorf("history length = %d, want 8", len(s.History()))
	}
}
