package implslo

import (
	"testing"

	qbtypes "github.com/SigNoz/signoz/pkg/types/querybuildertypes/querybuildertypesv5"
)

func scalarResp(cols []string, row []any) *qbtypes.QueryRangeResponse {
	descriptors := make([]*qbtypes.ColumnDescriptor, len(cols))
	for i, name := range cols {
		descriptors[i] = &qbtypes.ColumnDescriptor{QueryName: name}
		descriptors[i].Name = name
	}
	return &qbtypes.QueryRangeResponse{
		Data: qbtypes.QueryData{
			Results: []any{
				&qbtypes.ScalarData{
					QueryName: "__result_0",
					Columns:   descriptors,
					Data:      [][]any{row},
				},
			},
		},
	}
}

func TestExtractScalar(t *testing.T) {
	t.Run("reads __result_0 column", func(t *testing.T) {
		resp := scalarResp([]string{"__result_0"}, []any{float64(990)})
		got, err := extractScalar(resp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != 990 {
			t.Fatalf("got %v, want 990", got)
		}
	})

	t.Run("coerces int64", func(t *testing.T) {
		resp := scalarResp([]string{"__result_0"}, []any{int64(42)})
		got, err := extractScalar(resp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != 42 {
			t.Fatalf("got %v, want 42", got)
		}
	})

	t.Run("empty results errors", func(t *testing.T) {
		resp := &qbtypes.QueryRangeResponse{Data: qbtypes.QueryData{Results: []any{}}}
		if _, err := extractScalar(resp); err == nil {
			t.Fatal("expected error for empty results")
		}
	})

	t.Run("nil response errors", func(t *testing.T) {
		if _, err := extractScalar(nil); err == nil {
			t.Fatal("expected error for nil response")
		}
	})
}

func TestExtractScalarFromTimeSeries(t *testing.T) {
	// PromQL scalar queries return a TimeSeriesData: read the latest value.
	resp := &qbtypes.QueryRangeResponse{
		Data: qbtypes.QueryData{
			Results: []any{
				&qbtypes.TimeSeriesData{
					QueryName: "__result_0",
					Aggregations: []*qbtypes.AggregationBucket{
						{
							Series: []*qbtypes.TimeSeries{
								{Values: []*qbtypes.TimeSeriesValue{
									{Timestamp: 1, Value: 9000},
									{Timestamp: 2, Value: 10000},
								}},
							},
						},
					},
				},
			},
		},
	}
	got, err := extractScalar(resp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 10000 {
		t.Fatalf("got %v, want 10000 (latest value)", got)
	}

	t.Run("empty series errors", func(t *testing.T) {
		empty := &qbtypes.QueryRangeResponse{Data: qbtypes.QueryData{Results: []any{
			&qbtypes.TimeSeriesData{Aggregations: []*qbtypes.AggregationBucket{{Series: nil}}},
		}}}
		if _, err := extractScalar(empty); err == nil {
			t.Fatal("expected error for empty series")
		}
	})
}
