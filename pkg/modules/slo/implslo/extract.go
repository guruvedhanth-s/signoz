package implslo

import (
	"github.com/SigNoz/signoz/pkg/errors"
	qbtypes "github.com/SigNoz/signoz/pkg/types/querybuildertypes/querybuildertypesv5"
)

// extractScalar pulls a single float value out of a scalar query response.
//
// SLI queries are issued with RequestTypeScalar. A PromQL query yields a
// TimeSeriesData (aggregations -> series -> values); a builder query yields a
// ScalarData (columns/rows). Both shapes are handled.
func extractScalar(resp *qbtypes.QueryRangeResponse) (float64, error) {
	if resp == nil || len(resp.Data.Results) == 0 {
		return 0, errors.New(errors.TypeNotFound, errors.CodeNotFound, "no results in query response")
	}

	switch result := resp.Data.Results[0].(type) {
	case *qbtypes.TimeSeriesData:
		return scalarFromTimeSeries(result)
	case *qbtypes.ScalarData:
		return scalarFromColumns(result)
	default:
		return 0, errors.Newf(errors.TypeInternal, errors.CodeInternal, "unexpected result type %T", result)
	}
}

// scalarFromTimeSeries reads the latest value of the first series of the first
// aggregation, which is how PromQL scalar queries come back.
func scalarFromTimeSeries(ts *qbtypes.TimeSeriesData) (float64, error) {
	if ts == nil || len(ts.Aggregations) == 0 {
		return 0, errors.New(errors.TypeNotFound, errors.CodeNotFound, "no aggregations in result")
	}
	for _, agg := range ts.Aggregations {
		for _, series := range agg.Series {
			values := series.EvaluableValues()
			if len(values) > 0 {
				return values[len(values)-1].Value, nil
			}
		}
	}
	return 0, errors.New(errors.TypeNotFound, errors.CodeNotFound, "no data points in result")
}

// scalarFromColumns reads the value from the first result column ("__result_0"),
// falling back to the last column, of a ScalarData response.
func scalarFromColumns(scalar *qbtypes.ScalarData) (float64, error) {
	if len(scalar.Data) == 0 || len(scalar.Columns) == 0 {
		return 0, errors.New(errors.TypeNotFound, errors.CodeNotFound, "scalar result is empty")
	}

	idx := -1
	for i, col := range scalar.Columns {
		if col != nil && col.Name == "__result_0" {
			idx = i
			break
		}
	}
	if idx == -1 {
		idx = len(scalar.Columns) - 1
	}

	row := scalar.Data[0]
	if idx >= len(row) {
		return 0, errors.New(errors.TypeInternal, errors.CodeInternal, "scalar column index out of range")
	}

	return toFloat64(row[idx])
}

// toFloat64 coerces the loosely-typed scalar cell into a float64.
func toFloat64(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case float32:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case int:
		return float64(n), nil
	case uint64:
		return float64(n), nil
	case nil:
		return 0, errors.New(errors.TypeNotFound, errors.CodeNotFound, "scalar value is null")
	default:
		return 0, errors.Newf(errors.TypeInternal, errors.CodeInternal, "cannot convert %T to float64", v)
	}
}
