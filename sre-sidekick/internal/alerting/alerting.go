package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/audit"
)

type Event struct {
	Kind           string       `json:"kind"`
	State          string       `json:"state"`
	Service        string       `json:"service"`
	Environment    string       `json:"environment"`
	PreviousStatus audit.Status `json:"previous_status"`
	CurrentStatus  audit.Status `json:"current_status"`
	ObservedAt     time.Time    `json:"observed_at"`
	Message        string       `json:"message"`
	Report         audit.Report `json:"report"`
}

type Sink interface {
	Notify(context.Context, Event) error
}

type JSONSink struct {
	Writer io.Writer
	mu     sync.Mutex
}

func (s *JSONSink) Notify(_ context.Context, event Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Writer == nil {
		return nil
	}
	return json.NewEncoder(s.Writer).Encode(event)
}

type WebhookSink struct {
	URL    string
	Client *http.Client
}

func (s WebhookSink) Notify(ctx context.Context, event Event) error {
	if s.URL == "" {
		return nil
	}
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("alert webhook returned %s", response.Status)
	}
	return nil
}

// SlackSink delivers events through a Slack Incoming Webhook.
//
// This is deliberately the minimal Slack path, not a replacement for
// internal/notify/slack: that package speaks the Web API with a bot token,
// renders Block Kit, and keeps an incident thread per alert. It is the right
// choice whenever a bot token exists. This sink covers the case where one does
// not - a workspace that only granted an Incoming Webhook URL - so audit-watch
// can still reach a channel with a single flag and no app installation.
//
// It sends {"text": "..."} rather than the internal Event JSON, which Slack
// rejects because it carries no text field.
type SlackSink struct {
	URL    string
	Client *http.Client
}

type slackPayload struct {
	Text string `json:"text"`
}

func (s SlackSink) Notify(ctx context.Context, event Event) error {
	if s.URL == "" {
		return nil
	}
	body, err := json.Marshal(slackPayload{Text: slackText(event)})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		// The URL embeds the webhook secret, so the status is all we report.
		return fmt.Errorf("slack webhook returned %s", response.Status)
	}
	return nil
}

// slackText renders one line per event. Score and coverage are appended only
// when the event actually carries an audit report: a zero-valued Report would
// otherwise render as "score=0.0, coverage=0.0%" and read as a measured zero.
func slackText(event Event) string {
	state := event.State
	if state == "" {
		state = "updated"
	}
	var parts []string
	if event.Service != "" {
		parts = append(parts, event.Service)
	}
	if event.Environment != "" {
		parts = append(parts, event.Environment)
	}
	scope := strings.Join(parts, "/")
	text := fmt.Sprintf("SRE Sidekick %s: %s", state, event.Message)
	if scope != "" {
		text += fmt.Sprintf(" (%s)", scope)
	}
	if event.CurrentStatus != "" {
		text += fmt.Sprintf("; status=%s", event.CurrentStatus)
	}
	if event.Report.SchemaVersion != "" {
		text += fmt.Sprintf("; score=%.1f, coverage=%.1f%%", event.Report.Score, event.Report.Coverage*100)
	}
	return text
}

type MultiSink []Sink

func (s MultiSink) Notify(ctx context.Context, event Event) error {
	var errs []error
	for _, sink := range s {
		if sink == nil {
			continue
		}
		if err := sink.Notify(ctx, event); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
