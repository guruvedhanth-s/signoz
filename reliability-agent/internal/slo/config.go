package slo

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
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
	// Target is a fraction in (0,1]. Authored as 99 or 0.99; normalized on load.
	Target float64 `yaml:"target" json:"target"`
	Window Window  `yaml:"window" json:"window"`
	// Ratio inputs (single-vector PromQL expressions).
	GoodQuery  string `yaml:"good_query" json:"goodQuery,omitempty"`
	TotalQuery string `yaml:"total_query" json:"totalQuery,omitempty"`
	// RequiresCompleteness gates the SLO on the telemetry auditor.
	RequiresCompleteness bool `yaml:"requires_completeness" json:"requiresCompleteness,omitempty"`
}

// LoadConfig reads and validates an SLO-as-code file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading SLO config %q: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing SLO config %q: %w", path, err)
	}
	cfg.Normalize()
	for _, def := range cfg.SLOs {
		if err := def.Validate(); err != nil {
			return nil, fmt.Errorf("invalid SLO config %q: %w", path, err)
		}
	}
	return &cfg, nil
}

// Normalize converts author-friendly values to canonical form. A target above 1
// is treated as a percentage and divided by 100.
func (c *Config) Normalize() {
	for i := range c.SLOs {
		if c.SLOs[i].Target > 1 {
			c.SLOs[i].Target /= 100
		}
	}
}

// Validate checks a definition has the fields its type requires.
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

// Duration parses a Window such as "30d", "12h", or "45m". The day unit "d" is
// supported in addition to Go's stdlib units.
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
