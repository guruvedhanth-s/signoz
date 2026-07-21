package slo

import (
	"testing"
	"time"
)

func TestWindowDuration(t *testing.T) {
	cases := map[Window]time.Duration{
		"30d": 30 * 24 * time.Hour,
		"12h": 12 * time.Hour,
		"45m": 45 * time.Minute,
	}
	for w, want := range cases {
		got, err := w.Duration()
		if err != nil || got != want {
			t.Fatalf("Window(%q) = %v, %v; want %v", w, got, err, want)
		}
	}
	for _, bad := range []Window{"", "nope"} {
		if _, err := bad.Duration(); err == nil {
			t.Fatalf("Window(%q): expected error", bad)
		}
	}
}

func TestConfigNormalizeAndValidate(t *testing.T) {
	cfg := &Config{SLOs: []SLODefinition{{
		Name: "x", Type: SLITypeRatio, Target: 99, Window: "30d", GoodQuery: "g", TotalQuery: "t",
	}}}
	cfg.Normalize()
	if cfg.SLOs[0].Target != 0.99 {
		t.Fatalf("target = %v, want 0.99", cfg.SLOs[0].Target)
	}
	if err := cfg.SLOs[0].Validate(); err != nil {
		t.Fatalf("valid definition rejected: %v", err)
	}
	bad := cfg.SLOs[0]
	bad.GoodQuery = ""
	if err := bad.Validate(); err == nil {
		t.Fatal("expected error for ratio without good_query")
	}
}

func TestStateMachine(t *testing.T) {
	if resolveState(0.995, 0.99, 1.0, 0.95, true) != StateHealthy {
		t.Fatal("want healthy")
	}
	if resolveState(0.98, 0.99, 1.0, 0.95, true) != StateUnhealthy {
		t.Fatal("want unhealthy")
	}
	if resolveState(0.995, 0.99, 0.4, 0.95, true) != StateIndeterminate {
		t.Fatal("want indeterminate")
	}
	if resolveState(0.995, 0.99, 0.4, 0.95, false) != StateHealthy {
		t.Fatal("want healthy when not gated")
	}
}
