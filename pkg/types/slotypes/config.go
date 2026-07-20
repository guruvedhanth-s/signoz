package slotypes

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Config is the root of an SLO-as-code document.
type Config struct {
	Service     string          `yaml:"service" json:"service"`
	Environment string          `yaml:"environment" json:"environment"`
	SLOs        []SLODefinition `yaml:"slos" json:"slos"`
}

// SLODefinition is a single service-level objective declared as code.
type SLODefinition struct {
	Name        string  `yaml:"name" json:"name"`
	Description string  `yaml:"description" json:"description"`
	Type        SLIType `yaml:"type" json:"type"`

	// Target is the objective as a fraction in the range 0..1 (for example 0.99).
	// It may be authored as "99%" and normalized on load; see NormalizeTarget.
	Target float64 `yaml:"target" json:"target"`

	// Window is the evaluation window such as "7d" or "30d".
	Window Window `yaml:"window" json:"window"`

	// Ratio inputs.
	GoodQuery  string `yaml:"good_query" json:"goodQuery,omitempty"`
	TotalQuery string `yaml:"total_query" json:"totalQuery,omitempty"`

	// RequiresCompleteness gates the SLO on the Telemetry Health Auditor. When the
	// auditor reports completeness below the engine's threshold, the SLO resolves
	// to StateIndeterminate instead of a possibly-false pass.
	RequiresCompleteness bool `yaml:"requires_completeness" json:"requiresCompleteness,omitempty"`
}

// Validate checks that the definition has the fields its type requires.
func (d SLODefinition) Validate() error {
	if strings.TrimSpace(d.Name) == "" {
		return fmt.Errorf("slo name is required")
	}
	if d.Target <= 0 || d.Target > 1 {
		return fmt.Errorf("slo %q: target must be in (0,1], got %v", d.Name, d.Target)
	}
	if _, err := d.Window.Duration(); err != nil {
		return fmt.Errorf("slo %q: %w", d.Name, err)
	}

	switch d.Type {
	case SLITypeRatio:
		if strings.TrimSpace(d.GoodQuery) == "" || strings.TrimSpace(d.TotalQuery) == "" {
			return fmt.Errorf("slo %q: ratio type requires good_query and total_query", d.Name)
		}
	case SLITypeLatencyThreshold, SLITypeCompleteness, SLITypeGroundedAnswers:
		// Validated by their own evaluators in later milestones.
	default:
		return fmt.Errorf("slo %q: unknown type %q", d.Name, d.Type)
	}
	return nil
}

// Duration parses a Window string such as "30d", "12h", "45m", or "3600s" into a
// time.Duration. The day unit "d" is supported in addition to Go's stdlib units.
func (w Window) Duration() (time.Duration, error) {
	s := strings.TrimSpace(string(w))
	if s == "" {
		return 0, fmt.Errorf("window is empty")
	}
	if strings.HasSuffix(s, "d") {
		days, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil {
			return 0, fmt.Errorf("invalid window %q: %w", s, err)
		}
		return time.Duration(days * 24 * float64(time.Hour)), nil
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid window %q: %w", s, err)
	}
	return dur, nil
}
