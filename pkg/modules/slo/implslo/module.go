package implslo

import (
	"context"

	"github.com/SigNoz/signoz/pkg/modules/slo"
	"github.com/SigNoz/signoz/pkg/querier"
	"github.com/SigNoz/signoz/pkg/types/slotypes"
	"github.com/SigNoz/signoz/pkg/valuer"
)

type module struct {
	querier querier.Querier
	gate    slo.CompletenessGate
}

// NewModule constructs the SLO engine.
//
// querier executes the SLI good/total and latency queries. gate is the
// completeness seam to the Telemetry Health Auditor; pass NewNoopGate until
// Track A is available.
func NewModule(querier querier.Querier, gate slo.CompletenessGate) slo.Module {
	return &module{querier: querier, gate: gate}
}

// ListSLOs returns the evaluated SLOs for an organization.
//
// M0: returns an empty slice. SLI evaluation, budget and burn-rate math, and the
// completeness gate are wired in subsequent milestones.
func (m *module) ListSLOs(_ context.Context, _ valuer.UUID) ([]*slotypes.Report, error) {
	return []*slotypes.Report{}, nil
}
