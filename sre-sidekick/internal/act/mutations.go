package act

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/notify/slack"
)

// ParseMutationCommand accepts a deliberately small command grammar for the
// future conversational entry point. It produces one typed mutation or an
// error; it never guesses between mutations or builds an arbitrary API call.
func ParseMutationCommand(raw string) (slack.MutationRequest, error) {
	parts := strings.Fields(strings.TrimSpace(raw))
	if len(parts) == 0 {
		return slack.MutationRequest{}, fmt.Errorf("mutation command is empty")
	}
	req := slack.MutationRequest{Kind: slack.MutationKind(parts[0]), ManagedBy: "signoz-sre-sidekick"}
	values := map[string]string{}
	for _, part := range parts[1:] {
		key, value, ok := strings.Cut(part, "=")
		if !ok || strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			return slack.MutationRequest{}, fmt.Errorf("mutation argument %q must be key=value", part)
		}
		if _, exists := values[key]; exists {
			return slack.MutationRequest{}, fmt.Errorf("mutation argument %q was repeated", key)
		}
		values[key] = value
	}
	req.Name = values["name"]
	req.SLO = values["slo"]
	req.Tier = values["tier"]
	if rawMultiplier := values["multiplier"]; rawMultiplier != "" {
		multiplier, err := strconv.ParseFloat(rawMultiplier, 64)
		if err != nil {
			return slack.MutationRequest{}, fmt.Errorf("invalid multiplier %q: %w", rawMultiplier, err)
		}
		req.NewMultiplier = multiplier
	}
	if rawDuration := values["duration"]; rawDuration != "" {
		duration, err := time.ParseDuration(rawDuration)
		if err != nil {
			return slack.MutationRequest{}, fmt.Errorf("invalid duration %q: %w", rawDuration, err)
		}
		req.Duration = duration
	}
	if err := req.Validate(); err != nil {
		return slack.MutationRequest{}, err
	}
	return req, nil
}
