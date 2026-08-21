package http

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	gincommon "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

// errorStatusByCode maps every domain.Err* sentinel's wire code to its HTTP
// status per LLD §20's error taxonomy table verbatim. A code absent from
// this map (there should be none) falls back to 500 in HandleError.
var errorStatusByCode = map[string]int{
	// Common / cross-cutting — not in §20's table (that table only lists
	// the delegation-specific codes) but needed for completeness; mapped
	// per §13/§17 convention shared with the sibling IAM services.
	domain.ErrValidation.Error():             http.StatusBadRequest,
	domain.ErrMissingIdentity.Error():        http.StatusUnauthorized,
	domain.ErrInsufficientRole.Error():       http.StatusForbidden,
	domain.ErrOptimisticLockConflict.Error(): http.StatusConflict,

	// 400 — malformed input
	domain.ErrInvalidDelegationScope.Error():           http.StatusBadRequest,
	domain.ErrInvalidDelegationMaxDurationDays.Error(): http.StatusBadRequest,
	domain.ErrInvalidDelegationReviewWindow.Error():    http.StatusBadRequest,

	// 404
	domain.ErrDelegationNotFound.Error(): http.StatusNotFound,

	// 422 — domain-rule validation
	domain.ErrScopeIDRequired.Error():             http.StatusUnprocessableEntity,
	domain.ErrInvalidScopeID.Error():              http.StatusUnprocessableEntity,
	domain.ErrSelfDelegation.Error():              http.StatusUnprocessableEntity,
	domain.ErrInvalidDelegate.Error():             http.StatusUnprocessableEntity,
	domain.ErrDelegateUnavailable.Error():         http.StatusUnprocessableEntity,
	domain.ErrReasonTooLong.Error():               http.StatusUnprocessableEntity,
	domain.ErrDelegationWindowInverted.Error():    http.StatusUnprocessableEntity,
	domain.ErrDelegationWindowTooLong.Error():     http.StatusUnprocessableEntity,
	domain.ErrDelegationStartInPast.Error():       http.StatusUnprocessableEntity,
	domain.ErrDelegationStartTooFarFuture.Error(): http.StatusUnprocessableEntity,
	domain.ErrNotReviewTracked.Error():            http.StatusUnprocessableEntity,
	domain.ErrExtendDaysOutOfRange.Error():        http.StatusUnprocessableEntity,

	// 503 — dependency
	domain.ErrOrgMembershipUnavailable.Error(): http.StatusServiceUnavailable,
	domain.ErrUserProfileUnavailable.Error():   http.StatusServiceUnavailable,
}

// writeError writes the standard gincommon.ErrorResponse envelope — the
// real (flat Error/Status/TraceID/RequestID) shape platform-gincommon
// ships today, verified via `go doc`, not the nested {"error":{...}} shape
// LLD §20's prose example sketches. Mirrors iam-tender-acl's writeError.
func writeError(c *gin.Context, status int, code string) {
	c.AbortWithStatusJSON(status, gincommon.ErrorResponse{
		Error:     code,
		Status:    status,
		TraceID:   gincommon.TraceIDFromContext(c),
		RequestID: gincommon.RequestIDFromContext(c),
	})
}

// HandleError maps err onto the HTTP status from LLD §20 and writes it via
// writeError. Any error that isn't a *domain.Error — or is one with a code
// this adapter doesn't recognize — becomes a generic 500, never leaking
// implementation detail.
func HandleError(c *gin.Context, err error) {
	var derr *domain.Error
	if !errors.As(err, &derr) {
		writeError(c, http.StatusInternalServerError, "internal_server_error")
		return
	}
	status, ok := errorStatusByCode[derr.Code]
	if !ok {
		status = http.StatusInternalServerError
	}
	writeError(c, status, derr.Code)
}
