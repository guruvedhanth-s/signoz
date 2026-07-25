package slack

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metrics reports what the Communicate stage is doing, so the loop shows up on
// a SigNoz dashboard rather than only in logs (PRD section 17).
//
// Every method is nil-safe: a nil *Metrics records nothing. That way metrics
// are genuinely optional and no constructor or test has to supply a meter.
type Metrics struct {
	incidents      metric.Int64Counter
	decisions      metric.Int64Counter
	sessionsActive metric.Int64UpDownCounter
	notifyFailures metric.Int64Counter
	followups      metric.Int64Counter
	verifyChecks   metric.Int64Counter
}

// Metric names are effectively a public API: dashboards and alerts hardcode
// them, and renaming one later breaks both silently. They all carry the
// sidekick_ prefix so the whole adapter is one search in SigNoz's metric list,
// and so nothing collides with the metrics the other tracks emit.
const (
	// MetricIncidents counts incidents announced to on-call, by service and
	// status. Named by PRD section 17.
	MetricIncidents = "sidekick_incidents"
	// MetricDecisions counts terminal outcomes: approved, declined, closed or
	// expired.
	MetricDecisions = "sidekick_decisions"
	// MetricSessionsActive tracks how many incident conversations are open. A
	// value that climbs and never falls is a session leak.
	MetricSessionsActive = "sidekick_sessions_active"
	// MetricNotifyFailures counts messages that could not be delivered. This
	// is what makes a Slack outage visible on a dashboard instead of only in
	// the logs.
	MetricNotifyFailures = "sidekick_notify_failures"
	// MetricFollowups counts follow-up questions by outcome.
	MetricFollowups = "sidekick_followups"
	// MetricVerifyChecks counts verify-stage SLO re-evaluations (PRD
	// section 16), by service and the re-evaluated SLO state.
	MetricVerifyChecks = "sidekick_verify_checks"
)

// Outcomes recorded on MetricFollowups.
const (
	FollowupAnswered    = "answered"
	FollowupUnavailable = "unavailable"
	FollowupFailed      = "failed"
	FollowupBusy        = "busy"
)

// Kinds recorded on MetricNotifyFailures.
const (
	FailurePost   = "post"
	FailureUpdate = "update"
	FailureReply  = "reply"
)

// NewMetrics builds the instrument set from an OpenTelemetry meter.
func NewMetrics(meter metric.Meter) (*Metrics, error) {
	if meter == nil {
		return nil, fmt.Errorf("slack: meter is required")
	}

	incidents, err := meter.Int64Counter(MetricIncidents,
		metric.WithDescription("Incidents announced to on-call, by service and status"))
	if err != nil {
		return nil, err
	}
	decisions, err := meter.Int64Counter(MetricDecisions,
		metric.WithDescription("Terminal outcomes recorded for an incident conversation"))
	if err != nil {
		return nil, err
	}
	sessionsActive, err := meter.Int64UpDownCounter(MetricSessionsActive,
		metric.WithDescription("Incident conversations currently open"))
	if err != nil {
		return nil, err
	}
	notifyFailures, err := meter.Int64Counter(MetricNotifyFailures,
		metric.WithDescription("Slack messages that could not be delivered"))
	if err != nil {
		return nil, err
	}
	followups, err := meter.Int64Counter(MetricFollowups,
		metric.WithDescription("Follow-up questions answered in an incident thread"))
	if err != nil {
		return nil, err
	}
	verifyChecks, err := meter.Int64Counter(MetricVerifyChecks,
		metric.WithDescription("Verify-stage SLO re-evaluations, by service and re-evaluated SLO state"))
	if err != nil {
		return nil, err
	}

	return &Metrics{
		incidents:      incidents,
		decisions:      decisions,
		sessionsActive: sessionsActive,
		notifyFailures: notifyFailures,
		followups:      followups,
		verifyChecks:   verifyChecks,
	}, nil
}

// IncidentAnnounced records a new incident conversation.
func (m *Metrics) IncidentAnnounced(ctx context.Context, service, status string) {
	if m == nil || m.incidents == nil {
		return
	}
	m.incidents.Add(ctx, 1, metric.WithAttributes(
		attribute.String("service", service),
		attribute.String("status", status),
	))
	if m.sessionsActive != nil {
		m.sessionsActive.Add(ctx, 1)
	}
}

// DecisionRecorded records a terminal outcome and closes out the session
// gauge.
func (m *Metrics) DecisionRecorded(ctx context.Context, service, decision string) {
	if m == nil || m.decisions == nil {
		return
	}
	m.decisions.Add(ctx, 1, metric.WithAttributes(
		attribute.String("service", service),
		attribute.String("decision", decision),
	))
	if m.sessionsActive != nil {
		m.sessionsActive.Add(ctx, -1)
	}
}

// FollowupRecorded records the outcome of one follow-up question.
func (m *Metrics) FollowupRecorded(ctx context.Context, service, outcome string) {
	if m == nil || m.followups == nil {
		return
	}
	m.followups.Add(ctx, 1, metric.WithAttributes(
		attribute.String("service", service),
		attribute.String("outcome", outcome),
	))
}

// VerifyChecked records one verify-stage SLO re-evaluation.
func (m *Metrics) VerifyChecked(ctx context.Context, service, sloState string) {
	if m == nil || m.verifyChecks == nil {
		return
	}
	m.verifyChecks.Add(ctx, 1, metric.WithAttributes(
		attribute.String("service", service),
		attribute.String("slo_state", sloState),
	))
}

// NotifyFailed records an undelivered message.
func (m *Metrics) NotifyFailed(ctx context.Context, kind string) {
	if m == nil || m.notifyFailures == nil {
		return
	}
	m.notifyFailures.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind)))
}
