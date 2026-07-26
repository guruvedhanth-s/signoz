package main

import (
	"fmt"
	"math/rand"
	"time"
)

// RunPlan describes the telemetry to emit for one simulated agent run:
// the model call attributes, the tool.search_kb outcome, whether the
// answer was grounded, and the correlated log record. buildRunPlan
// computes it without touching any OTel API so the healthy/buggy
// decision logic can be unit tested without a live collector.
type RunPlan struct {
	// Failing is true when this run simulates the tool.search_kb timeout.
	Failing bool

	ModelProvider string
	ModelName     string
	InputTokens   int64
	OutputTokens  int64

	// ToolDuration is the simulated duration of the tool.search_kb span.
	ToolDuration time.Duration

	// Populated only when Failing is true, describing the planted
	// exception span event on tool.search_kb.
	ExceptionType    string
	ExceptionMessage string
	ExceptionStack   string

	Grounded bool

	LogSeverity string
	LogBody     string
}

const (
	defaultModelProvider = "openai"
	defaultModelName     = "gpt-4o-mini"

	minInputTokens   = 80
	inputTokenRange  = 420
	minOutputTokens  = 20
	outputTokenRange = 280

	healthyToolBaseDuration   = 40 * time.Millisecond
	healthyToolJitterDuration = 80 * time.Millisecond

	// buggyToolTimeout is the simulated tool.search_kb timeout duration
	// recorded on the span. The process does not actually block for this
	// long; the span start/end timestamps are set explicitly so the loop
	// stays responsive regardless of --interval.
	buggyToolTimeout = 5 * time.Second

	logSeverityInfo  = "INFO"
	logSeverityError = "ERROR"
)

// buildRunPlan decides the outcome of one simulated agent run. buggy
// selects whether this process is simulating the tool.search_kb timeout
// failure mode; errorRate (0..1) controls what fraction of runs actually
// fail while buggy is enabled, so partial degradation can be simulated
// too. rng supplies all randomness so the result is reproducible in
// tests.
func buildRunPlan(buggy bool, errorRate float64, rng *rand.Rand) RunPlan {
	failing := buggy && rng.Float64() < errorRate

	plan := RunPlan{
		Failing:       failing,
		ModelProvider: defaultModelProvider,
		ModelName:     defaultModelName,
		InputTokens:   int64(minInputTokens + rng.Intn(inputTokenRange)),
		OutputTokens:  int64(minOutputTokens + rng.Intn(outputTokenRange)),
	}

	if failing {
		plan.ToolDuration = buggyToolTimeout
		plan.ExceptionType = "TimeoutError"
		plan.ExceptionMessage = fmt.Sprintf("tool.search_kb timed out after %s", buggyToolTimeout)
		plan.ExceptionStack = fmt.Sprintf(
			"TimeoutError: tool.search_kb timed out after %s\n"+
				"  at searchKnowledgeBase (tool/search_kb.go:42)\n"+
				"  at agentRun (agent/run.go:118)",
			buggyToolTimeout,
		)
		plan.Grounded = false
		plan.LogSeverity = logSeverityError
		plan.LogBody = "agent run failed: tool.search_kb timed out"
		return plan
	}

	plan.ToolDuration = healthyToolBaseDuration + time.Duration(rng.Int63n(int64(healthyToolJitterDuration)))
	plan.Grounded = true
	plan.LogSeverity = logSeverityInfo
	plan.LogBody = "agent run completed successfully"
	return plan
}

// resolveErrorRate applies the "--error-rate default depends on --buggy"
// rule: an explicit, non-negative value is always honored; otherwise it
// defaults to 1.0 in buggy mode (always fail) and 0.0 in healthy mode
// (never fail).
func resolveErrorRate(explicit float64, buggy bool) (float64, error) {
	if explicit < 0 {
		if buggy {
			return 1.0, nil
		}
		return 0.0, nil
	}
	if explicit > 1 {
		return 0, fmt.Errorf("error-rate must be between 0 and 1, got %v", explicit)
	}
	return explicit, nil
}
