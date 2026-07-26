package rca

import (
	"sort"
	"strings"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/slo"
)

// CorrelateDeploys computes timing candidates deterministically. It never
// labels a deploy as causal; when several versions match, all are returned.
func CorrelateDeploys(events []notify.DeployEvent, onset time.Time, window string) notify.DeployCorrelation {
	duration, err := slo.WindowDuration(window)
	if err != nil || duration <= 0 {
		return notify.DeployCorrelation{State: notify.DeployCorrelationUnknown, Reason: "invalid incident window"}
	}
	start := onset.Add(-2 * duration)
	byKey := map[string]notify.DeployCandidate{}
	for _, event := range events {
		if event.At.IsZero() || event.At.After(onset) || event.At.Before(start) {
			continue
		}
		version := strings.TrimSpace(event.Version)
		deployID := strings.TrimSpace(event.DeployID)
		key := version + "\x00" + deployID
		candidate := notify.DeployCandidate{Version: version, DeployID: deployID, At: event.At, Gap: onset.Sub(event.At)}
		if previous, ok := byKey[key]; !ok || candidate.At.After(previous.At) {
			byKey[key] = candidate
		}
	}
	candidates := make([]notify.DeployCandidate, 0, len(byKey))
	for _, candidate := range byKey {
		candidates = append(candidates, candidate)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].At.After(candidates[j].At) })
	state := notify.DeployCorrelationFound
	if len(candidates) == 0 {
		state = notify.DeployCorrelationNone
	}
	return notify.DeployCorrelation{Candidates: candidates, State: state, ExplicitNone: len(candidates) == 0}
}

func DeployEventsFromEvidence(ev []notify.Evidence) []notify.DeployEvent {
	var out []notify.DeployEvent
	for _, item := range ev {
		if item.DeployVersion == "" || item.Timestamp.IsZero() {
			continue
		}
		out = append(out, notify.DeployEvent{Version: item.DeployVersion, DeployID: item.DeployID, At: item.Timestamp})
	}
	return out
}
