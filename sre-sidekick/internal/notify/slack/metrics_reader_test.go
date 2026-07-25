package slack

import (
	"context"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// metricReader is the slice of the SDK reader these tests use.
type metricReader interface {
	Collect(ctx context.Context, rm *metricdata.ResourceMetrics) error
}

// testMetrics builds an instrument set backed by an in-memory reader, so a
// test can assert on what was actually recorded without an exporter.
func testMetrics(t *testing.T) (metricReader, *Metrics) {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown meter provider: %v", err)
		}
	})

	metrics, err := NewMetrics(provider.Meter("sre-sidekick-test"))
	if err != nil {
		t.Fatalf("NewMetrics() error = %v", err)
	}
	return reader, metrics
}

// collectSums gathers the data points recorded for one instrument.
func collectSums(t *testing.T, reader metricReader, name string) []metricdata.DataPoint[int64] {
	t.Helper()

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	points := []metricdata.DataPoint[int64]{}
	for _, scope := range collected.ScopeMetrics {
		for _, instrument := range scope.Metrics {
			if instrument.Name != name {
				continue
			}
			sum, ok := instrument.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("metric %q is %T, want an int64 sum", name, instrument.Data)
			}
			points = append(points, sum.DataPoints...)
		}
	}
	return points
}
