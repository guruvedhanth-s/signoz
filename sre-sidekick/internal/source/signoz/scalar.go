package signoz

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/source"
)

// Scalar issues a single PromQL scalar read against SigNoz. It remains
// available as a general-purpose escape hatch, but the SLO engine no
// longer uses it: PromQL label matchers against these OTel metric
// attributes have proven unreliable against live SigNoz - sometimes
// returning no data, sometimes returning a value from the wrong series -
// so every SLO metric read goes through ScalarBuilder instead.
func (c *Client) Scalar(ctx context.Context, expression string, startMs, endMs uint64) (float64, error) {
	request := map[string]any{
		"schemaVersion": "v5",
		"start":         startMs,
		"end":           endMs,
		"requestType":   "scalar",
		"noCache":       true,
		"compositeQuery": map[string]any{
			"queries": []any{
				map[string]any{
					"type": "promql",
					"spec": map[string]any{
						"name":  "__result_0",
						"query": expression,
					},
				},
			},
		},
	}
	var response queryRangeResponse
	if err := c.QueryRange(ctx, request, &response); err != nil {
		return 0, err
	}
	return response.scalar()
}

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
		return 0, fmt.Errorf("no results in SigNoz query response")
	}
	for _, result := range r.Data.Data.Results {
		for _, aggregation := range result.Aggregations {
			for _, series := range aggregation.Series {
				for i := len(series.Values) - 1; i >= 0; i-- {
					if !series.Values[i].Partial {
						return series.Values[i].Value, nil
					}
				}
			}
		}
	}
	return 0, fmt.Errorf("only partial or empty values in SigNoz query response")
}

// ScalarBuilder issues a single SigNoz builder-query scalar read for
// query, reduced to one number over [startMs, endMs]. Builder queries,
// unlike PromQL, filter correctly on the OTel metric attributes SLOs are
// scoped by, which is why the SLO engine and completeness gate use this
// instead of Scalar.
//
// SigNoz's v5 "scalar" request type still buckets metric aggregations into
// steps (60s by default, coarser for long windows) instead of collapsing
// the whole range into one point, so the aggregation spec sets reduceTo
// "sum": summing the per-step values across the window yields the total
// for the whole range (an "increase" time aggregation summed per-step
// gives the total increase; a "count" time aggregation summed per-step
// gives the total sample count, which is what the completeness gate uses
// to check presence). Once reduceTo is set, the response comes back in
// SigNoz's ScalarData shape (aggregation columns plus data rows), not the
// TimeSeriesData shape Scalar parses, so it needs its own extraction.
func (c *Client) ScalarBuilder(ctx context.Context, query source.MetricQuery, startMs, endMs uint64) (float64, error) {
	value, _, err := c.ScalarBuilderWarning(ctx, query, startMs, endMs)
	return value, err
}

// ScalarBuilderWarning is ScalarBuilder plus SigNoz's own top-level query
// warning (PRD section 11.2: "preserve SigNoz query-completeness
// metadata"), read from the v5 response's data.warning.message - the only
// completeness-adjacent signal that field carries for a scalar builder
// query. Confirmed against the actual response type
// (querybuildertypesv5.QueryRangeResponse.Warning): SigNoz populates it
// for step-interval clamping and metrics that have gone dormant, among
// other query-shape adjustments; there is no per-row/per-series partial
// flag on the scalar shape itself (unlike the time-series shape's
// TimeSeriesValue.Partial, which the PromQL-based Scalar method already
// reads) - so a per-value flag was never a gap that had a field to parse.
func (c *Client) ScalarBuilderWarning(ctx context.Context, query source.MetricQuery, startMs, endMs uint64) (float64, string, error) {
	timeAggregation := query.TimeAggregation
	if timeAggregation == "" {
		timeAggregation = "increase"
	}
	spaceAggregation := query.SpaceAggregation
	if spaceAggregation == "" {
		spaceAggregation = "sum"
	}
	temporality := query.Temporality
	if temporality == "" {
		temporality = "Cumulative"
	}
	request := map[string]any{
		"schemaVersion": "v5",
		"start":         startMs,
		"end":           endMs,
		"requestType":   "scalar",
		"noCache":       true,
		"compositeQuery": map[string]any{
			"queries": []any{
				map[string]any{
					"type": "builder_query",
					"spec": map[string]any{
						"name":   "A",
						"signal": "metrics",
						"aggregations": []any{
							map[string]any{
								"metricName":       query.Metric,
								"temporality":      temporality,
								"timeAggregation":  timeAggregation,
								"spaceAggregation": spaceAggregation,
								"reduceTo":         "sum",
							},
						},
						"filter": map[string]any{"expression": query.Filter},
					},
				},
			},
		},
	}
	var response scalarBuilderResponse
	if err := c.QueryRange(ctx, request, &response); err != nil {
		return 0, "", err
	}
	value, err := response.scalar()
	if err != nil {
		return 0, "", err
	}
	warning := ""
	if response.Data.Warning != nil {
		warning = response.Data.Warning.Message
	}
	return value, warning, nil
}

// scalarBuilderResponse parses SigNoz's ScalarData response shape:
// aggregation columns plus data rows. A metric with no samples in the
// window (never instrumented, or a real zero increase) comes back with no
// columns and no rows; that is a valid answer of 0, not an error - see
// MetricQuerier's documented contract. The SLO engine's total<=0 check is
// what turns a genuinely absent total metric into ErrNoData.
//
// Status is SigNoz's application-level result ("success" or "error"),
// distinct from the HTTP status code requestJSON already checks: a
// malformed query or an internal query-engine failure can still come back
// as HTTP 200 with Status "error" and an empty/absent Results, which would
// otherwise silently parse as a valid answer of 0 - indistinguishable from
// a metric that genuinely has no samples. scalar() treats that as an
// error rather than a zero.
//
// A data row is not guaranteed to be all-float: a query with an "le"
// filter (a latency_threshold SLI's bucket query) still comes back with
// "le" as its own "group"-typed column alongside the "aggregation" column
// - SigNoz keeps histogram bucket boundaries as an explicit group even
// when the filter pins it to one value, confirmed live against a running
// SigNoz instance (a query filtered to le='1000' still returned a row
// shaped ["1000", <count>], not just [<count>]). Decoding Data as
// [][]float64 fails outright the moment any row contains that group
// value, so each cell is decoded individually and only "aggregation"-typed
// columns are read as numbers.
type scalarBuilderResponse struct {
	Status string `json:"status"`
	Data   struct {
		// Warning is the v5 response's top-level completeness signal
		// (querybuildertypesv5.QueryRangeResponse.Warning) - populated for
		// step-interval clamping, dormant metrics, and similar query-shape
		// adjustments SigNoz made on our behalf.
		Warning *struct {
			Message string `json:"message"`
		} `json:"warning,omitempty"`
		Data struct {
			Results []struct {
				Columns []struct {
					ColumnType string `json:"columnType"`
				} `json:"columns"`
				Data [][]json.RawMessage `json:"data"`
			} `json:"results"`
		} `json:"data"`
	} `json:"data"`
}

func (r scalarBuilderResponse) scalar() (float64, error) {
	if r.Status != "" && r.Status != "success" {
		return 0, fmt.Errorf("SigNoz scalar builder query returned status %q", r.Status)
	}
	var sum float64
	for _, result := range r.Data.Data.Results {
		for _, row := range result.Data {
			for i, column := range result.Columns {
				if column.ColumnType != "aggregation" || i >= len(row) {
					continue
				}
				var value float64
				if err := json.Unmarshal(row[i], &value); err != nil {
					return 0, fmt.Errorf("decode SigNoz scalar aggregation value: %w", err)
				}
				sum += value
			}
		}
	}
	return sum, nil
}
