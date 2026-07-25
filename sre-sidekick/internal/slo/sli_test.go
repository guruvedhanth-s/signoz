package slo

import (
	"testing"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/source"
)

func TestDeriveMetricQueriesScopesRatioByServiceAndEnvironment(t *testing.T) {
	cfg := Config{Service: "support-agent", Environment: "local"}
	definition := Definition{
		Name: "grounded-answers", Type: SLITypeGroundedAnswers, Window: "1h",
		GoodMetric: "agent_grounded_answers_total", TotalMetric: "agent_evaluated_answers_total",
	}
	good, total, err := deriveMetricQueries(cfg, definition)
	if err != nil {
		t.Fatal(err)
	}
	wantFilter := `service_name = 'support-agent' AND environment = 'local'`
	if good.Metric != "agent_grounded_answers_total" || good.Filter != wantFilter {
		t.Fatalf("unexpected good query: %+v", good)
	}
	if total.Metric != "agent_evaluated_answers_total" || total.Filter != wantFilter {
		t.Fatalf("unexpected total query: %+v", total)
	}
	for _, query := range []source.MetricQuery{good, total} {
		if query.TimeAggregation != "increase" || query.SpaceAggregation != "sum" || query.Temporality != "Cumulative" {
			t.Fatalf("unexpected aggregation shape: %+v", query)
		}
	}
}

func TestDeriveMetricQueriesUsesConfiguredLabelsForCompleteness(t *testing.T) {
	cfg := Config{
		Service: "checkout-api", Environment: "test",
		Completeness: &CompletenessConfig{ServiceLabel: "svc", EnvironmentLabel: "env"},
	}
	definition := Definition{
		Name: "success", Type: SLITypeCompleteness, Window: "1h",
		GoodMetric: "good_total", TotalMetric: "total_total",
	}
	good, total, err := deriveMetricQueries(cfg, definition)
	if err != nil {
		t.Fatal(err)
	}
	wantFilter := `svc = 'checkout-api' AND env = 'test'`
	if good.Filter != wantFilter || total.Filter != wantFilter {
		t.Fatalf("completeness queries did not use configured labels: good=%+v total=%+v", good, total)
	}
}

// TestDeriveMetricQueriesBuildsLatencyBucketAndCountQueries locks in the
// default bucket unit, "ms": live-verified against a running SigNoz
// instance that SigNoz's own zero-instrumentation latency histogram
// (spanmetrics-processor "signoz_latency") stores "le" boundaries in
// milliseconds (0.1, 1, 2, 6, 10, 50, 100, 250, 500, 1000, 1400, 2000,
// 5000, 10000, 20000, 40000, 60000, +Inf - a 60000 boundary only makes
// sense as 60 seconds in ms). Previously the code always divided
// ThresholdMS by 1000 (assuming seconds unconditionally): a 1000ms
// threshold queried le='1', which live-matched a real bucket - the "<=1ms"
// one - not the intended "<=1000ms" bucket, so the query never errored but
// silently computed the SLI against the wrong, far stricter cutoff.
func TestDeriveMetricQueriesBuildsLatencyBucketAndCountQueries(t *testing.T) {
	cfg := Config{Service: "checkout-api", Environment: "test"}
	good, total, err := deriveMetricQueries(cfg, Definition{
		Name: "latency", Type: SLITypeLatencyThreshold, Window: "30d",
		LatencyMetric: "request_duration_seconds", ThresholdMS: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if good.Metric != "request_duration_seconds_bucket" {
		t.Fatalf("unexpected good metric: %q", good.Metric)
	}
	wantGoodFilter := `service_name = 'checkout-api' AND environment = 'test' AND le = '1000'`
	if good.Filter != wantGoodFilter {
		t.Fatalf("unexpected good filter: %q, want the threshold in milliseconds unconverted (default unit)", good.Filter)
	}
	if total.Metric != "request_duration_seconds_count" {
		t.Fatalf("unexpected total metric: %q", total.Metric)
	}
	wantTotalFilter := `service_name = 'checkout-api' AND environment = 'test'`
	if total.Filter != wantTotalFilter {
		t.Fatalf("unexpected total filter: %q", total.Filter)
	}
}

// TestDeriveMetricQueriesConvertsToSecondsWhenConfigured covers the "s"
// unit for an OTel semantic-convention histogram (e.g. a custom
// "*_duration_seconds" metric), which does store "le" in seconds.
func TestDeriveMetricQueriesConvertsToSecondsWhenConfigured(t *testing.T) {
	cfg := Config{Service: "checkout-api", Environment: "test"}
	good, _, err := deriveMetricQueries(cfg, Definition{
		Name: "latency", Type: SLITypeLatencyThreshold, Window: "30d",
		LatencyMetric: "request_duration_seconds", ThresholdMS: 1000, LatencyBucketUnit: "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantGoodFilter := `service_name = 'checkout-api' AND environment = 'test' AND le = '1'`
	if good.Filter != wantGoodFilter {
		t.Fatalf("unexpected good filter: %q, want the threshold converted to seconds", good.Filter)
	}
}

func TestDeriveMetricQueriesRejectsEmptyLatencyMetric(t *testing.T) {
	cfg := Config{Service: "checkout-api", Environment: "test"}
	if _, _, err := deriveMetricQueries(cfg, Definition{
		Name: "latency", Type: SLITypeLatencyThreshold, Window: "1h", ThresholdMS: 1000,
	}); err == nil {
		t.Fatal("expected error for empty latency metric")
	}
}

func TestEscapeFilterValueEscapesSingleQuotes(t *testing.T) {
	if got := escapeFilterValue("o'brien"); got != `o\'brien` {
		t.Fatalf("unexpected escaped value: %q", got)
	}
}
