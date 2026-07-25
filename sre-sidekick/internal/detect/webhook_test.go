package detect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify/slack"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/session"
)

type fakeDiagnoser struct {
	mu       sync.Mutex
	requests []slack.DiagnoseRequest
	err      error
}

func (f *fakeDiagnoser) Diagnose(_ context.Context, req slack.DiagnoseRequest) (notify.Diagnosis, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if f.err != nil {
		return notify.Diagnosis{}, f.err
	}
	return notify.Diagnosis{Service: req.Service, Environment: req.Environment, Window: req.Window}, nil
}

func (f *fakeDiagnoser) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

type fakeAnnouncer struct {
	mu        sync.Mutex
	diagnoses []notify.Diagnosis
	err       error
}

func (f *fakeAnnouncer) Announce(_ context.Context, d notify.Diagnosis) (*session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.diagnoses = append(f.diagnoses, d)
	if f.err != nil {
		return nil, f.err
	}
	return nil, nil
}

func (f *fakeAnnouncer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.diagnoses)
}

func postWebhook(t *testing.T, h http.Handler, secret string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	if secret != "" {
		req.Header.Set("X-Sidekick-Webhook-Secret", secret)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func firingPayload(t *testing.T, service, environment, slo, alertName, status string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"status": status,
		"alerts": []map[string]any{
			{
				"status": status,
				"labels": map[string]string{
					"service":     service,
					"environment": environment,
					"slo":         slo,
					"alertname":   alertName,
					"window":      "5m",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return body
}

func TestHandler_RejectsRequestsWithoutTheSharedSecret(t *testing.T) {
	diag := &fakeDiagnoser{}
	ann := &fakeAnnouncer{}
	h := NewHandler("s3cret", diag, ann)

	rec := postWebhook(t, h, "", firingPayload(t, "support-agent", "prod", "grounded-answers", "fast-burn", "firing"))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for a missing secret", rec.Code)
	}

	rec = postWebhook(t, h, "wrong", firingPayload(t, "support-agent", "prod", "grounded-answers", "fast-burn", "firing"))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for a wrong secret", rec.Code)
	}
	if diag.count() != 0 {
		t.Error("an unauthenticated request still triggered a diagnosis")
	}
}

func TestNewHandler_EmptySecretRefusesEverything(t *testing.T) {
	diag := &fakeDiagnoser{}
	h := NewHandler("", diag, &fakeAnnouncer{})

	rec := postWebhook(t, h, "anything", firingPayload(t, "support-agent", "prod", "grounded-answers", "fast-burn", "firing"))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 when no secret is configured", rec.Code)
	}
}

func TestHandler_ValidAlertTriggersDiagnoseAndAnnounce(t *testing.T) {
	diag := &fakeDiagnoser{}
	ann := &fakeAnnouncer{}
	h := NewHandler("s3cret", diag, ann)

	rec := postWebhook(t, h, "s3cret", firingPayload(t, "support-agent", "prod", "grounded-answers", "fast-burn", "firing"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if diag.count() != 1 {
		t.Fatalf("diagnose calls = %d, want 1", diag.count())
	}
	if diag.requests[0].Service != "support-agent" || diag.requests[0].Window != "5m" || diag.requests[0].Alert != "fast-burn" {
		t.Errorf("DiagnoseRequest = %+v, want the alert labels mapped through", diag.requests[0])
	}
	if ann.count() != 1 {
		t.Errorf("announce calls = %d, want 1", ann.count())
	}
}

// A re-fired alert (same service, environment, slo, alert name, and status)
// must not launch a second paid RCA run within the dedup window (PRD
// section 12/20).
func TestHandler_DedupesARefiredAlertWithinTheWindow(t *testing.T) {
	diag := &fakeDiagnoser{}
	ann := &fakeAnnouncer{}
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	h := NewHandler("s3cret", diag, ann, WithNow(func() time.Time { return now }))

	payload := firingPayload(t, "support-agent", "prod", "grounded-answers", "fast-burn", "firing")
	postWebhook(t, h, "s3cret", payload)
	postWebhook(t, h, "s3cret", payload)

	if diag.count() != 1 {
		t.Errorf("diagnose calls = %d, want exactly 1 for a duplicate delivery", diag.count())
	}
}

// A different alert *status* (e.g. the alert resolving, then firing again
// later) is a different fingerprint and must not be treated as a duplicate.
func TestHandler_DifferentAlertStatusIsNotADuplicate(t *testing.T) {
	diag := &fakeDiagnoser{}
	ann := &fakeAnnouncer{}
	h := NewHandler("s3cret", diag, ann)

	postWebhook(t, h, "s3cret", firingPayload(t, "support-agent", "prod", "grounded-answers", "fast-burn", "firing"))
	postWebhook(t, h, "s3cret", firingPayload(t, "support-agent", "prod", "grounded-answers", "fast-burn", "resolved"))

	if diag.count() != 2 {
		t.Errorf("diagnose calls = %d, want 2 - firing and resolved are different fingerprints", diag.count())
	}
}

// The dedup window expiring must allow a genuinely new occurrence of the
// same alert to trigger a fresh diagnosis.
func TestHandler_AllowsARepeatAfterTheDedupWindowExpires(t *testing.T) {
	diag := &fakeDiagnoser{}
	ann := &fakeAnnouncer{}
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	h := NewHandler("s3cret", diag, ann, WithDedupWindow(time.Minute), WithNow(func() time.Time { return now }))

	payload := firingPayload(t, "support-agent", "prod", "grounded-answers", "fast-burn", "firing")
	postWebhook(t, h, "s3cret", payload)
	now = now.Add(2 * time.Minute)
	postWebhook(t, h, "s3cret", payload)

	if diag.count() != 2 {
		t.Errorf("diagnose calls = %d, want 2 once the dedup window has passed", diag.count())
	}
}

func TestHandler_MissingServiceLabelDropsTheAlert(t *testing.T) {
	diag := &fakeDiagnoser{}
	ann := &fakeAnnouncer{}
	h := NewHandler("s3cret", diag, ann)

	body, _ := json.Marshal(map[string]any{
		"status": "firing",
		"alerts": []map[string]any{{"status": "firing", "labels": map[string]string{"environment": "prod"}}},
	})
	rec := postWebhook(t, h, "s3cret", body)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (a dropped alert is not a client error)", rec.Code)
	}
	if diag.count() != 0 {
		t.Error("an alert missing its service label still triggered a diagnosis")
	}
}

func TestHandler_MalformedPayloadReturnsBadRequest(t *testing.T) {
	h := NewHandler("s3cret", &fakeDiagnoser{}, &fakeAnnouncer{})
	rec := postWebhook(t, h, "s3cret", []byte("not json"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a malformed payload", rec.Code)
	}
}

func TestHandler_DiagnoseFailureDoesNotAnnounce(t *testing.T) {
	diag := &fakeDiagnoser{err: errors.New("evidence gate unavailable")}
	ann := &fakeAnnouncer{}
	h := NewHandler("s3cret", diag, ann)

	rec := postWebhook(t, h, "s3cret", firingPayload(t, "support-agent", "prod", "grounded-answers", "fast-burn", "firing"))
	// The webhook still acks 200 to SigNoz's alertmanager - a downstream
	// RCA failure is not a delivery failure, and alertmanager would retry
	// forever on a non-2xx.
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 even when Diagnose fails", rec.Code)
	}
	if ann.count() != 0 {
		t.Error("Announce was called despite Diagnose failing")
	}
}
