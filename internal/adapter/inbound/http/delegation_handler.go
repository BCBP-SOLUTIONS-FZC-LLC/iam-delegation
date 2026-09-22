package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/pkg/requestctx"
)

// DelegationService is the subset of *service.DelegationService's exported
// surface this adapter calls — declared locally so *_handler_test.go can
// fake it without constructing the real service's port dependencies
// (repository, membership-check client, User Profile client, idempotency
// store, cache, tx runner). *service.DelegationService satisfies this
// interface structurally; no changes to the service package were needed.
type DelegationService interface {
	List(ctx context.Context, tenantID, delegatorID uuid.UUID) ([]domain.Delegation, error)
	Create(ctx context.Context, tenantID, delegatorID uuid.UUID, idempotencyKey string, req service.CreateInput) (*domain.Delegation, error)
	Cancel(ctx context.Context, tenantID, id uuid.UUID, expectedVersion int64) (*domain.Delegation, error)
	Extend(ctx context.Context, tenantID, id uuid.UUID, extendDays *int, expectedVersion int64) (*domain.Delegation, error)
	Reassign(ctx context.Context, tenantID, id uuid.UUID, expectedVersion int64, req service.ReassignInput) (*domain.Delegation, error)
}

// DelegationHandler implements DLG-1…5 (LLD §8.4).
type DelegationHandler struct {
	svc    DelegationService
	reader DelegationReader
}

// NewDelegationHandler builds a DelegationHandler. reader backs the
// self-or-admin authorization check on Cancel/Extend/Reassign (authz.go) —
// typically the same concrete postgres repository wired into svc.
func NewDelegationHandler(svc DelegationService, reader DelegationReader) *DelegationHandler {
	return &DelegationHandler{svc: svc, reader: reader}
}

func identityOrError(c *gin.Context) (*requestctx.Context, bool) {
	rc, ok := requestctx.FromContext(c.Request.Context())
	if !ok {
		HandleError(c, domain.NewError(domain.ErrMissingIdentity, "missing identity headers"))
		return nil, false
	}
	return rc, true
}

func parseUUIDParam(c *gin.Context, name string) (uuid.UUID, bool) {
	v, err := uuid.Parse(c.Param(name))
	if err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, name+" must be a valid UUID"))
		return uuid.UUID{}, false
	}
	return v, true
}

// List is DLG-1 — the caller's own active delegations (RLS-scoped).
//
// @Summary      DLG-1 — List active delegations
// @Description  Returns the caller's own active delegations (RLS-scoped, LLD §9.1).
// @Tags         delegations
// @Produce      json
// @Success      200  {object}  DelegationListResponse
// @Failure      401  {object}  gincommon.ErrorResponse
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /api/v1/delegations [get]
func (h *DelegationHandler) List(c *gin.Context) {
	rc, ok := identityOrError(c)
	if !ok {
		return
	}
	tenantID, err := uuid.Parse(rc.TenantID)
	if err != nil {
		HandleError(c, domain.NewError(domain.ErrMissingIdentity, "invalid tenant identity"))
		return
	}
	delegatorID, err := uuid.Parse(rc.UserID)
	if err != nil {
		HandleError(c, domain.NewError(domain.ErrMissingIdentity, "invalid user identity"))
		return
	}
	list, err := h.svc.List(c.Request.Context(), tenantID, delegatorID)
	if err != nil {
		HandleError(c, err)
		return
	}
	items := make([]DelegationResponse, len(list))
	for i, d := range list {
		items[i] = DelegationToResponse(d)
	}
	c.JSON(http.StatusOK, DelegationListResponse{Items: items})
}

// Create is DLG-2 — self-service OOO delegation (availability-first).
//
// @Summary      DLG-2 — Create OOO delegation
// @Description  Availability-first (LLD §11.1): calls User Profile SetAvailability BEFORE inserting the delegations row. Requires the Idempotency-Key header (DLG-Q3) — a repeat within 24h returns the original 201.
// @Tags         delegations
// @Accept       json
// @Produce      json
// @Param        Idempotency-Key  header    string                   true  "Idempotency key, required"
// @Param        request          body      DelegationCreateRequest  true  "Delegation payload"
// @Success      201  {object}  DelegationResponse
// @Failure      400  {object}  gincommon.ErrorResponse
// @Failure      422  {object}  gincommon.ErrorResponse  "self_delegation | invalid_delegate | delegate_unavailable | scope_id_required | invalid_scope_id | reason_too_long | delegation_window_inverted | delegation_window_too_long | delegation_start_in_past | delegation_start_too_far_future"
// @Failure      503  {object}  gincommon.ErrorResponse  "org_membership_unavailable | user_profile_unavailable | catalog_admin_unavailable"
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /api/v1/delegations [post]
func (h *DelegationHandler) Create(c *gin.Context) {
	rc, ok := identityOrError(c)
	if !ok {
		return
	}
	tenantID, err := uuid.Parse(rc.TenantID)
	if err != nil {
		HandleError(c, domain.NewError(domain.ErrMissingIdentity, "invalid tenant identity"))
		return
	}
	delegatorID, err := uuid.Parse(rc.UserID)
	if err != nil {
		HandleError(c, domain.NewError(domain.ErrMissingIdentity, "invalid user identity"))
		return
	}

	var req DelegationCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
		return
	}

	idempotencyKey := c.GetHeader("Idempotency-Key")
	d, err := h.svc.Create(c.Request.Context(), tenantID, delegatorID, idempotencyKey, service.CreateInput{
		DelegateID: req.DelegateID,
		Scope:      req.Scope,
		ScopeID:    req.ScopeID,
		Reason:     req.OOONote,
		StartsAt:   req.StartsAt,
		EndsAt:     req.EndsAt,
	})
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusCreated, DelegationToResponse(*d))
}

// Cancel is DLG-3 — self or admin. record_version is a query param, per the
// old P-20 shape this route is byte-compatible with (LLD §8.4/§8.1).
//
// @Summary      DLG-3 — Cancel delegation
// @Description  Pointer-clear-only, fail-open on User Profile (LLD §11.2): calls UP {delegate_id:null} first, then flips status to 'cancelled'. Self-service; tenant_admin/tenant_owner can cancel any.
// @Tags         delegations
// @Produce      json
// @Param        id              path      string   true   "Delegation UUID"  format(uuid)
// @Param        record_version  query     integer  false  "Optimistic-lock version"
// @Success      200  {object}  DelegationResponse
// @Failure      403  {object}  gincommon.ErrorResponse
// @Failure      404  {object}  gincommon.ErrorResponse
// @Failure      409  {object}  gincommon.ErrorResponse  "optimistic_lock_conflict"
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /api/v1/delegations/{id} [delete]
func (h *DelegationHandler) Cancel(c *gin.Context) {
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	rc, ok := identityOrError(c)
	if !ok {
		return
	}
	tenantID, err := uuid.Parse(rc.TenantID)
	if err != nil {
		HandleError(c, domain.NewError(domain.ErrMissingIdentity, "invalid tenant identity"))
		return
	}
	if _, ok := requireSelfOrAdmin(c, h.reader, tenantID, id); !ok {
		return
	}

	//nolint:errcheck // a malformed/missing record_version defaults to 0,
	// which the optimistic-lock check below rejects on its own merits
	// (matches the shipped O&M behavior this handler was lifted from).
	v, _ := strconv.ParseInt(c.Query("record_version"), 10, 64)
	d, err := h.svc.Cancel(c.Request.Context(), tenantID, id, v)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, DelegationToResponse(*d))
}

// Extend is DLG-4 — self or admin. Pushes review_due_at forward; open-ended
// delegations only.
//
// @Summary      DLG-4 — Extend delegation review window
// @Description  Pushes review_due_at forward and resets review_last_warned_bucket to NULL, re-arming the 3-day daily cascade. Open-ended delegations only (422 not_review_tracked otherwise).
// @Tags         delegations
// @Accept       json
// @Produce      json
// @Param        id       path  string                   true  "Delegation UUID"  format(uuid)
// @Param        request  body  DelegationExtendRequest  true  "extend_days + record_version"
// @Success      200  {object}  DelegationExtendResponse
// @Failure      403  {object}  gincommon.ErrorResponse
// @Failure      404  {object}  gincommon.ErrorResponse
// @Failure      409  {object}  gincommon.ErrorResponse  "optimistic_lock_conflict"
// @Failure      422  {object}  gincommon.ErrorResponse  "not_review_tracked | extend_days_out_of_range"
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /api/v1/delegations/{id}/extend [post]
func (h *DelegationHandler) Extend(c *gin.Context) {
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	rc, ok := identityOrError(c)
	if !ok {
		return
	}
	tenantID, err := uuid.Parse(rc.TenantID)
	if err != nil {
		HandleError(c, domain.NewError(domain.ErrMissingIdentity, "invalid tenant identity"))
		return
	}
	if _, ok := requireSelfOrAdmin(c, h.reader, tenantID, id); !ok {
		return
	}

	var req DelegationExtendRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
		return
	}
	d, err := h.svc.Extend(c.Request.Context(), tenantID, id, req.ExtendDays, req.RecordVersion)
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, DelegationToExtendResponse(*d))
}

// Reassign is DLG-5 — self or admin. Ends the current delegation and
// creates a new one (fuller body, DLG-D11/DLG-Q7).
//
// @Summary      DLG-5 — Reassign delegation to a new delegate
// @Description  Ends the current delegation (DelegationEnded{cancelled}) then creates a new one via the full DLG-2 pre-flight, including both membership checks. All body fields optional, each defaulting to the current delegation's value. On a Create failure after the old one is ended, the old delegation is NOT resurrected.
// @Tags         delegations
// @Accept       json
// @Produce      json
// @Param        id       path  string                     true  "Delegation UUID"  format(uuid)
// @Param        request  body  DelegationReassignRequest  true  "Reassignment payload"
// @Success      201  {object}  DelegationResponse
// @Failure      400  {object}  gincommon.ErrorResponse
// @Failure      403  {object}  gincommon.ErrorResponse
// @Failure      404  {object}  gincommon.ErrorResponse
// @Failure      422  {object}  gincommon.ErrorResponse  "invalid_delegate | delegate_unavailable | self_delegation | scope_id_required | invalid_scope_id"
// @Security     UserID
// @Security     TenantID
// @Security     TenantRoles
// @Router       /api/v1/delegations/{id}/reassign [post]
func (h *DelegationHandler) Reassign(c *gin.Context) {
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	rc, ok := identityOrError(c)
	if !ok {
		return
	}
	tenantID, err := uuid.Parse(rc.TenantID)
	if err != nil {
		HandleError(c, domain.NewError(domain.ErrMissingIdentity, "invalid tenant identity"))
		return
	}
	if _, ok := requireSelfOrAdmin(c, h.reader, tenantID, id); !ok {
		return
	}

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "unable to read request body"))
		return
	}
	var req DelegationReassignRequest
	if err := json.Unmarshal(body, &req); err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "invalid request body"))
		return
	}
	// Raw-key presence detection (LLD §8.4 DLG-5, service.ReassignInput's
	// doc comment): a plain nil-pointer check on the decoded struct cannot
	// distinguish "ends_at omitted" (keep the existing value) from
	// "ends_at present and null" (explicitly open-ended) — nor "scope_id
	// omitted" from "scope_id present and null" (explicitly clear it for a
	// scope='all' reassignment). Re-decoding into a raw key map answers
	// that; json.Unmarshal already validated the body above so this
	// second decode cannot fail.
	var rawFields map[string]json.RawMessage
	//nolint:errcheck // body was already successfully decoded into req
	// above via ShouldBindJSON — re-unmarshaling the same bytes into a raw
	// key map cannot fail.
	_ = json.Unmarshal(body, &rawFields)
	_, endsAtProvided := rawFields["ends_at"]
	_, scopeIDSet := rawFields["scope_id"]

	d, err := h.svc.Reassign(c.Request.Context(), tenantID, id, req.RecordVersion, service.ReassignInput{
		NewDelegateID:  req.NewDelegateID,
		Scope:          req.Scope,
		ScopeID:        req.ScopeID,
		ScopeIDSet:     scopeIDSet,
		EndsAt:         req.EndsAt,
		EndsAtProvided: endsAtProvided,
		Reason:         req.Reason,
	})
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusCreated, DelegationToResponse(*d))
}
