package main

import (
	"math/rand"
	"testing"
)

func TestBuildRunPlanHealthy(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	plan := buildRunPlan(false, 0, rng)

	if plan.Failing {
		t.Fatalf("healthy mode must never fail, got %+v", plan)
	}
	if !plan.Grounded {
		t.Fatalf("healthy mode must be grounded, got %+v", plan)
	}
	if plan.LogSeverity != logSeverityInfo {
		t.Fatalf("expected INFO severity, got %q", plan.LogSeverity)
	}
	if plan.ExceptionType != "" || plan.ExceptionMessage != "" || plan.ExceptionStack != "" {
		t.Fatalf("healthy mode must not populate exception fields, got %+v", plan)
	}
	if plan.ModelProvider == "" || plan.ModelName == "" {
		t.Fatalf("model attributes must be populated, got %+v", plan)
	}
	if plan.InputTokens <= 0 || plan.OutputTokens <= 0 {
		t.Fatalf("token counts must be positive, got %+v", plan)
	}
	if plan.ToolDuration >= buggyToolTimeout {
		t.Fatalf("healthy tool duration must be short, got %s", plan.ToolDuration)
	}
}

func TestBuildRunPlanBuggyAlwaysFailsAtErrorRateOne(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for i := 0; i < 50; i++ {
		plan := buildRunPlan(true, 1.0, rng)
		if !plan.Failing {
			t.Fatalf("errorRate=1.0 must always fail, got %+v", plan)
		}
		if plan.Grounded {
			t.Fatalf("a failing run must not be grounded, got %+v", plan)
		}
		if plan.LogSeverity != logSeverityError {
			t.Fatalf("expected ERROR severity, got %q", plan.LogSeverity)
		}
		if plan.ExceptionType != "TimeoutError" {
			t.Fatalf("expected TimeoutError exception type, got %q", plan.ExceptionType)
		}
		if plan.ExceptionMessage == "" || plan.ExceptionStack == "" {
			t.Fatalf("exception message and stacktrace must be populated, got %+v", plan)
		}
		if plan.ToolDuration != buggyToolTimeout {
			t.Fatalf("expected the tool timeout duration, got %s", plan.ToolDuration)
		}
	}
}

func TestBuildRunPlanBuggyNeverFailsAtErrorRateZero(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 50; i++ {
		plan := buildRunPlan(true, 0.0, rng)
		if plan.Failing {
			t.Fatalf("errorRate=0.0 must never fail even in buggy mode, got %+v", plan)
		}
		if !plan.Grounded {
			t.Fatalf("a non-failing run must be grounded, got %+v", plan)
		}
	}
}

func TestBuildRunPlanPartialFailureRate(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	const trials = 2000
	failures := 0
	for i := 0; i < trials; i++ {
		if buildRunPlan(true, 0.3, rng).Failing {
			failures++
		}
	}
	rate := float64(failures) / float64(trials)
	if rate < 0.2 || rate > 0.4 {
		t.Fatalf("expected roughly 30%% failures, observed %.2f", rate)
	}
}

func TestResolveErrorRateDefaults(t *testing.T) {
	cases := []struct {
		name     string
		explicit float64
		buggy    bool
		want     float64
		wantErr  bool
	}{
		{name: "buggy default", explicit: -1, buggy: true, want: 1.0},
		{name: "healthy default", explicit: -1, buggy: false, want: 0.0},
		{name: "explicit override in buggy mode", explicit: 0.25, buggy: true, want: 0.25},
		{name: "explicit override in healthy mode", explicit: 0.5, buggy: false, want: 0.5},
		{name: "explicit zero", explicit: 0, buggy: true, want: 0},
		{name: "explicit one", explicit: 1, buggy: true, want: 1},
		{name: "rejects out of range", explicit: 1.5, buggy: true, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveErrorRate(tc.explicit, tc.buggy)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got rate=%v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("expected %v, got %v", tc.want, got)
			}
		})
	}
}
