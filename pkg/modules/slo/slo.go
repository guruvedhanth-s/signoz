// Package slo is the SLO and Error-Budget Engine.
//
// It reads SLO definitions as code, evaluates SLIs against SigNoz telemetry via
// the querier, computes error budgets and multi-window burn rates, and gates the
// result on telemetry completeness through the CompletenessGate.
//
// This file is the M0 walking skeleton: the interfaces are frozen so the four
// workstreams (SLI evaluation, budget math, gate, frontend) can proceed in
// parallel. The concrete implementation lives in the implslo subpackage.
package slo

import (
	"context"
	"net/http"

	"github.com/SigNoz/signoz/pkg/types/dashboardtypes"
	"github.com/SigNoz/signoz/pkg/types/slotypes"
	"github.com/SigNoz/signoz/pkg/valuer"
)

// Module is the SLO engine's domain interface.
type Module interface {
	// ListSLOs evaluates and returns every configured SLO for an organization.
	ListSLOs(ctx context.Context, orgID valuer.UUID) ([]*slotypes.Report, error)

	// GenerateDashboard creates, or idempotently updates, the SLO dashboard for an
	// organization.
	GenerateDashboard(ctx context.Context, orgID valuer.UUID, createdBy string, creator valuer.UUID) (*dashboardtypes.Dashboard, error)
}

// Handler exposes the SLO engine over HTTP.
type Handler interface {
	// List handles GET /api/v1/slo.
	List(http.ResponseWriter, *http.Request)

	// Generate handles POST /api/v1/slo/generate.
	Generate(http.ResponseWriter, *http.Request)
}

// CompletenessGate is the single integration seam between the SLO engine
// (Track B) and the Telemetry Health Auditor (Track A).
//
// Completeness returns a value in the range 0..1 for a service and window, where
// 1.0 means the telemetry is fully trustworthy. When completeness falls below an
// SLO's configured gate, the SLO short-circuits to StateIndeterminate instead of
// reporting a possibly-false pass.
//
// FREEZE THIS SIGNATURE. Track A implements it via the telemetryhealth module;
// until then implslo.NewNoopGate provides a 1.0 default so Track B is unblocked.
type CompletenessGate interface {
	Completeness(ctx context.Context, orgID valuer.UUID, service string, window slotypes.Window) (float64, error)
}
