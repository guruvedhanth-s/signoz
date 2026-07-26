package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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

// SlackSink converts reliability events to Slack Incoming Webhook payloads.
// It intentionally sends a small text payload instead of the internal Event
// JSON shape, which Slack does not render as a message.
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
		return fmt.Errorf("Slack webhook returned %s", response.Status)
	}
	return nil
}

func slackText(event Event) string {
	state := event.State
	if state == "" {
		state = "updated"
	}
	return fmt.Sprintf(
		"SRE Sidekick %s: %s (%s/%s); status=%s, score=%.1f, coverage=%.1f%%",
		state, event.Message, event.Service, event.Environment,
		event.CurrentStatus, event.Report.Score, event.Report.Coverage*100,
	)
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
