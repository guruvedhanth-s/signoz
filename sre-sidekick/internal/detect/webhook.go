// Package detect implements the alert-driven Detect stage (PRD section
// 12): a SigNoz notification-channel webhook lands here, is authenticated
// by a shared secret, deduplicated by an incident fingerprint that
// includes alert state, and turned into a Diagnose call whose result is
// handed to on-call - no LLM involved in routing the incident.
package detect

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify/slack"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/session"
)

// maxWebhookBody bounds how much of a webhook request body is read, so an
// oversized or malformed delivery cannot exhaust memory.
const maxWebhookBody = 1 << 20

// defaultDedupWindow is how long a repeated delivery of the same incident
// fingerprint is treated as a duplicate rather than a fresh RCA trigger.
// SigNoz's alertmanager repeats a firing alert on its own interval; this
// must be long enough to absorb that repetition without silently
// swallowing a genuinely new incident.
const defaultDedupWindow = 10 * time.Minute

// Diagnoser runs the RCA pipeline for one incident - the same seam
// slack.RCA.Diagnose uses, so the alert-driven path and the /diagnose
// command both run through the identical pipeline (PRD section 7).
type Diagnoser interface {
	Diagnose(ctx context.Context, req slack.DiagnoseRequest) (notify.Diagnosis, error)
}

// Announcer posts a diagnosis and opens or reuses its incident session -
// *slack.Coordinator's own entry point for the alert-driven path.
type Announcer interface {
	Announce(ctx context.Context, d notify.Diagnosis) (*session.Session, error)
}

// Handler is an http.Handler for one SigNoz alertmanager-compatible
// notification-channel webhook. Construct with NewHandler; the zero value
// is not usable (it has no secret, so it refuses every request - see
// authorized).
type Handler struct {
	secret    string
	diagnoser Diagnoser
	announcer Announcer

	// dedup bounds how long a fingerprint is deduplicated for; defaults to
	// defaultDedupWindow.
	dedup time.Duration
	// now returns the current time; overridable in tests.
	now func() time.Time

	mu   sync.Mutex
	seen map[string]time.Time
}

// HandlerOption configures a Handler built by NewHandler.
type HandlerOption func(*Handler)

// WithDedupWindow overrides the default deduplication window.
func WithDedupWindow(d time.Duration) HandlerOption {
	return func(h *Handler) { h.dedup = d }
}

// WithNow overrides the Handler's clock, for tests.
func WithNow(now func() time.Time) HandlerOption {
	return func(h *Handler) { h.now = now }
}

// NewHandler builds a Handler. secret must be non-empty: a Handler with no
// secret refuses every request rather than silently accepting unauthenticated
// ones (PRD section 20 - the webhook must not be usable to trigger unbounded
// paid RCA runs).
func NewHandler(secret string, diagnoser Diagnoser, announcer Announcer, opts ...HandlerOption) *Handler {
	h := &Handler{
		secret:    secret,
		diagnoser: diagnoser,
		announcer: announcer,
		dedup:     defaultDedupWindow,
		now:       time.Now,
		seen:      make(map[string]time.Time),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// alertmanagerWebhook is the subset of Prometheus Alertmanager's webhook
// payload (which SigNoz's alertmanager sends to a webhook notification
// channel) this handler reads. Only labels are used to route the
// incident; no LLM is involved in detection (PRD section 12).
type alertmanagerWebhook struct {
	Status string `json:"status"` // "firing" or "resolved", the group-level default
	Alerts []struct {
		Status string            `json:"status"`
		Labels map[string]string `json:"labels"`
	} `json:"alerts"`
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	var payload alertmanagerWebhook
	if err := json.NewDecoder(io.LimitReader(r.Body, maxWebhookBody)).Decode(&payload); err != nil {
		http.Error(w, "malformed webhook payload", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	for _, alert := range payload.Alerts {
		status := alert.Status
		if status == "" {
			status = payload.Status
		}
		h.handleAlert(ctx, alert.Labels, status)
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) authorized(r *http.Request) bool {
	if h.secret == "" {
		return false
	}
	got := strings.TrimSpace(r.Header.Get("X-Sidekick-Webhook-Secret"))
	return subtle.ConstantTimeCompare([]byte(got), []byte(h.secret)) == 1
}

// handleAlert routes one alert's labels into a Diagnose + Announce call.
// Only resolves service/environment/window/alert from labels - a
// deliberately dumb, deterministic mapping with no reasoning involved
// (PRD section 12).
func (h *Handler) handleAlert(ctx context.Context, labels map[string]string, status string) {
	service := labels["service"]
	environment := labels["environment"]
	if service == "" || environment == "" {
		slog.Warn("detect: alert webhook missing service/environment labels, dropping", "labels", labels)
		return
	}
	slo := labels["slo"]
	window := labels["window"]
	alertName := labels["alertname"]

	fp := fingerprint(service, environment, slo, alertName, status)
	if h.recentlySeen(fp) {
		slog.Info("detect: duplicate alert delivery within the dedup window, skipping",
			"service", service, "environment", environment, "alert", alertName, "status", status)
		return
	}

	diagnosis, err := h.diagnoser.Diagnose(ctx, slack.DiagnoseRequest{
		Service:     service,
		Environment: environment,
		Window:      window,
		Alert:       alertName,
	})
	if err != nil {
		slog.Error("detect: diagnose failed for an alert-driven incident",
			"service", service, "environment", environment, "alert", alertName, "error", err)
		return
	}
	if _, err := h.announcer.Announce(ctx, diagnosis); err != nil {
		slog.Error("detect: could not announce an alert-driven diagnosis",
			"service", service, "environment", environment, "error", err)
	}
}

// recentlySeen reports whether fp was already handled within the dedup
// window, recording it as seen either way. It also opportunistically prunes
// entries far outside the window, so a long-running watch process's
// dedup map does not grow unbounded.
func (h *Handler) recentlySeen(fp string) bool {
	now := h.now()

	h.mu.Lock()
	defer h.mu.Unlock()

	if last, ok := h.seen[fp]; ok && now.Sub(last) < h.dedup {
		return true
	}
	h.seen[fp] = now

	for k, t := range h.seen {
		if now.Sub(t) > h.dedup*4 {
			delete(h.seen, k)
		}
	}
	return false
}

func fingerprint(service, environment, slo, alertName, status string) string {
	return strings.Join([]string{service, environment, slo, alertName, status}, "|")
}
