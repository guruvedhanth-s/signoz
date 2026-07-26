package slo

import (
	"strings"
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

// TestDeriveMetricQueriesDefinitionLabelOverridesWinOverConfigDefaults
// locks in a per-Definition ServiceLabel/EnvironmentLabel override taking
// priority over cfg.MetricLabels() - live-verified necessary because a
// single Config commonly mixes custom-instrumented counters (attribute
// keys like "service_name"/"environment") with SigNoz's own
// spanmetrics-derived metrics (OTel resource semantic-convention keys
// "service.name"/"deployment.environment" instead): a Config-wide default
// cannot serve both within the same file.
func TestDeriveMetricQueriesDefinitionLabelOverridesWinOverConfigDefaults(t *testing.T) {
	cfg := Config{Service: "support-agent", Environment: "local"}
	definition := Definition{
		Name: "span-latency", Type: SLITypeLatencyThreshold, Window: "1h",
		LatencyMetric: "signoz_latency", ThresholdMS: 1000,
		ServiceLabel: "service.name", EnvironmentLabel: "deployment.environment",
	}
	good, total, err := deriveMetricQueries(cfg, definition)
	if err != nil {
		t.Fatal(err)
	}
	wantTotalFilter := `service.name = 'support-agent' AND deployment.environment = 'local'`
	if total.Filter != wantTotalFilter {
		t.Fatalf("total filter = %q, want the per-definition label override, not the config-wide default", total.Filter)
	}
	if !strings.Contains(good.Filter, wantTotalFilter) {
		t.Fatalf("good filter = %q, want it to also use the per-definition label override", good.Filter)
	}
}

// TestDeriveMetricQueriesUsesDeltaTemporalityWhenConfigured locks in the
// fix for a bug where every counter query hardcoded Cumulative
// temporality: SigNoz's own spanmetrics-processor-derived metrics
// (signoz_latency.bucket/.count) are Delta temporality - confirmed live,
// an "increase" aggregation against a Delta metric queried as Cumulative
// returned an empty result set (silent no-data), not an error.
func TestDeriveMetricQueriesUsesDeltaTemporalityWhenConfigured(t *testing.T) {
	cfg := Config{Service: "support-agent", Environment: "local"}
	definition := Definition{
		Name: "span-latency", Type: SLITypeLatencyThreshold, Window: "1h",
		LatencyMetric: "signoz_latency", ThresholdMS: 1000, MetricTemporality: "delta",
	}
	good, total, err := deriveMetricQueries(cfg, definition)
	if err != nil {
		t.Fatal(err)
	}
	if good.Temporality != "Delta" || total.Temporality != "Delta" {
		t.Fatalf("Temporality = %q/%q, want Delta for both queries", good.Temporality, total.Temporality)
	}
}

// TestDeriveMetricQueriesDefaultsToCumulativeTemporality preserves today's
// behavior when MetricTemporality is left unset.
func TestDeriveMetricQueriesDefaultsToCumulativeTemporality(t *testing.T) {
	cfg := Config{Service: "support-agent", Environment: "local"}
	definition := Definition{
		Name: "grounded-answers", Type: SLITypeGroundedAnswers, Window: "1h",
		GoodMetric: "agent_grounded_answers_total", TotalMetric: "agent_evaluated_answers_total",
	}
	good, total, err := deriveMetricQueries(cfg, definition)
	if err != nil {
		t.Fatal(err)
	}
	if good.Temporality != "Cumulative" || total.Temporality != "Cumulative" {
		t.Fatalf("Temporality = %q/%q, want Cumulative by default", good.Temporality, total.Temporality)
	}
}

// grounded_answers shares deriveMetricQueries with ratio - a good/total
// counter ratio is the right math for "fraction of answers that were
// grounded", so it is deliberately not given separate query-building logic.
// completeness is the one SLI type with genuinely different math - see
// TestEvaluateCompleteness* in engine_test.go - so it is no longer part of
// this switch at all (config.Validate no longer requires good_metric/
// total_metric for it either).
func TestDeriveMetricQueriesUsesConfiguredLabelsForGroundedAnswers(t *testing.T) {
	cfg := Config{
		Service: "checkout-api", Environment: "test",
		Completeness: &CompletenessConfig{ServiceLabel: "svc", EnvironmentLabel: "env"},
	}
	definition := Definition{
		Name: "success", Type: SLITypeGroundedAnswers, Window: "1h",
		GoodMetric: "good_total", TotalMetric: "total_total",
	}
	good, total, err := deriveMetricQueries(cfg, definition)
	if err != nil {
		t.Fatal(err)
	}
	wantFilter := `svc = 'checkout-api' AND env = 'test'`
	if good.Filter != wantFilter || total.Filter != wantFilter {
		t.Fatalf("grounded_answers queries did not use configured labels: good=%+v total=%+v", good, total)
	}
}

func TestDeriveMetricQueries_CompletenessIsUnsupported(t *testing.T) {
	_, _, err := deriveMetricQueries(Config{Service: "x", Environment: "y"}, Definition{
		Name: "coverage", Type: SLITypeCompleteness, Window: "1h",
	})
	if err == nil {
		t.Fatal("deriveMetricQueries(completeness) error = nil, want an error - Engine never calls this for completeness (see evaluateCompleteness)")
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

// TestDeriveMetricQueriesUsesExplicitBucketAndCountMetricOverrides locks in
// the fix for SigNoz's own zero-instrumentation latency histogram, whose
// child metrics are dot-separated (signoz_latency.bucket,
// signoz_latency.count) rather than the OTel semantic-convention underscore
// suffix (signoz_latency_bucket, signoz_latency_count) the code derives by
// default - confirmed live: `go run ./cmd/reliability-agent slo` against
// latency_metric: signoz_latency always returned indeterminate before this
// fix, because the derived underscore names do not exist as metrics.
func TestDeriveMetricQueriesUsesExplicitBucketAndCountMetricOverrides(t *testing.T) {
	cfg := Config{Service: "support-agent", Environment: "local"}
	good, total, err := deriveMetricQueries(cfg, Definition{
		Name: "latency", Type: SLITypeLatencyThreshold, Window: "1h",
		LatencyMetric:       "signoz_latency",
		LatencyBucketMetric: "signoz_latency.bucket",
		LatencyCountMetric:  "signoz_latency.count",
		ThresholdMS:         1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if good.Metric != "signoz_latency.bucket" {
		t.Fatalf("good metric = %q, want the explicit override verbatim, not the derived underscore form", good.Metric)
	}
	if total.Metric != "signoz_latency.count" {
		t.Fatalf("total metric = %q, want the explicit override verbatim, not the derived underscore form", total.Metric)
	}
}

// TestDeriveMetricQueriesPartialOverrideLeavesOtherMetricDerived proves the
// two override fields are independent: setting only one does not force the
// other into anything but the default derived form.
func TestDeriveMetricQueriesPartialOverrideLeavesOtherMetricDerived(t *testing.T) {
	cfg := Config{Service: "support-agent", Environment: "local"}
	good, total, err := deriveMetricQueries(cfg, Definition{
		Name: "latency", Type: SLITypeLatencyThreshold, Window: "1h",
		LatencyMetric:       "signoz_latency",
		LatencyBucketMetric: "signoz_latency.bucket",
		ThresholdMS:         1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if good.Metric != "signoz_latency.bucket" {
		t.Fatalf("good metric = %q, want the explicit override", good.Metric)
	}
	if total.Metric != "signoz_latency_count" {
		t.Fatalf("total metric = %q, want the derived underscore form since latency_count_metric was left unset", total.Metric)
	}
}

func TestEscapeFilterValueEscapesSingleQuotes(t *testing.T) {
	if got := escapeFilterValue("o'brien"); got != `o\'brien` {
		t.Fatalf("unexpected escaped value: %q", got)
	}
}
