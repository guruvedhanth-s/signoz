package slo

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestExampleSLOConfigsLoad(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate test file")
	}
	examples := filepath.Join(filepath.Dir(filename), "..", "..", "examples")
	for _, name := range []string{"checkout-slo.yaml", "support-agent-slo.yaml"} {
		if _, err := LoadConfig(filepath.Join(examples, name)); err != nil {
			t.Fatalf("load example %s: %v", name, err)
		}
	}
}

func TestTargetPercentageAndOneHundredPercent(t *testing.T) {
	percentage := Definition{
		Name: "x", Type: SLITypeRatio, Target: 99, Window: "1h",
		GoodMetric: "good", TotalMetric: "total",
	}
	if err := percentage.Validate(); err != nil || percentage.NormalizedTarget() != 0.99 {
		t.Fatalf("percentage target did not normalize: err=%v target=%v", err, percentage.NormalizedTarget())
	}
	one := Definition{
		Name: "x", Type: SLITypeRatio, Target: 1, Window: "1h",
		GoodMetric: "good", TotalMetric: "total",
	}
	if err := one.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRatioRequiresGoodAndTotalMetric(t *testing.T) {
	missingGood := Definition{Name: "x", Type: SLITypeRatio, Target: 0.99, Window: "1h", TotalMetric: "total"}
	if err := missingGood.Validate(); err == nil {
		t.Fatal("expected missing good_metric to be rejected")
	}
	missingTotal := Definition{Name: "x", Type: SLITypeRatio, Target: 0.99, Window: "1h", GoodMetric: "good"}
	if err := missingTotal.Validate(); err == nil {
		t.Fatal("expected missing total_metric to be rejected")
	}
}

func TestLatencyThresholdRequiresMetricAndPositiveThreshold(t *testing.T) {
	missingMetric := Definition{Name: "x", Type: SLITypeLatencyThreshold, Target: 0.99, Window: "1h", ThresholdMS: 1000}
	if err := missingMetric.Validate(); err == nil {
		t.Fatal("expected missing latency_metric to be rejected")
	}
	nonPositiveThreshold := Definition{Name: "x", Type: SLITypeLatencyThreshold, Target: 0.99, Window: "1h", LatencyMetric: "duration"}
	if err := nonPositiveThreshold.Validate(); err == nil {
		t.Fatal("expected non-positive threshold_ms to be rejected")
	}
}

func TestConfigRequiresServiceAndEnvironment(t *testing.T) {
	cfg := Config{
		SLOs: []Definition{{
			Name: "x", Type: SLITypeRatio, Target: 0.99, Window: "1h",
			GoodMetric: "good", TotalMetric: "total",
		}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected missing service/environment to be rejected")
	}
}
