package signoz

import (
	"context"
	"fmt"
)

// Scalar runs a single PromQL expression as a scalar query over [startMs, endMs]
// and returns the latest value. It implements slo.ScalarQuerier.
func (c *Client) Scalar(ctx context.Context, expr string, startMs, endMs uint64) (float64, error) {
	req := map[string]any{
		"schemaVersion": "v5",
		"start":         startMs,
		"end":           endMs,
		"requestType":   "scalar",
		"noCache":       true,
		"compositeQuery": map[string]any{
			"queries": []any{
				map[string]any{
					"type": "promql",
					"spec": map[string]any{"name": "__result_0", "query": expr},
				},
			},
		},
	}

	var resp queryRangeResponse
	if err := c.post(ctx, "/api/v5/query_range", req, &resp); err != nil {
		return 0, err
	}
	return resp.scalar()
}

// queryRangeResponse models the parts of the /api/v5/query_range response we
// need. A PromQL scalar query returns a time-series shape (aggregations ->
// series -> values), not a columnar scalar, so we read the latest value.
type queryRangeResponse struct {
	Data struct {
		Data struct {
			Results []struct {
				Aggregations []struct {
					Series []struct {
						Values []struct {
							Value   float64 `json:"value"`
							Partial bool    `json:"partial"`
						} `json:"values"`
					} `json:"series"`
				} `json:"aggregations"`
			} `json:"results"`
		} `json:"data"`
	} `json:"data"`
}

func (r queryRangeResponse) scalar() (float64, error) {
	if len(r.Data.Data.Results) == 0 {
		return 0, fmt.Errorf("no results in query response")
	}
	for _, res := range r.Data.Data.Results {
		for _, agg := range res.Aggregations {
			for _, series := range agg.Series {
				for i := len(series.Values) - 1; i >= 0; i-- {
					if !series.Values[i].Partial {
						return series.Values[i].Value, nil
					}
				}
			}
		}
	}
	return 0, fmt.Errorf("no data points in result")
}
