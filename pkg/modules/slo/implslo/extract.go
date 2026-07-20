package implslo

import (
	"github.com/SigNoz/signoz/pkg/errors"
	qbtypes "github.com/SigNoz/signoz/pkg/types/querybuildertypes/querybuildertypesv5"
)

// extractScalar pulls a single float value out of a scalar query response.
//
// SLI queries are issued with RequestTypeScalar, so the first result is a
// ScalarData with one row. The value is read from the first result column
// ("__result_0"), falling back to the last column when that name is absent.
func extractScalar(resp *qbtypes.QueryRangeResponse) (float64, error) {
	if resp == nil || len(resp.Data.Results) == 0 {
		return 0, errors.New(errors.TypeNotFound, errors.CodeNotFound, "no results in query response")
	}

	scalar, ok := resp.Data.Results[0].(*qbtypes.ScalarData)
	if !ok {
		return 0, errors.New(errors.TypeInternal, errors.CodeInternal, "unexpected result type, want scalar")
	}
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
