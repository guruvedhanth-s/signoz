package slo

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/source"
)

type burnMetrics struct{}

func (burnMetrics) ScalarBuilder(_ context.Context, query source.MetricQuery, _, _ uint64) (float64, error) {
	if strings.Contains(query.Metric, "good") {
		return 80, nil
	}
	return 100, nil
}

func TestEvaluateMultiWindowBurnUsesBothTierWindows(t *testing.T) {
	cfg := Config{Service: "checkout-api", Environment: "test", SLOs: []Definition{{
		Name: "success", Type: SLITypeRatio, Target: 0.99, Window: "30d",
		GoodMetric: "good_total", TotalMetric: "total_total",
	}}}
	engine := NewEngine(burnMetrics{}, nil)
	results, err := engine.EvaluateMultiWindow(context.Background(), cfg, time.Now(), []BurnTier{{
		Name: "fast", LongWindow: "1h", ShortWindow: "5m", Threshold: 1, Severity: "page",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].Firing || results[0].LongBurn <= 1 || results[0].ShortBurn <= 1 {
		t.Fatalf("unexpected multi-window result: %+v", results)
	}
}
