package http

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
)

// SettingsService is the subset of *service.SettingsService's exported
// surface this adapter calls — declared locally so tests can fake it
// without the real settings repository. *service.SettingsService satisfies
// this interface structurally.
type SettingsService interface {
	Get(ctx context.Context, tenantID uuid.UUID) (domain.DelegationTenantSettings, error)
	Set(ctx context.Context, tenantID uuid.UUID, maxDurationDays, reviewWindowDays int) (domain.DelegationTenantSettings, error)
}

// SettingsHandler implements DLG-6/7 (LLD §8.4).
type SettingsHandler struct {
	svc SettingsService
}

// NewSettingsHandler builds the DLG-6/7 handler.
func NewSettingsHandler(svc SettingsService) *SettingsHandler {
	return &SettingsHandler{svc: svc}
}

// Get is DLG-6 — any tenant member.
//
// @Summary      DLG-6 — Get tenant delegation policy
// @Description  Returns the 90/90 default when the tenant has no persisted delegation_tenant_settings row.
// @Tags         delegations
// @Produce      json
// @Success      200  {object}  SettingsResponse
// @Failure      401  {object}  gincommon.ErrorResponse
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /api/v1/delegations/settings [get]
func (h *SettingsHandler) Get(c *gin.Context) {
	rc, ok := identityOrError(c)
	if !ok {
		return
	}
	tenantID, err := uuid.Parse(rc.TenantID)
	if err != nil {
		HandleError(c, domain.NewError(domain.ErrMissingIdentity, "invalid tenant identity"))
		return
	}
	s, err := h.svc.Get(c.Request.Context(), tenantID)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, SettingsToResponse(s))
}

// Set is DLG-7 — tenant_admin/tenant_owner only (RequireAnyRole at the
// router level).
//
// @Summary      DLG-7 — Set tenant delegation policy
// @Description  Both fields required, each in [1,180]. Upserts the tenant's delegation_tenant_settings row.
// @Tags         delegations
// @Accept       json
// @Produce      json
// @Param        request  body  SettingsSetRequest  true  "Policy payload"
// @Success      200  {object}  SettingsResponse
// @Failure      400  {object}  gincommon.ErrorResponse  "invalid_delegation_max_duration_days | invalid_delegation_review_window_days"
// @Failure      403  {object}  gincommon.ErrorResponse
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /api/v1/delegations/settings [put]
func (h *SettingsHandler) Set(c *gin.Context) {
	rc, ok := identityOrError(c)
	if !ok {
		return
	}
	tenantID, err := uuid.Parse(rc.TenantID)
	if err != nil {
		HandleError(c, domain.NewError(domain.ErrMissingIdentity, "invalid tenant identity"))
		return
	}
	var req SettingsSetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
		return
	}
	s, err := h.svc.Set(c.Request.Context(), tenantID, req.MaxDurationDays, req.ReviewWindowDays)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, SettingsToResponse(s))
}
