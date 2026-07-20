package implslo

import (
	"context"

	"github.com/SigNoz/signoz/pkg/modules/slo"
	"github.com/SigNoz/signoz/pkg/types/slotypes"
	"github.com/SigNoz/signoz/pkg/valuer"
)

// noopGate is the default CompletenessGate used until the Telemetry Health
// Auditor (Track A) is wired in. It reports every service as fully trustworthy,
// so the SLO engine never short-circuits to indeterminate on its own.
type noopGate struct{}

// NewNoopGate returns a CompletenessGate that always reports full completeness.
func NewNoopGate() slo.CompletenessGate {
	return &noopGate{}
}

func (n *noopGate) Completeness(_ context.Context, _ valuer.UUID, _ string, _ slotypes.Window) (float64, error) {
	return 1.0, nil
}
