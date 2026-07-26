package rca

import (
	"strings"
	"testing"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify"
)

func TestCorrelateDeploysReturnsOneCandidateAndGap(t *testing.T) {
	onset := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	got := CorrelateDeploys([]notify.DeployEvent{{Version: "v1.4.2", At: onset.Add(-4 * time.Minute)}}, onset, "5m")
	if len(got.Candidates) != 1 || got.Candidates[0].Version != "v1.4.2" || got.Candidates[0].Gap != 4*time.Minute {
		t.Fatalf("correlation = %+v", got)
	}
	if strings.Contains(got.Summary(), "caus") {
		t.Fatalf("summary makes a causal claim: %q", got.Summary())
	}
}

func TestCorrelateDeploysKeepsSeveralCandidates(t *testing.T) {
	onset := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	got := CorrelateDeploys([]notify.DeployEvent{
		{Version: "v1", At: onset.Add(-2 * time.Minute)},
		{Version: "v2", At: onset.Add(-4 * time.Minute)},
	}, onset, "5m")
	if len(got.Candidates) != 2 {
		t.Fatalf("candidate count = %d, want 2", len(got.Candidates))
	}
}

func TestCorrelateDeploysExplicitlyReportsNoCandidate(t *testing.T) {
	onset := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	got := CorrelateDeploys(nil, onset, "5m")
	if !got.ExplicitNone || !strings.Contains(got.Summary(), "No recent deploy") {
		t.Fatalf("correlation = %+v, want explicit no-deploy result", got)
	}
}
