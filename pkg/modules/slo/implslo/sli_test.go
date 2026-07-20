package implslo

import (
	"context"
	"errors"
	"testing"

	"github.com/SigNoz/signoz/pkg/types/slotypes"
	"github.com/SigNoz/signoz/pkg/valuer"
)

// stubScalarQuerier returns canned values keyed by the query expression, so
// evaluateRatio can be tested without any real querier.
type stubScalarQuerier struct {
	values map[string]float64
	err    error
}

func (s stubScalarQuerier) Scalar(_ context.Context, _ valuer.UUID, expr string, _, _ uint64) (float64, error) {
	if s.err != nil {
		return 0, s.err
	}
	return s.values[expr], nil
}

func ratioDef() slotypes.SLODefinition {
	return slotypes.SLODefinition{
		Name:       "successful-agent-runs",
		Type:       slotypes.SLITypeRatio,
		Target:     0.99,
		Window:     slotypes.Window("30d"),
		GoodQuery:  "good",
		TotalQuery: "total",
	}
}

func TestEvaluateRatio(t *testing.T) {
	tests := []struct {
		name    string
		values  map[string]float64
		err     error
		want    float64
		wantErr bool
	}{
		{
			name:   "healthy",
			values: map[string]float64{"good": 990, "total": 1000},
			want:   0.99,
		},
		{
			name:   "unhealthy",
			values: map[string]float64{"good": 900, "total": 1000},
			want:   0.90,
		},
		{
			name:    "zero total is not a silent pass",
			values:  map[string]float64{"good": 0, "total": 0},
			wantErr: true,
		},
		{
			name:   "good clamped to total",
			values: map[string]float64{"good": 1200, "total": 1000},
			want:   1.0,
		},
		{
			name:   "negative good clamped to zero",
			values: map[string]float64{"good": -5, "total": 1000},
			want:   0.0,
		},
		{
			name:    "query error propagates",
			err:     errors.New("clickhouse down"),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sq := stubScalarQuerier{values: tt.values, err: tt.err}
			got, err := evaluateRatio(context.Background(), sq, valuer.GenerateUUID(), ratioDef(), 0, 1000)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got sli=%v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("sli = %v, want %v", got, tt.want)
			}
		})
	}
}
