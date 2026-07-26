package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/config"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
	sidekickslack "github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify/slack"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/session"
)

// This file exercises the Slack adapter end to end with every real component
// wired together - the socket receiver, the coordinator, the session store and
// a real slack-go client - against a stand-in Slack API served over httptest.
//
// Unit tests check each piece against a fake of its neighbour, which means
// they cannot catch a seam that is wrong on both sides. That is not
// hypothetical here: the receiver's ack interface once omitted the error that
// socketmode.Client.Ack returns, so the real client never satisfied it, and
// every unit test still passed. These tests exist for that class of bug.

// fakeSlack stands in for Slack's Web API.
type fakeSlack struct {
	server *httptest.Server

	mu       sync.Mutex
	posts    []slackCall
	updates  []slackCall
	replies  []slackapi.Message
	nextTS   int
	postErr  string
	threadOf map[string]string
}

type slackCall struct {
	channel  string
	text     string
	blocks   string
	threadTS string
	ts       string
}

func newFakeSlack(t *testing.T) *fakeSlack {
	t.Helper()

	fake := &fakeSlack{threadOf: map[string]string{}}
	mux := http.NewServeMux()

	mux.HandleFunc("/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		form := readForm(t, r)

		fake.mu.Lock()
		fake.nextTS++
		timestamp := fmt.Sprintf("1720000%03d.000100", fake.nextTS)
		fake.posts = append(fake.posts, slackCall{
			channel:  form.Get("channel"),
			text:     form.Get("text"),
			blocks:   form.Get("blocks"),
			threadTS: form.Get("thread_ts"),
			ts:       timestamp,
		})
		failure := fake.postErr
		fake.mu.Unlock()

		if failure != "" {
			writeJSON(w, map[string]any{"ok": false, "error": failure})
			return
		}
		writeJSON(w, map[string]any{
			"ok": true, "channel": form.Get("channel"), "ts": timestamp,
		})
	})

	mux.HandleFunc("/chat.update", func(w http.ResponseWriter, r *http.Request) {
		form := readForm(t, r)

		fake.mu.Lock()
		fake.updates = append(fake.updates, slackCall{
			channel: form.Get("channel"),
			text:    form.Get("text"),
			blocks:  form.Get("blocks"),
			ts:      form.Get("ts"),
		})
		fake.mu.Unlock()

		writeJSON(w, map[string]any{
			"ok": true, "channel": form.Get("channel"), "ts": form.Get("ts"),
		})
	})

	mux.HandleFunc("/conversations.replies", func(w http.ResponseWriter, r *http.Request) {
		readForm(t, r)

		fake.mu.Lock()
		messages := fake.replies
		fake.mu.Unlock()

		writeJSON(w, map[string]any{"ok": true, "messages": messages})
	})

	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func readForm(t *testing.T, r *http.Request) url.Values {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		t.Fatalf("parse request body: %v", err)
	}
	return values
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func (f *fakeSlack) postCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts)
}

func (f *fakeSlack) post(t *testing.T, index int) slackCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if index >= len(f.posts) {
		t.Fatalf("message %d was not posted; %d were", index, len(f.posts))
	}
	return f.posts[index]
}

func (f *fakeSlack) lastPost(t *testing.T) slackCall {
	t.Helper()
	f.mu.Lock()
	count := len(f.posts)
	f.mu.Unlock()
	if count == 0 {
		t.Fatal("nothing was posted")
	}
	return f.post(t, count-1)
}

func (f *fakeSlack) allText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for _, call := range f.posts {
		b.WriteString(call.text)
		b.WriteString("\n")
	}
	return b.String()
}

// stubRCA is the analysis engine, which lives behind an interface and in
// another branch.
type stubRCA struct {
	mu        sync.Mutex
	answer    string
	diagnosis notify.Diagnosis
	requests  []sidekickslack.FollowupRequest
}

func (s *stubRCA) Diagnose(
	context.Context, sidekickslack.DiagnoseRequest,
) (notify.Diagnosis, error) {
	return s.diagnosis, nil
}

func (s *stubRCA) AnswerFollowup(
	_ context.Context, req sidekickslack.FollowupRequest,
) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, req)
	return s.answer, nil
}

func (s *stubRCA) lastRequest(t *testing.T) sidekickslack.FollowupRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		t.Fatal("the analysis engine was never asked anything")
	}
	return s.requests[len(s.requests)-1]
}

type ackRecorder struct {
	mu    sync.Mutex
	acked []string
}

func (a *ackRecorder) Ack(req socketmode.Request, _ ...any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.acked = append(a.acked, req.EnvelopeID)
	return nil
}

func (a *ackRecorder) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.acked)
}

// slackStack is every real component of the adapter, wired the way `watch`
// wires them.
type slackStack struct {
	slack       *fakeSlack
	rca         *stubRCA
	sessions    *session.Manager
	coordinator *sidekickslack.Coordinator
	events      chan socketmode.Event
	acks        *ackRecorder
	now         func() time.Time
}

func newSlackStack(t *testing.T, opts ...session.ManagerOption) *slackStack {
	t.Helper()

	fake := newFakeSlack(t)
	rca := &stubRCA{answer: "78% of the failing spans are TimeoutError from tool.search_kb."}

	api := slackapi.New("xoxb-test", slackapi.OptionAPIURL(fake.server.URL+"/"))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	client, err := sidekickslack.New(config.SlackConfig{
		BotTokenEnv:        "SLACK_BOT_TOKEN",
		AppTokenEnv:        "SLACK_APP_TOKEN",
		DefaultChannel:     "C0SRE",
		DefaultEnvironment: "prod",
		SessionTTL:         "30m",
		MaxConcurrentRCA:   5,
	}, sidekickslack.WithPoster(api), sidekickslack.WithLogger(logger))
	if err != nil {
		t.Fatalf("slack.New() error = %v", err)
	}

	sessions := session.NewManager(opts...)
	coordinator, err := sidekickslack.NewCoordinator(client, sessions, rca,
		sidekickslack.WithCoordinatorLogger(logger),
		sidekickslack.WithDefaultEnvironment("prod"),
	)
	if err != nil {
		t.Fatalf("NewCoordinator() error = %v", err)
	}

	events := make(chan socketmode.Event, 16)
	acks := &ackRecorder{}
	receiver, err := sidekickslack.NewReceiver(events, acks, coordinator,
		sidekickslack.WithReceiverLogger(logger))
	if err != nil {
		t.Fatalf("NewReceiver() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = receiver.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(3 * time.Second):
			t.Error("the receiver did not stop")
		}
	})

	return &slackStack{
		slack: fake, rca: rca, sessions: sessions,
		coordinator: coordinator, events: events, acks: acks,
	}
}

func diagnosisFor(service string) notify.Diagnosis {
	return notify.Diagnosis{
		CorrelationID: "corr-" + service,
		Service:       service,
		Environment:   "prod",
		Window:        "30d",
		Status:        notify.StatusDiagnosed,
		Grounding: notify.Grounding{
			Environment:          "prod",
			SLO:                  "agent-success-rate",
			SLOState:             "unhealthy",
			BurnRate:             14.2,
			ErrorBudgetRemaining: -0.034,
			TelemetryTrusted:     true,
		},
		RootCause:   "tool.search_kb times out after the 2s client deadline",
		ProposedFix: "raise the search_kb client timeout to 5s",
		Reversible:  true,
		Evidence: []notify.Evidence{{
			ID: "e1", Kind: notify.EvidenceKindTrace,
			SignozLink: "https://signoz.example/trace/abc",
			Note:       "78% of failing spans are TimeoutError",
		}},
		Timestamp: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
	}
}

func waitForCondition(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func threadedMessage(envelopeID, eventID, channel, threadTS, user, text string) socketmode.Event {
	return socketmode.Event{
		Type: socketmode.EventTypeEventsAPI,
		Data: slackevents.EventsAPIEvent{
			TeamID: "T1",
			Data:   slackevents.EventsAPICallbackEvent{EventID: eventID},
			InnerEvent: slackevents.EventsAPIInnerEvent{Data: &slackevents.MessageEvent{
				Channel:         channel,
				ThreadTimeStamp: threadTS,
				TimeStamp:       threadTS + "9",
				User:            user,
				Text:            text,
			}},
		},
		Request: &socketmode.Request{EnvelopeID: envelopeID},
	}
}

func buttonClick(envelopeID, actionID, channel, messageTS, user string) socketmode.Event {
	callback := slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeBlockActions,
		User: slackapi.User{ID: user, Name: "priya"},
		Team: slackapi.Team{ID: "T1"},
	}
	callback.Channel.ID = channel
	callback.Container.MessageTs = messageTS
	callback.Message.Timestamp = messageTS
	callback.ActionCallback.BlockActions = []*slackapi.BlockAction{
		{ActionID: actionID, Value: "corr-support-agent"},
	}
	return socketmode.Event{
		Type:    socketmode.EventTypeInteractive,
		Data:    callback,
		Request: &socketmode.Request{EnvelopeID: envelopeID},
	}
}

// Scenario 1 (PRD section 23): a healthy service produces no message at all.
// The adapter only speaks when it is given something to say.
func TestSlackFlowHealthyRunSaysNothing(t *testing.T) {
	stack := newSlackStack(t)

	if stack.slack.postCount() != 0 {
		t.Errorf("posts = %d, want silence when nothing was diagnosed", stack.slack.postCount())
	}
	if stack.sessions.Len() != 0 {
		t.Errorf("sessions = %d, want none", stack.sessions.Len())
	}
}

// Scenario 2: telemetry is not trusted, so the message states no cause and
// offers nothing to approve.
func TestSlackFlowIndeterminateOffersNoDecision(t *testing.T) {
	stack := newSlackStack(t)

	indeterminate := diagnosisFor("support-agent")
	indeterminate.Status = notify.StatusIndeterminate
	indeterminate.RootCause = ""
	indeterminate.ProposedFix = ""
	indeterminate.Grounding.TelemetryTrusted = false
	indeterminate.Grounding.SLOState = "indeterminate"
	indeterminate.MissingEvidence = []string{"agent_success_total missing for 30d"}

	if _, err := stack.coordinator.Announce(context.Background(), indeterminate); err != nil {
		t.Fatalf("Announce() error = %v", err)
	}

	posted := stack.slack.post(t, 0)
	if !strings.Contains(posted.text, "telemetry incomplete") {
		t.Errorf("fallback text = %q, want the incompleteness stated", posted.text)
	}
	if strings.Contains(posted.blocks, "Root cause") {
		t.Error("an indeterminate message stated a root cause")
	}
	if strings.Contains(posted.blocks, sidekickslack.ActionApprove) {
		t.Error("an indeterminate message offered an approve button")
	}
	if !strings.Contains(posted.blocks, sidekickslack.ActionClose) {
		t.Error("an indeterminate message offered no way to close the session")
	}
	if !strings.Contains(posted.blocks, "agent_success_total") {
		t.Error("the missing evidence was not listed")
	}
}

// Scenario 3: the demo path. Alert to diagnosis to approval, with the decision
// recorded, the buttons retired, and nothing executed.
func TestSlackFlowDiagnoseThenApprove(t *testing.T) {
	stack := newSlackStack(t)

	opened, err := stack.coordinator.Announce(context.Background(), diagnosisFor("support-agent"))
	if err != nil {
		t.Fatalf("Announce() error = %v", err)
	}

	posted := stack.slack.post(t, 0)
	// The session key is the timestamp Slack returned for the message just
	// posted: one incident, one thread, one session.
	if opened.ThreadTS != posted.ts {
		t.Fatalf("session thread = %q, want the posted message ts %q", opened.ThreadTS, posted.ts)
	}
	if !strings.Contains(posted.blocks, "14.2x") || !strings.Contains(posted.blocks, "-3.4%") {
		t.Errorf("the grounded facts are missing from the message: %s", posted.blocks)
	}
	for _, action := range []string{
		sidekickslack.ActionApprove, sidekickslack.ActionDecline, sidekickslack.ActionClose,
	} {
		if !strings.Contains(posted.blocks, action) {
			t.Errorf("the message is missing the %q button", action)
		}
	}

	stack.events <- buttonClick("env-1", sidekickslack.ActionApprove, "C0SRE", posted.ts, "U1")
	waitForCondition(t, "the approval to be recorded", func() bool {
		return opened.Decision() != nil
	})

	decision := opened.Decision()
	if decision.Kind != session.DecisionApproved || decision.UserID != "U1" {
		t.Errorf("decision = %+v, want an approval by U1", decision)
	}
	if opened.Status() != session.StatusResolved {
		t.Errorf("status = %q, want resolved", opened.Status())
	}

	// The buttons are retired so a decided incident shows no live control.
	waitForCondition(t, "the message to be rewritten", func() bool {
		stack.slack.mu.Lock()
		defer stack.slack.mu.Unlock()
		return len(stack.slack.updates) == 1
	})
	stack.slack.mu.Lock()
	update := stack.slack.updates[0]
	stack.slack.mu.Unlock()

	if update.ts != posted.ts {
		t.Errorf("updated ts = %q, want the diagnosis message %q", update.ts, posted.ts)
	}
	if strings.Contains(update.blocks, sidekickslack.ActionApprove) {
		t.Error("the approve button survived the decision")
	}
	if !strings.Contains(update.blocks, "U1") {
		t.Error("the rewritten message does not name who decided")
	}
	// The grounded facts must survive the rewrite.
	if !strings.Contains(update.blocks, "14.2x") {
		t.Error("the rewritten message lost the grounding")
	}

	// Advisory only: the confirmation must not imply anything ran.
	waitForCondition(t, "the confirmation reply", func() bool {
		return strings.Contains(stack.slack.allText(), "Nothing was executed")
	})
	if stack.acks.count() == 0 {
		t.Error("the envelope was never acknowledged")
	}
}

// Scenario 4: the same alert firing again lands in the existing thread, and
// does not open a second session or pay for a second analysis.
func TestSlackFlowReFiredAlertReusesTheThread(t *testing.T) {
	stack := newSlackStack(t)
	ctx := context.Background()

	first, err := stack.coordinator.Announce(ctx, diagnosisFor("support-agent"))
	if err != nil {
		t.Fatalf("Announce() error = %v", err)
	}

	again := diagnosisFor("support-agent")
	again.CorrelationID = "corr-second-firing"
	second, err := stack.coordinator.Announce(ctx, again)
	if err != nil {
		t.Fatalf("Announce() error = %v", err)
	}

	if second != first {
		t.Fatal("the re-fire opened a second session")
	}
	if stack.sessions.Len() != 1 {
		t.Errorf("sessions = %d, want 1", stack.sessions.Len())
	}
	notice := stack.slack.lastPost(t)
	if notice.threadTS != first.ThreadTS {
		t.Errorf("the re-fire notice went to thread %q, want %q", notice.threadTS, first.ThreadTS)
	}
	if !strings.Contains(notice.text, "fired again") {
		t.Errorf("re-fire notice = %q", notice.text)
	}
	// The frozen diagnosis is not overwritten by a later firing.
	if first.Diagnosis().CorrelationID != "corr-support-agent" {
		t.Error("the re-fire overwrote the frozen diagnosis")
	}
}

// Scenario 5: a threaded question is answered in context, with the
// deterministic facts pinned from the original diagnosis.
func TestSlackFlowFollowupQuestion(t *testing.T) {
	stack := newSlackStack(t)

	opened, err := stack.coordinator.Announce(context.Background(), diagnosisFor("support-agent"))
	if err != nil {
		t.Fatalf("Announce() error = %v", err)
	}
	posted := stack.slack.post(t, 0)

	stack.events <- threadedMessage(
		"env-q1", "Ev-q1", "C0SRE", posted.ts, "U1", "why do you think it is the timeout?")

	waitForCondition(t, "the answer to be posted", func() bool {
		return strings.Contains(stack.slack.allText(), "TimeoutError")
	})

	req := stack.rca.lastRequest(t)
	if req.Question != "why do you think it is the timeout?" {
		t.Errorf("question = %q", req.Question)
	}
	// Pinned, not regenerated: the numbers come from the frozen diagnosis.
	if req.Diagnosis.Grounding.BurnRate != 14.2 {
		t.Errorf("burn rate = %v, want the frozen 14.2", req.Diagnosis.Grounding.BurnRate)
	}
	if req.CorrelationID != "corr-support-agent" {
		t.Errorf("correlation id = %q", req.CorrelationID)
	}

	answer := stack.slack.lastPost(t)
	if answer.threadTS != opened.ThreadTS {
		t.Errorf("the answer went to thread %q, want %q", answer.threadTS, opened.ThreadTS)
	}
	// Both sides of the exchange are on the record for the next turn.
	waitForCondition(t, "the exchange to be recorded", func() bool {
		return len(opened.History()) == 2
	})

	// A second question carries the first exchange as history.
	stack.events <- threadedMessage(
		"env-q2", "Ev-q2", "C0SRE", posted.ts, "U2", "could it be the KB service instead?")
	waitForCondition(t, "the second answer", func() bool {
		return len(opened.History()) == 4
	})

	second := stack.rca.lastRequest(t)
	if len(second.History) != 2 {
		t.Errorf("history = %d turns, want the first exchange", len(second.History))
	}
	if second.Question != "could it be the KB service instead?" {
		t.Errorf("question = %q", second.Question)
	}
	// Incident response is a team activity; everyone who spoke is recorded.
	if len(opened.Participants()) != 2 {
		t.Errorf("participants = %v, want both humans", opened.Participants())
	}
}

// Typing "approve" must point at the button rather than record a decision:
// only a button click, which carries a verified identity, decides.
func TestSlackFlowTypedApprovalDoesNotDecide(t *testing.T) {
	stack := newSlackStack(t)

	opened, err := stack.coordinator.Announce(context.Background(), diagnosisFor("support-agent"))
	if err != nil {
		t.Fatalf("Announce() error = %v", err)
	}
	posted := stack.slack.post(t, 0)

	stack.events <- threadedMessage("env-a", "Ev-a", "C0SRE", posted.ts, "U1", "approve")

	waitForCondition(t, "the nudge", func() bool {
		return strings.Contains(stack.slack.allText(), "use the buttons")
	})
	if opened.Decision() != nil {
		t.Fatal("typed text recorded a decision")
	}
	if opened.Status() != session.StatusOpen {
		t.Errorf("status = %q, want the session still open", opened.Status())
	}
}

// Scenario 6: after a restart the session store is empty. A human replying in
// an old thread gets an explanation rather than silence.
func TestSlackFlowRestartExplainsTheLostSession(t *testing.T) {
	stack := newSlackStack(t)

	if _, err := stack.coordinator.Announce(context.Background(), diagnosisFor("support-agent")); err != nil {
		t.Fatalf("Announce() error = %v", err)
	}
	posted := stack.slack.post(t, 0)

	// Stand in for a restart: a brand new stack with no memory of the thread,
	// while Slack still has the thread and its bot-authored root message.
	restarted := newSlackStack(t)

	botRoot := slackapi.Message{}
	botRoot.BotID = "B1"
	restarted.slack.mu.Lock()
	restarted.slack.replies = []slackapi.Message{botRoot}
	restarted.slack.mu.Unlock()

	restarted.coordinator.OnMessage(context.Background(), sidekickslack.Message{
		ChannelID: "C0SRE",
		ThreadTS:  posted.ts,
		MessageTS: posted.ts + "9",
		UserID:    "U1",
		Text:      "are you still looking at this?",
	})

	waitForCondition(t, "the lost-session notice", func() bool {
		return strings.Contains(restarted.slack.allText(), "/diagnose")
	})

	// A thread the sidekick did not start must not be answered at all: the bot
	// never barges into someone else's conversation.
	quiet := newSlackStack(t)
	quiet.slack.mu.Lock()
	quiet.slack.replies = []slackapi.Message{{}} // human-rooted thread
	quiet.slack.mu.Unlock()

	quiet.coordinator.OnMessage(context.Background(), sidekickslack.Message{
		ChannelID: "C0SRE", ThreadTS: posted.ts, MessageTS: posted.ts + "9",
		UserID: "U1", Text: "unrelated conversation",
	})
	if quiet.slack.postCount() != 0 {
		t.Errorf("the bot spoke in a thread it did not start: %q", quiet.slack.allText())
	}
}

// Scenario 7: an abandoned session is closed by the reaper, which says so in
// the thread and is careful not to imply the alert was silenced.
func TestSlackFlowIdleSessionExpires(t *testing.T) {
	now := time.Now()
	stack := newSlackStack(t,
		session.WithClock(func() time.Time { return now }),
		session.WithTTL(30*time.Minute),
	)

	opened, err := stack.coordinator.Announce(context.Background(), diagnosisFor("support-agent"))
	if err != nil {
		t.Fatalf("Announce() error = %v", err)
	}

	now = now.Add(31 * time.Minute)
	stack.coordinator.ReapIdle(context.Background())

	if opened.Status() != session.StatusExpired {
		t.Errorf("status = %q, want expired", opened.Status())
	}
	notice := stack.slack.lastPost(t)
	if notice.threadTS != opened.ThreadTS {
		t.Error("the expiry notice was not posted in the session's thread")
	}
	if !strings.Contains(notice.text, "inactivity") {
		t.Errorf("notice = %q, want the reason stated", notice.text)
	}
	if !strings.Contains(notice.text, "does not silence") {
		t.Errorf("notice = %q, want the alert caveat", notice.text)
	}

	// A message in the expired thread is answered once, not ignored.
	stack.coordinator.OnMessage(context.Background(), sidekickslack.Message{
		ChannelID: "C0SRE", ThreadTS: opened.ThreadTS,
		MessageTS: opened.ThreadTS + "9", UserID: "U1", Text: "hello?",
	})
	waitForCondition(t, "the expiry explanation", func() bool {
		return strings.Contains(stack.slack.allText(), "expired")
	})
}

// Two incidents are two threads and never interfere - the property that makes
// concurrent incidents safe.
func TestSlackFlowTwoIncidentsStayIndependent(t *testing.T) {
	stack := newSlackStack(t)
	ctx := context.Background()

	support, err := stack.coordinator.Announce(ctx, diagnosisFor("support-agent"))
	if err != nil {
		t.Fatalf("Announce() error = %v", err)
	}
	checkout, err := stack.coordinator.Announce(ctx, diagnosisFor("checkout-api"))
	if err != nil {
		t.Fatalf("Announce() error = %v", err)
	}

	if support.ThreadTS == checkout.ThreadTS {
		t.Fatal("two incidents shared one thread")
	}

	supportPost := stack.slack.post(t, 0)
	stack.events <- buttonClick("env-x", sidekickslack.ActionDecline, "C0SRE", supportPost.ts, "U1")
	waitForCondition(t, "the decline", func() bool { return support.Decision() != nil })

	if checkout.Decision() != nil {
		t.Error("a decision on one incident leaked into the other")
	}
	if checkout.Status() != session.StatusOpen {
		t.Errorf("the other incident is %q, want open", checkout.Status())
	}
}

// A double-tapped or contested decision resolves once, and the second person
// is told who decided.
func TestSlackFlowSecondDecisionIsRefused(t *testing.T) {
	stack := newSlackStack(t)

	opened, err := stack.coordinator.Announce(context.Background(), diagnosisFor("support-agent"))
	if err != nil {
		t.Fatalf("Announce() error = %v", err)
	}
	posted := stack.slack.post(t, 0)

	stack.events <- buttonClick("env-1", sidekickslack.ActionApprove, "C0SRE", posted.ts, "U1")
	waitForCondition(t, "the approval", func() bool { return opened.Decision() != nil })

	stack.events <- buttonClick("env-2", sidekickslack.ActionDecline, "C0SRE", posted.ts, "U2")
	waitForCondition(t, "the refusal", func() bool {
		return strings.Contains(stack.slack.allText(), "Already approved")
	})

	if got := opened.Decision(); got.UserID != "U1" {
		t.Errorf("decision = %+v, want the first one to stand", got)
	}
}

// A Slack outage must be reported, and must not corrupt the session state.
func TestSlackFlowPostFailureIsReported(t *testing.T) {
	stack := newSlackStack(t)

	stack.slack.mu.Lock()
	stack.slack.postErr = "channel_not_found"
	stack.slack.mu.Unlock()

	_, err := stack.coordinator.Announce(context.Background(), diagnosisFor("support-agent"))
	if err == nil {
		t.Fatal("Announce() error = nil, want the delivery failure surfaced")
	}
	if !strings.Contains(err.Error(), "channel_not_found") {
		t.Errorf("error = %q, want the Slack error carried", err)
	}
	// No thread was created, so no session may exist for it.
	if stack.sessions.Len() != 0 {
		t.Errorf("sessions = %d, want none after a failed post", stack.sessions.Len())
	}
}

// The sidekick's own posts come back as message events; acting on them would
// make it answer itself forever.
func TestSlackFlowIgnoresItsOwnMessages(t *testing.T) {
	stack := newSlackStack(t)

	if _, err := stack.coordinator.Announce(context.Background(), diagnosisFor("support-agent")); err != nil {
		t.Fatalf("Announce() error = %v", err)
	}
	posted := stack.slack.post(t, 0)
	before := stack.slack.postCount()

	botEcho := threadedMessage("env-bot", "Ev-bot", "C0SRE", posted.ts, "U1", "posted by the sidekick")
	inner := botEcho.Data.(slackevents.EventsAPIEvent).InnerEvent.Data.(*slackevents.MessageEvent)
	inner.BotID = "B1"
	stack.events <- botEcho

	waitForCondition(t, "the envelope to be acknowledged", func() bool {
		return stack.acks.count() == 1
	})
	time.Sleep(20 * time.Millisecond)

	if stack.slack.postCount() != before {
		t.Error("the sidekick replied to its own message")
	}
}

// Slack redelivers anything it believes was not handled; the same human turn
// must not be answered twice.
func TestSlackFlowRedeliveredMessageIsAnsweredOnce(t *testing.T) {
	stack := newSlackStack(t)

	if _, err := stack.coordinator.Announce(context.Background(), diagnosisFor("support-agent")); err != nil {
		t.Fatalf("Announce() error = %v", err)
	}
	posted := stack.slack.post(t, 0)

	first := threadedMessage("env-1", "Ev-dup", "C0SRE", posted.ts, "U1", "why the timeout?")
	stack.events <- first
	waitForCondition(t, "the first answer", func() bool {
		return strings.Contains(stack.slack.allText(), "TimeoutError")
	})

	retry := threadedMessage("env-1-retry", "Ev-dup", "C0SRE", posted.ts, "U1", "why the timeout?")
	retry.Request.RetryAttempt = 1
	stack.events <- retry

	waitForCondition(t, "the redelivery to be acknowledged", func() bool {
		return stack.acks.count() == 2
	})
	time.Sleep(20 * time.Millisecond)

	stack.rca.mu.Lock()
	defer stack.rca.mu.Unlock()
	if len(stack.rca.requests) != 1 {
		t.Errorf("analysis calls = %d, want the redelivery deduplicated", len(stack.rca.requests))
	}
}
