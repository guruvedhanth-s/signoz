package alerting

import (
	"bytes"
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

func TestSlackSinkSendsSlackPayload(t *testing.T) {
	var payload slackPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("unexpected content type: %s", r.Header.Get("Content-Type"))
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	event := Event{
		State: "firing", Service: "checkout-api", Environment: "test",
		CurrentStatus: "fail", Message: "telemetry quality audit failed",
		Report: audit.Report{Score: 85, Coverage: 0.9},
	}
	if err := (SlackSink{URL: server.URL}).Notify(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(payload.Text, "SRE Sidekick firing:") ||
		!strings.Contains(payload.Text, "checkout-api/test") ||
		!strings.Contains(payload.Text, "score=85.0") {
		t.Fatalf("unexpected Slack payload: %q", payload.Text)
	}
}

func TestSlackTextIsValidJSONPayload(t *testing.T) {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(slackPayload{Text: slackText(Event{Message: "recovered"})}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.String(), `"text"`) {
		t.Fatalf("missing Slack text payload: %s", body.String())
	}
}
