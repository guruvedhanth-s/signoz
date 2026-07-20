package implslo

import (
	"net/http"

	"github.com/SigNoz/signoz/pkg/http/render"
	"github.com/SigNoz/signoz/pkg/modules/slo"
	"github.com/SigNoz/signoz/pkg/types/authtypes"
	"github.com/SigNoz/signoz/pkg/valuer"
)

type handler struct {
	module slo.Module
}

// NewHandler constructs the SLO HTTP handler.
func NewHandler(module slo.Module) slo.Handler {
	return &handler{module: module}
}

// List handles GET /api/v1/slo and returns every evaluated SLO for the caller's
// organization.
func (h *handler) List(rw http.ResponseWriter, r *http.Request) {
	claims, err := authtypes.ClaimsFromContext(r.Context())
	if err != nil {
		render.Error(rw, err)
		return
	}

	reports, err := h.module.ListSLOs(r.Context(), valuer.MustNewUUID(claims.OrgID))
	if err != nil {
		render.Error(rw, err)
		return
	}

	render.Success(rw, http.StatusOK, reports)
}

// Generate handles POST /api/v1/slo/generate and creates or updates the SLO
// dashboard for the caller's organization.
func (h *handler) Generate(rw http.ResponseWriter, r *http.Request) {
	claims, err := authtypes.ClaimsFromContext(r.Context())
	if err != nil {
		render.Error(rw, err)
		return
	}

	dash, err := h.module.GenerateDashboard(
		r.Context(),
		valuer.MustNewUUID(claims.OrgID),
		claims.Email,
		valuer.MustNewUUID(claims.IdentityID()),
	)
	if err != nil {
		render.Error(rw, err)
		return
	}

	render.Success(rw, http.StatusOK, dash)
}
