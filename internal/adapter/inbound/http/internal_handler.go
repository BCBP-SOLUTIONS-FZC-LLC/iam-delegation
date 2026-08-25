package http

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
)

// ExpiryRunner backs DLG-I1 (POST /internal/delegations/expire — the
// delegation-expiry CronJob's entry point, LLD §11.3). This adapter does
// not itself implement the expiry sweep — that orchestration
// (SELECT …WHERE ends_at < now(); per-row UP pointer-clear;
// RunInTx{UPDATE status=ended; outbox DelegationEnded}) lives in
// cmd/reconciler/jobs (a separate, not-yet-built part of this codebase per
// the task split). Expire is a thin wrapper so cmd/server can wire in
// whatever concrete implementation lands there later without this package
// needing to change.
type ExpiryRunner interface {
	RunExpiry(ctx context.Context) (attempted, succeeded, failed int, err error)
}

// ReviewSweepResult carries the outcome counters from a single DLG-I2 run.
// Using a struct avoids the >5-return-value lint violation (gocritic
// tooManyResultsChecker) while keeping the interface clean.
type ReviewSweepResult struct {
	Warned3d int
	Warned2d int
	Warned1d int
	Expired  int
	Deferred int
	Failed   int
}

// ReviewRunner backs DLG-I2 (POST /internal/delegations/review-sweep —
// the delegation-review CronJob's entry point, LLD §11.4). Same
// thin-wrapper rationale as ExpiryRunner: the daily cascade warn + auto-end
// orchestration lives in cmd/reconciler/jobs, injected here later.
type ReviewRunner interface {
	RunReviewSweep(ctx context.Context) (ReviewSweepResult, error)
}

// InternalHandler implements DLG-I1…I4 — mesh-only, mTLS trust boundary, no
// RBAC/JWT check (LLD §8.2/§13.2). Registered under /internal without
// gincommon.ProtectedMiddlewares (router.go).
type InternalHandler struct {
	expiry ExpiryRunner
	review ReviewRunner
	reader DelegationReader
}

// NewInternalHandler builds the DLG-I1…I4 handler group.
func NewInternalHandler(expiry ExpiryRunner, review ReviewRunner, reader DelegationReader) *InternalHandler {
	return &InternalHandler{expiry: expiry, review: review, reader: reader}
}

// Expire is DLG-I1.
//
// @Summary      DLG-I1 — Run the delegation-expiry sweep
// @Description  Mesh-only. Ends every active fixed-ends_at delegation whose window has passed, UP pointer-clear first (fail-open per-row, DEL-6).
// @Tags         internal
// @Produce      json
// @Success      200  {object}  ExpiryRunResponse
// @Failure      500  {object}  gincommon.ErrorResponse
// @Router       /internal/delegations/expire [post]
func (h *InternalHandler) Expire(c *gin.Context) {
	attempted, succeeded, failed, err := h.expiry.RunExpiry(c.Request.Context())
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, ExpiryRunResponse{Attempted: attempted, Succeeded: succeeded, Failed: failed})
}

// ReviewSweep is DLG-I2.
//
// @Summary      DLG-I2 — Run the delegation review-window sweep
// @Description  Mesh-only. Daily cascade warn pass over open-ended delegations (days_remaining ∈ {3,2,1}), then an auto-end pass for those whose review_due_at has passed.
// @Tags         internal
// @Produce      json
// @Success      200  {object}  ReviewSweepRunResponse
// @Failure      500  {object}  gincommon.ErrorResponse
// @Router       /internal/delegations/review-sweep [post]
func (h *InternalHandler) ReviewSweep(c *gin.Context) {
	res, err := h.review.RunReviewSweep(c.Request.Context())
	if err != nil {
		HandleError(c, err)
		return
	}
	c.JSON(http.StatusOK, ReviewSweepRunResponse(res))
}

// DeptDelegate is DLG-I3 — Core's §8.8.4 department-scope removal precision
// (WFI-11).
//
// @Summary      DLG-I3 — Find the active department-scope delegate for a user
// @Description  Mesh-only. Backs Core's synchronous removal-gate call (LLD §11.5) — never 404; found:false is the valid "no active department delegation" answer.
// @Tags         internal
// @Produce      json
// @Param        tenant_id  query  string  true  "Tenant UUID"      format(uuid)
// @Param        user_id    query  string  true  "User UUID"        format(uuid)
// @Param        dept_id    query  string  true  "Department UUID"  format(uuid)
// @Success      200  {object}  DeptDelegateResponse
// @Failure      400  {object}  gincommon.ErrorResponse
// @Router       /internal/delegations/dept-delegate [get]
func (h *InternalHandler) DeptDelegate(c *gin.Context) {
	tenantID, err := uuid.Parse(c.Query("tenant_id"))
	if err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "tenant_id must be a valid UUID"))
		return
	}
	userID, err := uuid.Parse(c.Query("user_id"))
	if err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "user_id must be a valid UUID"))
		return
	}
	deptID, err := uuid.Parse(c.Query("dept_id"))
	if err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "dept_id must be a valid UUID"))
		return
	}

	d, err := h.reader.FindActiveDeptDelegateForUser(c.Request.Context(), tenantID, userID, deptID)
	if err != nil {
		HandleError(c, err)
		return
	}
	if d == nil {
		c.JSON(http.StatusOK, DeptDelegateResponse{Found: false})
		return
	}
	c.JSON(http.StatusOK, DeptDelegateResponse{
		Found:        true,
		DelegationID: &d.ID,
		DelegatorID:  &d.DelegatorID,
		DelegateID:   &d.DelegateID,
	})
}

// ActiveDelegations is DLG-I4 — the escape hatch replacing I-8's removed
// active_delegations[] field. Unused today (§6.1); provided for any future
// synchronous consumer.
//
// @Summary      DLG-I4 — List a user's active outbound delegations
// @Description  Mesh-only. Returns the delegations the given user has handed out (delegator side, active).
// @Tags         internal
// @Produce      json
// @Param        id         path   string  true  "User UUID"    format(uuid)
// @Param        tenant_id  query  string  true  "Tenant UUID"  format(uuid)
// @Success      200  {object}  ActiveDelegationsResponse
// @Failure      400  {object}  gincommon.ErrorResponse
// @Router       /internal/users/{id}/active-delegations [get]
func (h *InternalHandler) ActiveDelegations(c *gin.Context) {
	userID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "id must be a valid UUID"))
		return
	}
	tenantID, err := uuid.Parse(c.Query("tenant_id"))
	if err != nil {
		HandleError(c, domain.NewError(domain.ErrValidation, "tenant_id must be a valid UUID"))
		return
	}

	list, err := h.reader.FindActiveByDelegator(c.Request.Context(), tenantID, userID)
	if err != nil {
		HandleError(c, err)
		return
	}
	views := make([]ActiveDelegationView, len(list))
	for i, d := range list {
		views[i] = DelegationToActiveView(d)
	}
	c.JSON(http.StatusOK, ActiveDelegationsResponse{ActiveDelegations: views})
}
