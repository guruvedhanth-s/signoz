package slo

import (
	"context"
	"testing"
)

func TestDeriveQueries(t *testing.T) {
	t.Run("ratio uses authored queries", func(t *testing.T) {
		g, tot, err := deriveQueries(SLODefinition{Type: SLITypeRatio, GoodQuery: "g", TotalQuery: "t"})
		if err != nil || g != "g" || tot != "t" {
			t.Fatalf("got %q,%q,%v", g, tot, err)
		}
	})

	t.Run("completeness and grounded_answers use authored queries", func(t *testing.T) {
		for _, ty := range []SLIType{SLITypeCompleteness, SLITypeGroundedAnswers} {
			g, tot, err := deriveQueries(SLODefinition{Type: ty, GoodQuery: "gg", TotalQuery: "tt"})
			if err != nil || g != "gg" || tot != "tt" {
				t.Fatalf("%s: got %q,%q,%v", ty, g, tot, err)
			}
		}
	})

	t.Run("latency builds histogram bucket queries", func(t *testing.T) {
		g, tot, err := deriveQueries(SLODefinition{Type: SLITypeLatencyThreshold, LatencyMetric: "http_duration", ThresholdMs: 3000})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if g != `sum(http_duration_bucket{le="3"})` {
			t.Fatalf("good = %q", g)
		}
		if tot != "sum(http_duration_count)" {
			t.Fatalf("total = %q", tot)
		}
	})
}

func TestEvaluateSLIByType(t *testing.T) {
	ctx := context.Background()

	t.Run("latency_threshold ratio", func(t *testing.T) {
		// 9900 under threshold / 10000 total = 0.99.
		sq := stubScalar{values: map[string]float64{
			`sum(http_duration_bucket{le="3"})`: 9900,
			"sum(http_duration_count)":          10000,
		}}
		got, err := evaluateSLI(ctx, sq, SLODefinition{
			Name: "latency", Type: SLITypeLatencyThreshold, LatencyMetric: "http_duration", ThresholdMs: 3000,
		}, 0, 1)
		if err != nil || got != 0.99 {
			t.Fatalf("got %v, %v; want 0.99", got, err)
		}
	})

	t.Run("grounded_answers ratio", func(t *testing.T) {
		sq := stubScalar{values: map[string]float64{"grounded": 95, "answers": 100}}
		got, err := evaluateSLI(ctx, sq, SLODefinition{
			Name: "grounded", Type: SLITypeGroundedAnswers, GoodQuery: "grounded", TotalQuery: "answers",
		}, 0, 1)
		if err != nil || got != 0.95 {
			t.Fatalf("got %v, %v; want 0.95", got, err)
		}
	})
}

func TestValidateNewTypes(t *testing.T) {
	base := SLODefinition{Name: "x", Target: 0.99, Window: "30d"}

	lat := base
	lat.Type = SLITypeLatencyThreshold
	if err := lat.Validate(); err == nil {
		t.Fatal("latency without metric/threshold should fail")
	}
	lat.LatencyMetric, lat.ThresholdMs = "d", 3000
	if err := lat.Validate(); err != nil {
		t.Fatalf("valid latency rejected: %v", err)
	}

	gr := base
	gr.Type = SLITypeGroundedAnswers
	if err := gr.Validate(); err == nil {
		t.Fatal("grounded without good/total should fail")
	}
}
