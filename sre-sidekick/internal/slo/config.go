package slo

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Version      string              `yaml:"version" json:"version"`
	Service      string              `yaml:"service" json:"service"`
	Environment  string              `yaml:"environment" json:"environment"`
	SLOs         []Definition        `yaml:"slos" json:"slos"`
	Completeness *CompletenessConfig `yaml:"completeness,omitempty" json:"completeness,omitempty"`
}

type CompletenessConfig struct {
	ExpectedMetrics  []string `yaml:"expected_metrics" json:"expected_metrics"`
	ServiceLabel     string   `yaml:"service_label" json:"service_label"`
	EnvironmentLabel string   `yaml:"environment_label" json:"environment_label"`
}

type Definition struct {
	Name          string  `yaml:"name" json:"name"`
	Description   string  `yaml:"description" json:"description"`
	Type          SLIType `yaml:"type" json:"type"`
	Target        float64 `yaml:"target" json:"target"`
	Window        string  `yaml:"window" json:"window"`
	GoodMetric    string  `yaml:"good_metric" json:"good_metric,omitempty"`
	TotalMetric   string  `yaml:"total_metric" json:"total_metric,omitempty"`
	LatencyMetric string  `yaml:"latency_metric" json:"latency_metric,omitempty"`
	ThresholdMS   float64 `yaml:"threshold_ms" json:"threshold_ms,omitempty"`
	// LatencyBucketUnit is the unit LatencyMetric's histogram "le" bucket
	// boundaries are stored in: "ms" (default) or "s". This is NOT a given
	// - OTel semantic-convention histograms (e.g. a custom
	// "*_duration_seconds" metric) use seconds, but SigNoz's own
	// zero-instrumentation latency histogram (the spanmetrics-processor
	// "signoz_latency" metric, the natural default latency_metric for any
	// SigNoz-traced service) uses milliseconds - confirmed live against a
	// running SigNoz instance (le values 0.1, 1, 2, 6, 10, 50, 100, 250,
	// 500, 1000, 1400, 2000, 5000, 10000, 20000, 40000, 60000, +Inf; a
	// 60-second bucket boundary is only sane if the unit is milliseconds).
	// Getting this wrong does not error - the "le" filter still matches A
	// bucket, just the wrong one, so the SLI silently comes out far too
	// low (near 0%) rather than failing loudly. Defaults to "ms" to match
	// SigNoz's own metric; set to "s" for a custom OTel-semconv seconds
	// histogram.
	LatencyBucketUnit string `yaml:"latency_bucket_unit" json:"latency_bucket_unit,omitempty"`
	// LatencyBucketMetric and LatencyCountMetric override the concrete
	// metric names deriveMetricQueries (sli.go) reads for a
	// latency_threshold SLI, instead of deriving
	// "<latency_metric>_bucket" / "<latency_metric>_count" (the OTel
	// semantic-convention underscore suffix a custom-instrumented
	// histogram normally uses). This is NOT a given - SigNoz's own
	// zero-instrumentation latency histogram (the spanmetrics-processor
	// "signoz_latency" metric) does not follow that convention: its real
	// child metrics are dot-separated ("signoz_latency.bucket",
	// "signoz_latency.count"), confirmed live against a running SigNoz
	// instance (SELECT DISTINCT metric_name FROM
	// signoz_metrics.distributed_metadata WHERE metric_name LIKE
	// 'signoz_latency%'). Getting this wrong does not error: a
	// nonexistent metric name queries successfully with zero rows (see
	// ScalarBuilder), so the SLO silently reports indeterminate/no-data
	// instead of failing loudly. Both fields are independently optional;
	// either can be set alone, leaving the other on the derived form.
	// Empty (the default) preserves today's underscore-suffix derivation
	// unchanged.
	LatencyBucketMetric string `yaml:"latency_bucket_metric" json:"latency_bucket_metric,omitempty"`
	LatencyCountMetric  string `yaml:"latency_count_metric" json:"latency_count_metric,omitempty"`
	// ServiceLabel and EnvironmentLabel override, for this one definition
	// only, the attribute keys deriveMetricQueries scopes its filter by
	// (cfg.MetricLabels() otherwise, config-wide default
	// "service_name"/"environment"). This is NOT a given per metric: a
	// custom-instrumented counter typically carries the attribute keys the
	// application's own OTel SDK set (often "service_name"/"environment"),
	// but SigNoz's own latency histogram (signoz_latency) is derived from
	// trace resource attributes and carries the OTel resource
	// semantic-convention keys instead ("service.name",
	// "deployment.environment") - confirmed live against a running SigNoz
	// instance. A single Config commonly mixes both kinds of metric across
	// its SLOs (e.g. a custom request-count SLI alongside a
	// signoz_latency-based latency SLI), so this is a per-Definition
	// override, not a Config-wide one - mirrors GateRequest's own
	// ServiceLabel/EnvironmentLabel override in gate.go. Getting this
	// wrong does not error: the filter simply matches nothing, so the SLI
	// query returns zero total and the SLO reports indeterminate/no-data
	// rather than failing loudly.
	ServiceLabel     string `yaml:"service_label" json:"service_label,omitempty"`
	EnvironmentLabel string `yaml:"environment_label" json:"environment_label,omitempty"`
	// MetricTemporality overrides the OTel temporality deriveMetricQueries
	// assumes for this definition's counter queries: "" (default) means
	// Cumulative, matching a typical OTel-SDK-instrumented counter; "delta"
	// means Delta. This is NOT a given: SigNoz's own spanmetrics-processor-
	// derived metrics (signoz_latency.bucket/.count, and any other
	// signoz_*-prefixed derived metric) are Delta temporality - confirmed
	// live, an "increase" aggregation with the wrong temporality returns an
	// empty result set rather than an error, so the SLO silently reports
	// indeterminate/no-data instead of failing loudly.
	MetricTemporality     string   `yaml:"metric_temporality" json:"metric_temporality,omitempty"`
	RequiresCompleteness  bool     `yaml:"requires_completeness" json:"requires_completeness,omitempty"`
	CompletenessThreshold float64  `yaml:"completeness_threshold" json:"completeness_threshold,omitempty"`
	Dependencies          []string `yaml:"dependencies" json:"dependencies,omitempty"`
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read SLO config %q: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse SLO config %q: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate SLO config %q: %w", path, err)
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.Service) == "" {
		return fmt.Errorf("service is required")
	}
	if strings.TrimSpace(c.Environment) == "" {
		return fmt.Errorf("environment is required")
	}
	if len(c.SLOs) == 0 {
		return fmt.Errorf("at least one SLO is required")
	}
	for i, definition := range c.SLOs {
		if err := definition.Validate(); err != nil {
			return fmt.Errorf("slos[%d]: %w", i, err)
		}
		// A completeness-type SLI *is* a coverage measurement (see
		// evaluateCompleteness in engine.go), so it needs dependencies to
		// measure the presence of exactly the way RequiresCompleteness's
		// gate check does - independent of whether RequiresCompleteness
		// itself is also set.
		if (definition.RequiresCompleteness || definition.Type == SLITypeCompleteness) &&
			len(definition.Dependencies) == 0 &&
			(c.Completeness == nil || len(c.Completeness.ExpectedMetrics) == 0) {
			return fmt.Errorf(
				"slos[%d]: completeness requires dependencies or completeness.expected_metrics",
				i,
			)
		}
	}
	if c.Completeness != nil {
		if c.Completeness.ServiceLabel == "" {
			c.Completeness.ServiceLabel = "service_name"
		}
		if c.Completeness.EnvironmentLabel == "" {
			c.Completeness.EnvironmentLabel = "environment"
		}
	}
	return nil
}

func (d Definition) Validate() error {
	if strings.TrimSpace(d.Name) == "" {
		return fmt.Errorf("name is required")
	}
	target := d.NormalizedTarget()
	if target <= 0 || target > 1 {
		return fmt.Errorf("target must be in (0,1] or a percentage, got %v", d.Target)
	}
	if _, err := WindowDuration(d.Window); err != nil {
		return err
	}
	if d.CompletenessThreshold < 0 || d.CompletenessThreshold > 1 {
		return fmt.Errorf("completeness_threshold must be in [0,1], got %v", d.CompletenessThreshold)
	}
	switch strings.ToLower(strings.TrimSpace(d.MetricTemporality)) {
	case "", "cumulative", "delta":
		// valid: empty defaults to "cumulative" (see MetricTemporality doc)
	default:
		return fmt.Errorf("metric_temporality must be \"cumulative\" or \"delta\", got %q", d.MetricTemporality)
	}
	switch d.Type {
	case SLITypeRatio, SLITypeGroundedAnswers:
		if strings.TrimSpace(d.GoodMetric) == "" || strings.TrimSpace(d.TotalMetric) == "" {
			return fmt.Errorf("%s requires good_metric and total_metric", d.Type)
		}
	case SLITypeCompleteness:
		// No good_metric/total_metric: a completeness SLI's value is the
		// dependency-coverage fraction itself (see evaluateCompleteness in
		// engine.go), not a counter ratio. Its dependencies are validated
		// in Config.Validate, where completeness.expected_metrics is also
		// in scope.
	case SLITypeLatencyThreshold:
		if strings.TrimSpace(d.LatencyMetric) == "" || d.ThresholdMS <= 0 {
			return fmt.Errorf("latency_threshold requires latency_metric and positive threshold_ms")
		}
		switch strings.ToLower(strings.TrimSpace(d.LatencyBucketUnit)) {
		case "", "ms", "s":
			// valid: empty defaults to "ms" (see LatencyBucketUnit doc)
		default:
			return fmt.Errorf("latency_bucket_unit must be \"ms\" or \"s\", got %q", d.LatencyBucketUnit)
		}
	default:
		return fmt.Errorf("unsupported SLI type %q", d.Type)
	}
	return nil
}

func WindowDuration(window string) (time.Duration, error) {
	window = strings.TrimSpace(window)
	if window == "" {
		return 0, fmt.Errorf("window is required")
	}
	if strings.HasSuffix(window, "d") {
		days, err := strconv.ParseFloat(strings.TrimSuffix(window, "d"), 64)
		if err != nil || days <= 0 {
			return 0, fmt.Errorf("invalid window %q", window)
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	duration, err := time.ParseDuration(window)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("invalid window %q", window)
	}
	return duration, nil
}

func (d Definition) NormalizedTarget() float64 {
	if d.Target > 1 {
		return d.Target / 100
	}
	return d.Target
}

func (c Config) GateThreshold(definition Definition) float64 {
	if definition.CompletenessThreshold > 0 {
		return definition.CompletenessThreshold
	}
	return 0.95
}

func (c Config) MetricLabels() (string, string) {
	if c.Completeness == nil {
		return "service_name", "environment"
	}
	serviceLabel := strings.TrimSpace(c.Completeness.ServiceLabel)
	if serviceLabel == "" {
		serviceLabel = "service_name"
	}
	environmentLabel := strings.TrimSpace(c.Completeness.EnvironmentLabel)
	if environmentLabel == "" {
		environmentLabel = "environment"
	}
	return serviceLabel, environmentLabel
}
