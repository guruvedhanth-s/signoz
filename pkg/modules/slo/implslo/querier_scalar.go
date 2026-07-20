package implslo

import (
	"context"

	"github.com/SigNoz/signoz/pkg/querier"
	qbtypes "github.com/SigNoz/signoz/pkg/types/querybuildertypes/querybuildertypesv5"
	"github.com/SigNoz/signoz/pkg/valuer"
)

// querierScalarQuerier is the production ScalarQuerier. It treats each SLI
// expression as a PromQL query, executes it as a scalar request through the
// shared querier, and extracts the single resulting value.
type querierScalarQuerier struct {
	querier querier.Querier
}

// NewScalarQuerier wraps the shared querier as a ScalarQuerier for SLI evaluation.
func NewScalarQuerier(q querier.Querier) ScalarQuerier {
	return &querierScalarQuerier{querier: q}
}

func (s *querierScalarQuerier) Scalar(ctx context.Context, orgID valuer.UUID, expr string, startMs, endMs uint64) (float64, error) {
	req := buildPromQLScalarRequest(expr, startMs, endMs)
	if err := req.Validate(); err != nil {
		return 0, err
	}

	resp, err := s.querier.QueryRange(ctx, orgID, req)
	if err != nil {
		return 0, err
	}

	return extractScalar(resp)
}

// buildPromQLScalarRequest assembles a scalar QueryRangeRequest for a single
// PromQL expression over [startMs, endMs].
func buildPromQLScalarRequest(expr string, startMs, endMs uint64) *qbtypes.QueryRangeRequest {
	return &qbtypes.QueryRangeRequest{
		SchemaVersion: "v5",
		Start:         startMs,
		End:           endMs,
		RequestType:   qbtypes.RequestTypeScalar,
		CompositeQuery: qbtypes.CompositeQuery{
			Queries: []qbtypes.QueryEnvelope{
				{
					Type: qbtypes.QueryTypePromQL,
					Spec: qbtypes.PromQuery{
						Name:  "__result_0",
						Query: expr,
					},
				},
			},
		},
		NoCache: true,
	}
}
