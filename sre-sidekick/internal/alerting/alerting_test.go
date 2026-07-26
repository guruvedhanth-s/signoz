package alerting

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/audit"
)

type recordingSink struct {
	calls int
	err   error
}

func (s *recordingSink) Notify(context.Context, Event) error {
	s.calls++
	return s.err
}

func TestMultiSinkAttemptsEverySinkAndJoinsErrors(t *testing.T) {
	firstErr := errors.New("first sink failed")
	lastErr := errors.New("last sink failed")
	first := &recordingSink{err: firstErr}
	middle := &recordingSink{}
	last := &recordingSink{err: lastErr}

	err := (MultiSink{first, middle, last}).Notify(context.Background(), Event{})

	if first.calls != 1 || middle.calls != 1 || last.calls != 1 {
		t.Fatalf("every sink must be attempted: first=%d middle=%d last=%d", first.calls, middle.calls, last.calls)
	}
	if !errors.Is(err, firstErr) || !errors.Is(err, lastErr) {
		t.Fatalf("joined error does not retain sink failures: %v", err)
	}
}

// slackRecorder captures what the sink posted. The handler runs on the test
// server's goroutine, where t.Fatal would only kill that goroutine and leave
// the assertion below reading a zero value, so it records and the test asserts.
type slackRecorder struct {
	contentType string
	payload     slackPayload
	decodeErr   error
	status      int
}

func (r *slackRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.contentType = req.Header.Get("Content-Type")
		r.decodeErr = json.NewDecoder(req.Body).Decode(&r.payload)
		status := r.status
		if status == 0 {
			status = http.StatusNoContent
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestSlackSinkPostsSlackTextPayload(t *testing.T) {
	recorder := &slackRecorder{}
	server := recorder.server(t)

	event := Event{
		State: "firing", Service: "checkout-api", Environment: "test",
		CurrentStatus: "fail", Message: "telemetry quality audit failed",
		Report: audit.Report{SchemaVersion: "1.0", Score: 85, Coverage: 0.9},
	}
	if err := (SlackSink{URL: server.URL}).Notify(context.Background(), event); err != nil {
		t.Fatal(err)
	}

	if recorder.decodeErr != nil {
		t.Fatalf("Slack payload is not JSON: %v", recorder.decodeErr)
	}
	if recorder.contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", recorder.contentType)
	}
	for _, want := range []string{
		"SRE Sidekick firing:", "telemetry quality audit failed",
		"checkout-api/test", "status=fail", "score=85.0", "coverage=90.0%",
	} {
		if !strings.Contains(recorder.payload.Text, want) {
			t.Errorf("Slack text %q is missing %q", recorder.payload.Text, want)
		}
	}
}

// Track A events always carry a report, but sinks are shared: any event built
// without one would otherwise announce "score=0.0, coverage=0.0%", which reads
// as a measured zero rather than an absent measurement.
func TestSlackTextOmitsScoreWhenTheEventCarriesNoReport(t *testing.T) {
	text := slackText(Event{State: "resolved", Service: "checkout-api", Message: "recovered"})

	if strings.Contains(text, "score=") || strings.Contains(text, "coverage=") {
		t.Fatalf("Slack text must not report an absent audit report: %q", text)
	}
	if !strings.Contains(text, "SRE Sidekick resolved: recovered") {
		t.Fatalf("unexpected Slack text: %q", text)
	}
}

func TestSlackSinkReportsNon2xxAndSkipsAnEmptyURL(t *testing.T) {
	recorder := &slackRecorder{status: http.StatusForbidden}
	server := recorder.server(t)

	err := (SlackSink{URL: server.URL}).Notify(context.Background(), Event{Message: "denied"})
	if err == nil {
		t.Fatal("a non-2xx Slack response must surface as an error")
	}
	if strings.Contains(err.Error(), server.URL) {
		t.Errorf("error must not echo the webhook URL, which embeds the secret: %v", err)
	}

	if err := (SlackSink{}).Notify(context.Background(), Event{Message: "unconfigured"}); err != nil {
		t.Fatalf("an empty URL must be a no-op, got %v", err)
	}
}
