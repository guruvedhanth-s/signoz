package slack

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/session"
)

// fakeRCA stands in for the analysis engine, which lives behind the RCA
// interface and in another branch.
type fakeRCA struct {
	mu sync.Mutex

	answer      string
	answerErr   error
	diagnosis   notify.Diagnosis
	diagnoseErr error

	requests  []FollowupRequest
	diagnosed []DiagnoseRequest

	// block, when set, holds Diagnose until it is closed, so concurrency
	// limits can be exercised.
	block chan struct{}
}

func (f *fakeRCA) Diagnose(_ context.Context, req DiagnoseRequest) (notify.Diagnosis, error) {
	f.mu.Lock()
	f.diagnosed = append(f.diagnosed, req)
	block := f.block
	f.mu.Unlock()

	if block != nil {
		<-block
	}
	return f.diagnosis, f.diagnoseErr
}

func (f *fakeRCA) AnswerFollowup(_ context.Context, req FollowupRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	return f.answer, f.answerErr
}

func (f *fakeRCA) lastRequest(t *testing.T) FollowupRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("the analysis engine was never asked anything")
	}
	return f.requests[len(f.requests)-1]
}

type coordinatorFixture struct {
	coordinator *Coordinator
	poster      *fakePoster
	sessions    *session.Manager
	rca         *fakeRCA
	logs        *syncBuffer
}

func newFixture(t *testing.T, rca RCA, opts ...CoordinatorOption) *coordinatorFixture {
	t.Helper()

	poster := &fakePoster{}
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	client, err := New(testConfig(),
		WithPoster(poster),
		WithLogger(logger),
		withoutJitter(),
		withClock(time.Now, func(context.Context, time.Duration) error { return nil }),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	sessions := session.NewManager()
	coordinator, err := NewCoordinator(client, sessions, rca,
		append([]CoordinatorOption{WithCoordinatorLogger(logger)}, opts...)...)
	if err != nil {
		t.Fatalf("NewCoordinator() error = %v", err)
	}

	fixture := &coordinatorFixture{
		coordinator: coordinator,
		poster:      poster,
		sessions:    sessions,
		rca:         nil,
		logs:        logs,
	}
	if typed, ok := rca.(*fakeRCA); ok {
		fixture.rca = typed
	}
	return fixture
}

// postedText returns the plain text of the nth posted message.
func (f *coordinatorFixture) postedText(t *testing.T, index int) string {
	t.Helper()
	f.poster.mu.Lock()
	defer f.poster.mu.Unlock()
	if index >= len(f.poster.calls) {
		t.Fatalf("message %d was not posted; %d were", index, len(f.poster.calls))
	}
	return optionText(t, f.poster.calls[index].options)
}

func (f *coordinatorFixture) lastPosted(t *testing.T) string {
	t.Helper()
	f.poster.mu.Lock()
	count := len(f.poster.calls)
	f.poster.mu.Unlock()
	if count == 0 {
		t.Fatal("nothing was posted")
	}
	return f.postedText(t, count-1)
}

// optionText applies the message options the way the Slack library would and
// pulls out the text field.
func optionText(t *testing.T, options []slack.MsgOption) string {
	t.Helper()
	_, values, err := slack.UnsafeApplyMsgOptions("xoxb-test", "C1", "http://localhost/", options...)
	if err != nil {
		t.Fatalf("apply msg options: %v", err)
	}
	return values.Get("text")
}

func optionThreadTS(t *testing.T, options []slack.MsgOption) string {
	t.Helper()
	_, values, err := slack.UnsafeApplyMsgOptions("xoxb-test", "C1", "http://localhost/", options...)
	if err != nil {
		t.Fatalf("apply msg options: %v", err)
	}
	return values.Get("thread_ts")
}

func announceFixture(t *testing.T, f *coordinatorFixture) *session.Session {
	t.Helper()
	opened, err := f.coordinator.Announce(context.Background(), diagnosedFixture())
	if err != nil {
		t.Fatalf("Announce() error = %v", err)
	}
	return opened
}

func TestAnnounceOpensASessionOnThePostedThread(t *testing.T) {
	f := newFixture(t, &fakeRCA{})

	opened := announceFixture(t, f)

	if opened.ThreadTS != "1720000000.000100" {
		t.Errorf("thread ts = %q, want the posted message timestamp", opened.ThreadTS)
	}
	if opened.Status() != session.StatusOpen {
		t.Errorf("status = %q, want open", opened.Status())
	}
	if _, ok := f.sessions.ByThread(opened.ChannelID, opened.ThreadTS); !ok {
		t.Error("the session is not routable by thread")
	}
}

// A re-fired alert must land in the existing thread, not open a second one.
func TestAnnounceDedupsAReFiredAlert(t *testing.T) {
	f := newFixture(t, &fakeRCA{})
	first := announceFixture(t, f)

	again := diagnosedFixture()
	again.CorrelationID = "corr-456"
	second, err := f.coordinator.Announce(context.Background(), again)
	if err != nil {
		t.Fatalf("Announce() error = %v", err)
	}

	if second != first {
		t.Error("the re-fire opened a second session")
	}
	if f.sessions.Len() != 1 {
		t.Errorf("sessions = %d, want 1", f.sessions.Len())
	}
	if !strings.Contains(f.lastPosted(t), "fired again") {
		t.Errorf("the re-fire was not reported in the thread: %q", f.lastPosted(t))
	}
	// The notice belongs in the existing thread.
	f.poster.mu.Lock()
	defer f.poster.mu.Unlock()
	last := f.poster.calls[len(f.poster.calls)-1]
	if optionThreadTS(t, last.options) != first.ThreadTS {
		t.Error("the re-fire notice was not posted in the original thread")
	}
}

func TestApproveRecordsTheDeciderAndRetiresTheButtons(t *testing.T) {
	f := newFixture(t, &fakeRCA{})
	opened := announceFixture(t, f)

	f.coordinator.OnInteraction(context.Background(), Interaction{
		ActionID:  ActionApprove,
		Value:     "corr-123",
		ChannelID: opened.ChannelID,
		ThreadTS:  opened.ThreadTS,
		MessageTS: opened.ThreadTS,
		UserID:    "U1",
	})

	decision := opened.Decision()
	if decision == nil {
		t.Fatal("no decision was recorded")
	}
	if decision.Kind != session.DecisionApproved || decision.UserID != "U1" {
		t.Errorf("decision = %+v, want an approval by U1", decision)
	}
	if opened.Status() != session.StatusResolved {
		t.Errorf("status = %q, want resolved", opened.Status())
	}

	// The original message is rewritten so no live-looking button remains.
	// (What the rewrite contains is asserted by TestResolvedBlocks.)
	if f.poster.updateCount() != 1 {
		t.Fatalf("message updates = %d, want 1", f.poster.updateCount())
	}
	f.poster.mu.Lock()
	update := f.poster.updates[0]
	f.poster.mu.Unlock()
	if update.updateTS != opened.ThreadTS {
		t.Errorf("updated message ts = %q, want the diagnosis message", update.updateTS)
	}
	if update.channel != opened.ChannelID {
		t.Errorf("updated channel = %q", update.channel)
	}

	// Advisory only: the reply must say nothing was executed.
	if reply := f.lastPosted(t); !strings.Contains(reply, "Nothing was executed") {
		t.Errorf("reply = %q, want it to state that nothing ran", reply)
	}
	if !strings.Contains(f.logs.String(), "decision recorded") {
		t.Error("the decision was not logged for audit")
	}
}

func TestDeclineIsRecordedWithoutPromisingAction(t *testing.T) {
	f := newFixture(t, &fakeRCA{})
	opened := announceFixture(t, f)

	f.coordinator.OnInteraction(context.Background(), Interaction{
		ActionID: ActionDecline, ChannelID: opened.ChannelID,
		ThreadTS: opened.ThreadTS, MessageTS: opened.ThreadTS, UserID: "U2",
	})

	decision := opened.Decision()
	if decision == nil || decision.Kind != session.DecisionDeclined {
		t.Fatalf("decision = %+v, want a decline", decision)
	}
	if reply := f.lastPosted(t); !strings.Contains(reply, "declined") {
		t.Errorf("reply = %q", reply)
	}
}

// Two people deciding at once, or one double-tap, must not produce two
// decisions: the first stands and the second is told so.
func TestSecondDecisionIsRejectedWithWhoDecided(t *testing.T) {
	f := newFixture(t, &fakeRCA{})
	opened := announceFixture(t, f)
	click := Interaction{
		ChannelID: opened.ChannelID, ThreadTS: opened.ThreadTS,
		MessageTS: opened.ThreadTS,
	}

	approve := click
	approve.ActionID, approve.UserID = ActionApprove, "U1"
	f.coordinator.OnInteraction(context.Background(), approve)

	decline := click
	decline.ActionID, decline.UserID = ActionDecline, "U2"
	f.coordinator.OnInteraction(context.Background(), decline)

	if got := opened.Decision(); got.UserID != "U1" || got.Kind != session.DecisionApproved {
		t.Errorf("decision = %+v, want the first one to stand", got)
	}
	reply := f.lastPosted(t)
	if !strings.Contains(reply, "Already approved") || !strings.Contains(reply, "U1") {
		t.Errorf("reply = %q, want it to name the existing decision and decider", reply)
	}
}

func TestCloseButtonEndsTheSession(t *testing.T) {
	f := newFixture(t, &fakeRCA{})
	opened := announceFixture(t, f)

	f.coordinator.OnInteraction(context.Background(), Interaction{
		ActionID: ActionClose, ChannelID: opened.ChannelID,
		ThreadTS: opened.ThreadTS, MessageTS: opened.ThreadTS, UserID: "U1",
	})

	if opened.Status() != session.StatusResolved {
		t.Errorf("status = %q, want resolved", opened.Status())
	}
	if opened.CloseReason() != session.ReasonClosedByUser {
		t.Errorf("close reason = %q", opened.CloseReason())
	}
	// Ending a conversation is not the same as silencing an alert; the reply
	// must not imply otherwise.
	if reply := f.lastPosted(t); !strings.Contains(reply, "does not silence") {
		t.Errorf("reply = %q, want the alert caveat", reply)
	}
}

func TestClickOnALostSessionIsAnswered(t *testing.T) {
	f := newFixture(t, &fakeRCA{})

	f.coordinator.OnInteraction(context.Background(), Interaction{
		ActionID: ActionApprove, ChannelID: "C1", ThreadTS: "9999.0001", UserID: "U1",
	})

	if reply := f.lastPosted(t); !strings.Contains(reply, "lost") {
		t.Errorf("reply = %q, want it to say the session was lost", reply)
	}
}

func TestFollowupAnswersWithFrozenContext(t *testing.T) {
	rca := &fakeRCA{answer: "78% of the failing spans are TimeoutError from tool.search_kb."}
	f := newFixture(t, rca)
	opened := announceFixture(t, f)

	f.coordinator.OnMessage(context.Background(), Message{
		ChannelID: opened.ChannelID,
		ThreadTS:  opened.ThreadTS,
		MessageTS: "1720000100.0002",
		UserID:    "U1",
		Text:      "why do you think it is the timeout?",
	})

	req := rca.lastRequest(t)
	if req.Question != "why do you think it is the timeout?" {
		t.Errorf("question = %q", req.Question)
	}
	if req.AskedBy != "U1" {
		t.Errorf("asked by = %q", req.AskedBy)
	}
	// The deterministic facts are pinned from the frozen diagnosis, not
	// regenerated per turn.
	if req.Diagnosis.Grounding.BurnRate != 14.2 || req.Diagnosis.Grounding.SLO != "agent-success-rate" {
		t.Errorf("grounding = %+v, want the frozen facts", req.Diagnosis.Grounding)
	}
	if req.CorrelationID != "corr-123" {
		t.Errorf("correlation id = %q", req.CorrelationID)
	}
	// History is the conversation so far; the new question travels in
	// Question, not duplicated into the history.
	if len(req.History) != 0 {
		t.Errorf("history = %+v, want the new question kept out of it", req.History)
	}

	if reply := f.lastPosted(t); !strings.Contains(reply, "TimeoutError") {
		t.Errorf("reply = %q, want the analysis answer", reply)
	}
	// Both sides of the exchange are on the record for the next turn.
	if len(opened.History()) != 2 {
		t.Errorf("history = %d turns, want the question and the answer", len(opened.History()))
	}
}

func TestFollowupHistoryAccumulates(t *testing.T) {
	rca := &fakeRCA{answer: "answer"}
	f := newFixture(t, rca)
	opened := announceFixture(t, f)

	for _, text := range []string{"first question", "second question"} {
		f.coordinator.OnMessage(context.Background(), Message{
			ChannelID: opened.ChannelID, ThreadTS: opened.ThreadTS,
			MessageTS: "1720000100.0002", UserID: "U1", Text: text,
		})
	}

	req := rca.lastRequest(t)
	// The first exchange is history; the second question is the Question.
	if len(req.History) != 2 {
		t.Fatalf("history = %d turns, want the first exchange", len(req.History))
	}
	if req.History[0].Text != "first question" || req.History[0].Role != RoleHuman {
		t.Errorf("history is out of order: %+v", req.History)
	}
	if req.History[1].Role != RoleSidekick {
		t.Errorf("the answer was not recorded as the sidekick's: %+v", req.History)
	}
	if req.Question != "second question" {
		t.Errorf("question = %q", req.Question)
	}
}

// Free text is never a decision. Typing "approve" points at the button; it
// does not record an approval.
func TestTerminalWordNudgesTowardTheButton(t *testing.T) {
	rca := &fakeRCA{answer: "should not be called"}
	f := newFixture(t, rca)
	opened := announceFixture(t, f)

	f.coordinator.OnMessage(context.Background(), Message{
		ChannelID: opened.ChannelID, ThreadTS: opened.ThreadTS,
		MessageTS: "1720000100.0002", UserID: "U1", Text: "approve",
	})

	if opened.Decision() != nil {
		t.Fatal("typed text recorded a decision; only buttons may decide")
	}
	if reply := f.lastPosted(t); !strings.Contains(reply, "Approve") {
		t.Errorf("reply = %q, want a pointer to the buttons", reply)
	}
	rca.mu.Lock()
	defer rca.mu.Unlock()
	if len(rca.requests) != 0 {
		t.Error("the nudge still paid for an analysis call")
	}
}

func TestIsTerminalWord(t *testing.T) {
	for _, text := range []string{"approve", "Approved.", " done ", "LGTM", "ship it", "close it"} {
		if !isTerminalWord(text) {
			t.Errorf("isTerminalWord(%q) = false, want true", text)
		}
	}
	// A qualified sentence is a question, not a decision. Treating it as an
	// approval is the exact misread this design avoids.
	for _, text := range []string{
		"approve if you think the timeout theory holds",
		"not approved",
		"should we approve this?",
		"",
		"why is it burning so fast?",
	} {
		if isTerminalWord(text) {
			t.Errorf("isTerminalWord(%q) = true, want false", text)
		}
	}
}

func TestTopLevelMessagesAreIgnored(t *testing.T) {
	rca := &fakeRCA{answer: "answer"}
	f := newFixture(t, rca)
	announceFixture(t, f)
	posted := f.poster.callCount()

	f.coordinator.OnMessage(context.Background(), Message{
		ChannelID: "C1", MessageTS: "1720000100.0002", UserID: "U1",
		Text: "unrelated chatter in the channel",
	})

	if f.poster.callCount() != posted {
		t.Error("the bot replied to a message that belongs to no incident")
	}
}

// A reply in a thread the sidekick did not start must not be answered: the bot
// should never barge into an unrelated conversation.
func TestUnknownThreadNotOursStaysSilent(t *testing.T) {
	f := newFixture(t, &fakeRCA{})
	f.poster.replies = []slack.Message{{}} // a human-rooted thread: no bot id

	f.coordinator.OnMessage(context.Background(), Message{
		ChannelID: "C1", ThreadTS: "8888.0001", MessageTS: "8888.0002",
		UserID: "U1", Text: "hello?",
	})

	if f.poster.callCount() != 0 {
		t.Errorf("the bot spoke in a thread that was not its own: %q", f.lastPosted(t))
	}
}

// After a restart the session is gone but the thread is still ours, so the
// human gets an answer instead of silence.
func TestUnknownThreadOfOursExplainsTheLostSession(t *testing.T) {
	f := newFixture(t, &fakeRCA{})
	botRoot := slack.Message{}
	botRoot.BotID = "B1"
	f.poster.replies = []slack.Message{botRoot}

	msg := Message{
		ChannelID: "C1", ThreadTS: "8888.0001", MessageTS: "8888.0002",
		UserID: "U1", Text: "are you still there?",
	}
	f.coordinator.OnMessage(context.Background(), msg)

	if reply := f.lastPosted(t); !strings.Contains(reply, "/diagnose") {
		t.Errorf("reply = %q, want it to point at /diagnose", reply)
	}

	// Told once, not once per message.
	f.coordinator.OnMessage(context.Background(), msg)
	if f.poster.callCount() != 1 {
		t.Errorf("posts = %d, want the notice sent exactly once", f.poster.callCount())
	}
}

func TestUnknownThreadStaysSilentWhenTheLookupFails(t *testing.T) {
	f := newFixture(t, &fakeRCA{})
	f.poster.repliesErr = errors.New("channel_not_found")

	f.coordinator.OnMessage(context.Background(), Message{
		ChannelID: "C1", ThreadTS: "8888.0001", MessageTS: "8888.0002",
		UserID: "U1", Text: "hello?",
	})

	if f.poster.callCount() != 0 {
		t.Error("the bot spoke despite not knowing whose thread it was")
	}
}

// A message in a closed thread gets one reply, not silence and not a reply per
// message.
func TestClosedSessionIsAnsweredOnce(t *testing.T) {
	rca := &fakeRCA{answer: "answer"}
	f := newFixture(t, rca)
	opened := announceFixture(t, f)

	if err := f.sessions.Close(opened, session.ReasonClosedByUser); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	before := f.poster.callCount()

	msg := Message{
		ChannelID: opened.ChannelID, ThreadTS: opened.ThreadTS,
		MessageTS: "1720000100.0002", UserID: "U1", Text: "one more thing",
	}
	f.coordinator.OnMessage(context.Background(), msg)
	f.coordinator.OnMessage(context.Background(), msg)

	if got := f.poster.callCount() - before; got != 1 {
		t.Errorf("replies = %d, want exactly one closed-session notice", got)
	}
	if reply := f.lastPosted(t); !strings.Contains(reply, "closed") {
		t.Errorf("reply = %q", reply)
	}
	rca.mu.Lock()
	defer rca.mu.Unlock()
	if len(rca.requests) != 0 {
		t.Error("a closed session still paid for an analysis call")
	}
}

func TestExpiredSessionSaysSoExplicitly(t *testing.T) {
	f := newFixture(t, &fakeRCA{})
	opened := announceFixture(t, f)
	if err := f.sessions.Close(opened, session.ReasonIdle); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	f.coordinator.OnMessage(context.Background(), Message{
		ChannelID: opened.ChannelID, ThreadTS: opened.ThreadTS,
		MessageTS: "1720000100.0002", UserID: "U1", Text: "hello?",
	})

	if reply := f.lastPosted(t); !strings.Contains(reply, "expired") {
		t.Errorf("reply = %q, want the expiry explained", reply)
	}
}

// The adapter must say the engine is missing rather than improvise an answer.
func TestUnavailableAnalysisIsStatedHonestly(t *testing.T) {
	f := newFixture(t, UnavailableRCA{})
	opened := announceFixture(t, f)

	f.coordinator.OnMessage(context.Background(), Message{
		ChannelID: opened.ChannelID, ThreadTS: opened.ThreadTS,
		MessageTS: "1720000100.0002", UserID: "U1", Text: "why the timeout?",
	})

	reply := f.lastPosted(t)
	if !strings.Contains(reply, "not connected") {
		t.Errorf("reply = %q, want it to admit the engine is missing", reply)
	}
	// The deterministic facts stay true and are not disowned.
	if !strings.Contains(reply, "SLO engine") {
		t.Errorf("reply = %q, want it to keep the grounded facts standing", reply)
	}
}

func TestFollowupErrorIsReportedNotSwallowed(t *testing.T) {
	f := newFixture(t, &fakeRCA{answerErr: errors.New("model timeout")})
	opened := announceFixture(t, f)

	f.coordinator.OnMessage(context.Background(), Message{
		ChannelID: opened.ChannelID, ThreadTS: opened.ThreadTS,
		MessageTS: "1720000100.0002", UserID: "U1", Text: "why?",
	})

	if reply := f.lastPosted(t); !strings.Contains(reply, "model timeout") {
		t.Errorf("reply = %q, want the failure surfaced", reply)
	}
}

func TestDiagnoseCommandOpensASession(t *testing.T) {
	rca := &fakeRCA{diagnosis: diagnosedFixture()}
	f := newFixture(t, rca)

	f.coordinator.OnCommand(context.Background(), Command{
		Command: "/diagnose", Text: "support-agent", ChannelID: "C1", UserID: "U1",
	})

	rca.mu.Lock()
	requested := rca.diagnosed
	rca.mu.Unlock()
	if len(requested) != 1 {
		t.Fatalf("analysis runs = %d, want 1", len(requested))
	}
	if requested[0].Service != "support-agent" {
		t.Errorf("service = %q", requested[0].Service)
	}
	// An SLO is always scoped to an environment, so a command without one
	// falls back to the configured default rather than guessing.
	if requested[0].Environment != "prod" {
		t.Errorf("environment = %q, want the configured default", requested[0].Environment)
	}
	if f.sessions.Len() != 1 {
		t.Errorf("sessions = %d, want the command to have opened one", f.sessions.Len())
	}
}

// Slack only ever routes commands this app is registered for, but the
// handler must not trust that blindly: an unrecognized command name must
// never fall through to "diagnose whatever text follows", or a renamed or
// misconfigured command could trigger a paid RCA run silently.
func TestUnrecognizedCommandIsRejected(t *testing.T) {
	rca := &fakeRCA{diagnosis: diagnosedFixture()}
	f := newFixture(t, rca)

	f.coordinator.OnCommand(context.Background(), Command{
		Command: "/other", Text: "support-agent", ChannelID: "C1", UserID: "U1",
	})

	if reply := f.lastPosted(t); !strings.Contains(reply, "Unrecognized command") {
		t.Errorf("reply = %q, want an unrecognized-command message", reply)
	}
	rca.mu.Lock()
	defer rca.mu.Unlock()
	if len(rca.diagnosed) != 0 {
		t.Error("an unrecognized command still triggered an analysis run")
	}
}

func TestDiagnoseCommandAcceptsAnExplicitEnvironment(t *testing.T) {
	rca := &fakeRCA{diagnosis: diagnosedFixture()}
	f := newFixture(t, rca, WithDefaultEnvironment("prod"))

	f.coordinator.OnCommand(context.Background(), Command{
		Command: "/diagnose", Text: "support-agent staging", ChannelID: "C1", UserID: "U1",
	})

	rca.mu.Lock()
	defer rca.mu.Unlock()
	if rca.diagnosed[0].Environment != "staging" {
		t.Errorf("environment = %q, want the explicit one", rca.diagnosed[0].Environment)
	}
}

func TestDiagnoseCommandWithoutAServiceExplainsUsage(t *testing.T) {
	rca := &fakeRCA{diagnosis: diagnosedFixture()}
	f := newFixture(t, rca)

	f.coordinator.OnCommand(context.Background(), Command{
		Command: "/diagnose", Text: "   ", ChannelID: "C1", UserID: "U1",
	})

	if reply := f.lastPosted(t); !strings.Contains(reply, "Usage") {
		t.Errorf("reply = %q, want usage guidance", reply)
	}
	rca.mu.Lock()
	defer rca.mu.Unlock()
	if len(rca.diagnosed) != 0 {
		t.Error("an empty command still paid for an analysis run")
	}
}

func TestDiagnoseCommandWithoutAnEngineSaysSo(t *testing.T) {
	f := newFixture(t, UnavailableRCA{})

	f.coordinator.OnCommand(context.Background(), Command{
		Command: "/diagnose", Text: "support-agent", ChannelID: "C1", UserID: "U1",
	})

	if reply := f.lastPosted(t); !strings.Contains(reply, "not connected") {
		t.Errorf("reply = %q", reply)
	}
	if f.sessions.Len() != 0 {
		t.Error("a failed analysis still opened a session")
	}
}

// An alert storm must not turn into a storm of paid analysis.
func TestAnalysisConcurrencyIsCapped(t *testing.T) {
	rca := &fakeRCA{diagnosis: diagnosedFixture(), block: make(chan struct{})}
	f := newFixture(t, rca, WithMaxConcurrentAnalysis(1))

	started := make(chan struct{})
	go func() {
		close(started)
		f.coordinator.OnCommand(context.Background(), Command{
			Command: "/diagnose", Text: "support-agent", ChannelID: "C1", UserID: "U1",
		})
	}()
	<-started

	// Wait until the first analysis is genuinely in flight.
	deadline := time.Now().Add(2 * time.Second)
	for {
		rca.mu.Lock()
		inFlight := len(rca.diagnosed)
		rca.mu.Unlock()
		if inFlight == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first analysis never started")
		}
		time.Sleep(time.Millisecond)
	}

	f.coordinator.OnCommand(context.Background(), Command{
		Command: "/diagnose", Text: "checkout-api", ChannelID: "C1", UserID: "U2",
	})

	if reply := f.lastPosted(t); !strings.Contains(reply, "Too many analyses") {
		t.Errorf("reply = %q, want the saturation stated plainly", reply)
	}
	rca.mu.Lock()
	runs := len(rca.diagnosed)
	rca.mu.Unlock()
	if runs != 1 {
		t.Errorf("analysis runs = %d, want the cap to have held at 1", runs)
	}
	close(rca.block)
}

// Button clicks are cheap and must not be blocked by a saturated analysis
// budget: a decision has to be possible even when the system is busy.
func TestDecisionsAreNotBlockedByTheAnalysisCap(t *testing.T) {
	rca := &fakeRCA{diagnosis: diagnosedFixture()}
	f := newFixture(t, rca, WithMaxConcurrentAnalysis(1))
	opened := announceFixture(t, f)

	// Saturate the budget.
	f.coordinator.analysis <- struct{}{}

	f.coordinator.OnInteraction(context.Background(), Interaction{
		ActionID: ActionApprove, ChannelID: opened.ChannelID,
		ThreadTS: opened.ThreadTS, MessageTS: opened.ThreadTS, UserID: "U1",
	})

	if opened.Decision() == nil {
		t.Error("a decision was refused because analysis was busy")
	}
}

func TestReapIdleAnnouncesExpiryInTheThread(t *testing.T) {
	poster := &fakePoster{}
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	client, err := New(testConfig(), WithPoster(poster), WithLogger(logger))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	now := time.Now()
	sessions := session.NewManager(
		session.WithClock(func() time.Time { return now }),
		session.WithTTL(time.Minute),
	)
	coordinator, err := NewCoordinator(client, sessions, &fakeRCA{}, WithCoordinatorLogger(logger))
	if err != nil {
		t.Fatalf("NewCoordinator() error = %v", err)
	}

	opened, err := coordinator.Announce(context.Background(), diagnosedFixture())
	if err != nil {
		t.Fatalf("Announce() error = %v", err)
	}

	now = now.Add(2 * time.Minute)
	coordinator.ReapIdle(context.Background())

	if opened.Status() != session.StatusExpired {
		t.Errorf("status = %q, want expired", opened.Status())
	}
	poster.mu.Lock()
	defer poster.mu.Unlock()
	last := poster.calls[len(poster.calls)-1]
	text := optionText(t, last.options)
	if !strings.Contains(text, "inactivity") {
		t.Errorf("notice = %q, want the inactivity reason", text)
	}
	// Ending a conversation does not stop an alert; the notice must not
	// imply that it does.
	if !strings.Contains(text, "does not silence") {
		t.Errorf("notice = %q, want the alert caveat", text)
	}
	if optionThreadTS(t, last.options) != opened.ThreadTS {
		t.Error("the expiry notice was not posted in the session's thread")
	}
}

// A failure to post must not corrupt the recorded state: the decision is
// already in the session and the audit log.
func TestReplyFailureDoesNotUndoTheDecision(t *testing.T) {
	f := newFixture(t, &fakeRCA{})
	opened := announceFixture(t, f)

	f.poster.mu.Lock()
	f.poster.results = []fakeResult{{err: errors.New("channel_not_found")}}
	f.poster.updateErr = errors.New("channel_not_found")
	f.poster.mu.Unlock()

	f.coordinator.OnInteraction(context.Background(), Interaction{
		ActionID: ActionApprove, ChannelID: opened.ChannelID,
		ThreadTS: opened.ThreadTS, MessageTS: opened.ThreadTS, UserID: "U1",
	})

	if opened.Decision() == nil {
		t.Error("the decision was lost because Slack refused the reply")
	}
	if !strings.Contains(f.logs.String(), "could not post a reply") {
		t.Error("the failed reply was not logged")
	}
}

func TestNewCoordinatorValidatesItsInputs(t *testing.T) {
	client, err := New(testConfig(), WithPoster(&fakePoster{}))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	sessions := session.NewManager()

	if _, err := NewCoordinator(nil, sessions, &fakeRCA{}); err == nil {
		t.Error("NewCoordinator() with no client returned no error")
	}
	if _, err := NewCoordinator(client, nil, &fakeRCA{}); err == nil {
		t.Error("NewCoordinator() with no session manager returned no error")
	}
	// A missing engine is substituted rather than rejected, so every call path
	// has an explicit, honest behaviour instead of a nil check.
	coordinator, err := NewCoordinator(client, sessions, nil)
	if err != nil {
		t.Fatalf("NewCoordinator() error = %v", err)
	}
	if _, ok := coordinator.rca.(UnavailableRCA); !ok {
		t.Errorf("rca = %T, want the honest stub", coordinator.rca)
	}
}

func TestParseDiagnoseArgs(t *testing.T) {
	cases := []struct {
		text        string
		wantService string
		wantEnv     string
	}{
		{"", "", "prod"},
		{"support-agent", "support-agent", "prod"},
		{"  support-agent  staging ", "support-agent", "staging"},
		{"support-agent staging extra", "support-agent", "staging"},
	}
	for _, tc := range cases {
		service, environment := parseDiagnoseArgs(tc.text, "prod")
		if service != tc.wantService || environment != tc.wantEnv {
			t.Errorf("parseDiagnoseArgs(%q) = %q/%q, want %q/%q",
				tc.text, service, environment, tc.wantService, tc.wantEnv)
		}
	}
}

// Concurrent turns in one thread are serialised, so the conversation history
// cannot be corrupted by two half-finished updates.
func TestConcurrentTurnsInOneThreadAreSerialised(t *testing.T) {
	f := newFixture(t, &fakeRCA{answer: "answer"})
	opened := announceFixture(t, f)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f.coordinator.OnMessage(context.Background(), Message{
				ChannelID: opened.ChannelID, ThreadTS: opened.ThreadTS,
				MessageTS: "1720000100.0002", UserID: "U1",
				Text: "question " + string(rune('a'+i)),
			})
		}(i)
	}
	wg.Wait()

	if got := len(opened.History()); got != 16 {
		t.Errorf("history = %d turns, want 8 questions and 8 answers", got)
	}
}

// fakeVerifier stands in for the deterministic SLO re-evaluation engine.
type fakeVerifier struct {
	result VerifyResult
	err    error

	mu       sync.Mutex
	requests []VerifyRequest
}

func (f *fakeVerifier) Verify(_ context.Context, req VerifyRequest) (VerifyResult, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if f.err != nil {
		return VerifyResult{}, f.err
	}
	return f.result, nil
}

// An approval must offer a way to check recovery, and clicking it must
// re-evaluate the SLO the incident was grounded on - not some other one.
func TestApproveThenVerify_ChecksTheGroundedSLO(t *testing.T) {
	verifier := &fakeVerifier{result: VerifyResult{SLOState: "healthy", BurnRate: 0.4, ErrorBudgetRemaining: 0.8, TelemetryTrusted: true, Recovered: true}}
	f := newFixture(t, &fakeRCA{}, WithCoordinatorVerifier(verifier))
	opened := announceFixture(t, f)

	f.coordinator.OnInteraction(context.Background(), Interaction{
		ActionID: ActionApprove, ChannelID: opened.ChannelID,
		ThreadTS: opened.ThreadTS, MessageTS: opened.ThreadTS, UserID: "U1",
	})
	f.coordinator.OnInteraction(context.Background(), Interaction{
		ActionID: ActionVerify, Value: "corr-123", ChannelID: opened.ChannelID,
		ThreadTS: opened.ThreadTS, MessageTS: opened.ThreadTS, UserID: "U1",
	})

	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	if len(verifier.requests) != 1 {
		t.Fatalf("verify calls = %d, want 1", len(verifier.requests))
	}
	req := verifier.requests[0]
	if req.Service != "support-agent" || req.Environment != "prod" || req.SLO != "agent-success-rate" {
		t.Errorf("VerifyRequest = %+v, want it scoped to the diagnosis's own service/environment/SLO", req)
	}

	if reply := f.lastPosted(t); !strings.Contains(reply, "recovered") {
		t.Errorf("reply = %q, want it to report recovery", reply)
	}
}

// Declining takes no action, so there is nothing to go verify - the button
// must not appear, and clicking a stale verify action id must not crash if
// it somehow arrives anyway.
func TestDecline_DoesNotOfferVerify(t *testing.T) {
	verifier := &fakeVerifier{}
	f := newFixture(t, &fakeRCA{}, WithCoordinatorVerifier(verifier))
	opened := announceFixture(t, f)

	f.coordinator.OnInteraction(context.Background(), Interaction{
		ActionID: ActionDecline, ChannelID: opened.ChannelID,
		ThreadTS: opened.ThreadTS, MessageTS: opened.ThreadTS, UserID: "U1",
	})

	f.poster.mu.Lock()
	update := f.poster.updates[len(f.poster.updates)-1]
	f.poster.mu.Unlock()
	_, values, err := slack.UnsafeApplyMsgOptions("xoxb-test", "C1", "http://localhost/", update.options...)
	if err != nil {
		t.Fatalf("apply msg options: %v", err)
	}
	if strings.Contains(values.Get("blocks"), ActionVerify) {
		t.Error("a declined incident's retired message still offers a Verify button")
	}

	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	if len(verifier.requests) != 0 {
		t.Error("declining triggered a verify call")
	}
}

// Verify can be clicked more than once while a human waits for a fix to
// take effect, and it must never touch the recorded decision.
func TestVerify_CanBeCheckedMultipleTimesWithoutAffectingTheDecision(t *testing.T) {
	verifier := &fakeVerifier{result: VerifyResult{SLOState: "unhealthy", BurnRate: 3.0, TelemetryTrusted: true, Recovered: false}}
	f := newFixture(t, &fakeRCA{}, WithCoordinatorVerifier(verifier))
	opened := announceFixture(t, f)

	f.coordinator.OnInteraction(context.Background(), Interaction{
		ActionID: ActionApprove, ChannelID: opened.ChannelID,
		ThreadTS: opened.ThreadTS, MessageTS: opened.ThreadTS, UserID: "U1",
	})
	for i := 0; i < 3; i++ {
		f.coordinator.OnInteraction(context.Background(), Interaction{
			ActionID: ActionVerify, Value: "corr-123", ChannelID: opened.ChannelID,
			ThreadTS: opened.ThreadTS, MessageTS: opened.ThreadTS, UserID: "U1",
		})
	}

	verifier.mu.Lock()
	calls := len(verifier.requests)
	verifier.mu.Unlock()
	if calls != 3 {
		t.Errorf("verify calls = %d, want 3 - each click should re-check", calls)
	}
	if reply := f.lastPosted(t); !strings.Contains(reply, "not yet recovered") {
		t.Errorf("reply = %q, want it to report the still-unhealthy state", reply)
	}
	if opened.Decision().Kind != session.DecisionApproved {
		t.Error("verify changed the recorded decision")
	}
}

func TestVerify_WithoutAVerifierRefusesCleanly(t *testing.T) {
	f := newFixture(t, &fakeRCA{}) // no WithCoordinatorVerifier: defaults to UnavailableVerifier
	opened := announceFixture(t, f)

	f.coordinator.OnInteraction(context.Background(), Interaction{
		ActionID: ActionApprove, ChannelID: opened.ChannelID,
		ThreadTS: opened.ThreadTS, MessageTS: opened.ThreadTS, UserID: "U1",
	})
	f.coordinator.OnInteraction(context.Background(), Interaction{
		ActionID: ActionVerify, Value: "corr-123", ChannelID: opened.ChannelID,
		ThreadTS: opened.ThreadTS, MessageTS: opened.ThreadTS, UserID: "U1",
	})

	if reply := f.lastPosted(t); !strings.Contains(reply, "not connected") {
		t.Errorf("reply = %q, want it to say the verify engine is not connected", reply)
	}
}

func TestVerify_UntrustedTelemetryIsReportedAsIndeterminate(t *testing.T) {
	verifier := &fakeVerifier{result: VerifyResult{SLOState: "indeterminate", TelemetryTrusted: false}}
	f := newFixture(t, &fakeRCA{}, WithCoordinatorVerifier(verifier))
	opened := announceFixture(t, f)

	f.coordinator.OnInteraction(context.Background(), Interaction{
		ActionID: ActionApprove, ChannelID: opened.ChannelID,
		ThreadTS: opened.ThreadTS, MessageTS: opened.ThreadTS, UserID: "U1",
	})
	f.coordinator.OnInteraction(context.Background(), Interaction{
		ActionID: ActionVerify, Value: "corr-123", ChannelID: opened.ChannelID,
		ThreadTS: opened.ThreadTS, MessageTS: opened.ThreadTS, UserID: "U1",
	})

	if reply := f.lastPosted(t); !strings.Contains(reply, "indeterminate") {
		t.Errorf("reply = %q, want it to say recovery is indeterminate rather than claim recovery or non-recovery", reply)
	}
}
