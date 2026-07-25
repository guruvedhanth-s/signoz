package slack

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/session"
)

func TestMetricsRecordIncidentsDecisionsAndFollowups(t *testing.T) {
	reader, metrics := testMetrics(t)
	ctx := context.Background()

	metrics.IncidentAnnounced(ctx, "support-agent", "diagnosed")
	metrics.IncidentAnnounced(ctx, "checkout-api", "indeterminate")
	metrics.DecisionRecorded(ctx, "support-agent", "approved")
	metrics.FollowupRecorded(ctx, "support-agent", FollowupAnswered)
	metrics.NotifyFailed(ctx, FailurePost)

	if got := counterValue(t, reader, MetricIncidents, "service", "support-agent"); got != 1 {
		t.Errorf("%s{service=support-agent} = %d, want 1", MetricIncidents, got)
	}
	if got := counterValue(t, reader, MetricIncidents, "status", "indeterminate"); got != 1 {
		t.Errorf("%s{status=indeterminate} = %d, want 1", MetricIncidents, got)
	}
	if got := counterValue(t, reader, MetricDecisions, "decision", "approved"); got != 1 {
		t.Errorf("%s{decision=approved} = %d, want 1", MetricDecisions, got)
	}
	if got := counterValue(t, reader, MetricFollowups, "outcome", FollowupAnswered); got != 1 {
		t.Errorf("%s{outcome=answered} = %d, want 1", MetricFollowups, got)
	}
	if got := counterValue(t, reader, MetricNotifyFailures, "kind", FailurePost); got != 1 {
		t.Errorf("%s{kind=post} = %d, want 1", MetricNotifyFailures, got)
	}

	// Two opened, one resolved: a gauge that never falls would mean sessions
	// are leaking.
	if got := sumValue(t, reader, MetricSessionsActive); got != 1 {
		t.Errorf("%s = %d, want 1", MetricSessionsActive, got)
	}
}

// Metrics must be genuinely optional: no constructor or test should need a
// meter.
func TestNilMetricsRecordNothingAndDoNotPanic(t *testing.T) {
	var metrics *Metrics
	ctx := context.Background()

	metrics.IncidentAnnounced(ctx, "support-agent", "diagnosed")
	metrics.DecisionRecorded(ctx, "support-agent", "approved")
	metrics.FollowupRecorded(ctx, "support-agent", FollowupFailed)
	metrics.NotifyFailed(ctx, FailureReply)
}

func TestNewMetricsRequiresAMeter(t *testing.T) {
	if _, err := NewMetrics(nil); err == nil {
		t.Fatal("NewMetrics(nil) error = nil, want a meter error")
	}
}

func TestFollowupOutcomeClassification(t *testing.T) {
	cases := map[error]string{
		ErrRCAUnavailable:        FollowupUnavailable,
		errAnalysisBusy:          FollowupBusy,
		errors.New("model died"): FollowupFailed,
	}
	for err, want := range cases {
		if got := followupOutcome(err); got != want {
			t.Errorf("followupOutcome(%v) = %q, want %q", err, got, want)
		}
	}
}

func TestFailureKindClassification(t *testing.T) {
	cases := []struct {
		name string
		msg  message
		want string
	}{
		{"post", message{}, FailurePost},
		{"reply", message{threadTS: "1720000000.0001"}, FailureReply},
		{"update", message{isUpdate: true, updateTS: "1720000000.0001"}, FailureUpdate},
	}
	for _, tc := range cases {
		if got := failureKind(tc.msg); got != tc.want {
			t.Errorf("%s: failureKind() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The coordinator must feed the instruments, not just hold them.
func TestCoordinatorEmitsMetricsThroughTheWholeFlow(t *testing.T) {
	reader, metrics := testMetrics(t)
	f := newFixture(t, &fakeRCA{answer: "because the calls time out"},
		WithCoordinatorMetrics(metrics))

	opened := announceFixture(t, f)
	if got := counterValue(t, reader, MetricIncidents, "service", "support-agent"); got != 1 {
		t.Errorf("%s = %d, want the announcement counted", MetricIncidents, got)
	}
	if got := sumValue(t, reader, MetricSessionsActive); got != 1 {
		t.Errorf("%s = %d, want 1 open session", MetricSessionsActive, got)
	}

	f.coordinator.OnMessage(context.Background(), Message{
		ChannelID: opened.ChannelID, ThreadTS: opened.ThreadTS,
		MessageTS: "1720000100.0002", UserID: "U1", Text: "why?",
	})
	if got := counterValue(t, reader, MetricFollowups, "outcome", FollowupAnswered); got != 1 {
		t.Errorf("%s = %d, want the answered follow-up counted", MetricFollowups, got)
	}

	f.coordinator.OnInteraction(context.Background(), Interaction{
		ActionID: ActionApprove, ChannelID: opened.ChannelID,
		ThreadTS: opened.ThreadTS, MessageTS: opened.ThreadTS, UserID: "U1",
	})
	if got := counterValue(t, reader, MetricDecisions, "decision", "approved"); got != 1 {
		t.Errorf("%s = %d, want the approval counted", MetricDecisions, got)
	}
	// Opened then resolved, so nothing is left outstanding.
	if got := sumValue(t, reader, MetricSessionsActive); got != 0 {
		t.Errorf("%s = %d, want the session closed out", MetricSessionsActive, got)
	}
}

func TestUnavailableAnalysisIsCounted(t *testing.T) {
	reader, metrics := testMetrics(t)
	f := newFixture(t, UnavailableRCA{}, WithCoordinatorMetrics(metrics))
	opened := announceFixture(t, f)

	f.coordinator.OnMessage(context.Background(), Message{
		ChannelID: opened.ChannelID, ThreadTS: opened.ThreadTS,
		MessageTS: "1720000100.0002", UserID: "U1", Text: "why?",
	})

	if got := counterValue(t, reader, MetricFollowups, "outcome", FollowupUnavailable); got != 1 {
		t.Errorf("%s{outcome=unavailable} = %d, want 1", MetricFollowups, got)
	}
}

// A Slack outage must be visible on a dashboard, not only in the logs.
func TestUndeliveredMessagesAreCounted(t *testing.T) {
	reader, metrics := testMetrics(t)
	fake := &fakePoster{results: []fakeResult{{err: errors.New("channel_not_found")}}}
	client, _, _ := testClient(t, fake, WithMetrics(metrics))

	if err := client.NotifyDiagnosis(context.Background(), diagnosedFixture()); err == nil {
		t.Fatal("NotifyDiagnosis() error = nil, want the failure")
	}
	if got := counterValue(t, reader, MetricNotifyFailures, "kind", FailurePost); got != 1 {
		t.Errorf("%s{kind=post} = %d, want 1", MetricNotifyFailures, got)
	}
}

func TestExpiredSessionsAreCounted(t *testing.T) {
	reader, metrics := testMetrics(t)
	f := newFixture(t, &fakeRCA{}, WithCoordinatorMetrics(metrics))

	opened := announceFixture(t, f)
	if err := f.sessions.Close(opened, session.ReasonIdle); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	// ReapIdle reports what it expired; here the close already happened, so
	// drive the metric through a genuine expiry instead.
	f.coordinator.ReapIdle(context.Background())

	if got := sumValue(t, reader, MetricSessionsActive); got != 1 {
		t.Errorf("%s = %d, want the announced session still counted open", MetricSessionsActive, got)
	}
	if got := counterValue(t, reader, MetricDecisions, "decision", "expired"); got != 0 {
		t.Errorf("%s{decision=expired} = %d, want 0 for an already-closed session", MetricDecisions, got)
	}
}

// counterValue sums the data points of a counter whose attributes match the
// given key and value.
func counterValue(t *testing.T, reader metricReader, name, attrKey, attrValue string) int64 {
	t.Helper()
	var total int64
	for _, point := range collectSums(t, reader, name) {
		if matchesAttribute(point, attrKey, attrValue) {
			total += point.Value
		}
	}
	return total
}

// sumValue totals every data point of an instrument, ignoring attributes.
func sumValue(t *testing.T, reader metricReader, name string) int64 {
	t.Helper()
	var total int64
	for _, point := range collectSums(t, reader, name) {
		total += point.Value
	}
	return total
}

func matchesAttribute(point metricdata.DataPoint[int64], key, value string) bool {
	for _, attr := range point.Attributes.ToSlice() {
		if string(attr.Key) == key && attr.Value.AsString() == value {
			return true
		}
	}
	return false
}
