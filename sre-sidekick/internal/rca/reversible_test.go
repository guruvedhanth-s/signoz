package rca

import "testing"

func TestReversibleForFix(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"rollback", "roll back the last deployment", true},
		{"rollback one word", "rollback to v1.2.3", true},
		{"restart", "restart the support-agent pods", true},
		{"scale up", "scale up the worker pool from 2 to 4 replicas", true},
		{"scale down", "scale down the connection pool", true},
		{"raise timeout", "raise timeout on the search_kb client to 10s", true},
		{"raise the timeout", "raise the timeout for the downstream call", true},
		{"case insensitive", "ROLL BACK the release", true},
		{"unrelated increase phrasing", "increase gateway timeout", false},
		{"vague investigate", "investigate the root cause further", false},
		{"schema migration", "run a destructive schema migration to drop the column", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reversibleForFix(tc.text); got != tc.want {
				t.Errorf("reversibleForFix(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}
