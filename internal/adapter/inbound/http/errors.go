package http

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
	gincommon "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
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

	// 409
	domain.ErrIdempotencyKeyInFlight.Error(): http.StatusConflict,

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
	domain.ErrDBUnavailable.Error():            http.StatusServiceUnavailable,
}

// errorLogger is the shared gincommon Zap sink, set once by NewRouter from
// ginCfg.Logger — the same port.Logger ObservabilityMiddlewares uses.
// Unhandled 500s log through it so they never bypass platform-gincommon
// (iam-org-membership / iam-realm-provisioner HandleError).
var errorLogger port.Logger

// errorResponseWithDetails extends gincommon.ErrorResponse with a free-form
// details map. Used instead of gincommon.ErrorResponse when domain.Error
// carries non-nil Details (e.g. record_version on ErrOptimisticLockConflict).
type errorResponseWithDetails struct {
	Error     string         `json:"error"`
	Status    int            `json:"status"`
	TraceID   string         `json:"trace_id,omitempty"`
	RequestID string         `json:"request_id,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
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

// writeErrorWithDetails writes the error envelope, including a non-nil
// details map when present. Falls back to writeError when details is nil to
// keep the common-path response shape unchanged.
func writeErrorWithDetails(c *gin.Context, status int, code string, details map[string]any) {
	if len(details) == 0 {
		writeError(c, status, code)
		return
	}
	c.AbortWithStatusJSON(status, errorResponseWithDetails{
		Error:     code,
		Status:    status,
		TraceID:   gincommon.TraceIDFromContext(c),
		RequestID: gincommon.RequestIDFromContext(c),
		Details:   details,
	})
}

// HandleError maps err onto the HTTP status from LLD §20 and writes it via
// writeErrorWithDetails. Any error that isn't a *domain.Error — or is one
// with a code this adapter doesn't recognize — becomes a generic 500, never
// leaking implementation detail. Unclassified 500s are logged through
// errorLogger (the gincommon Zap sink NewRouter installs).
func HandleError(c *gin.Context, err error) {
	var derr *domain.Error
	if errors.As(err, &derr) {
		status, ok := errorStatusByCode[derr.Code]
		if !ok {
			status = http.StatusInternalServerError
		}
		writeErrorWithDetails(c, status, derr.Code, derr.Details)
		return
	}
	// Raw pgconn.PgError that wrapConnErr did not catch. Classes 08/53 via
	// pgcommon v1.3.0 helpers; 57/58 via SQLSTATE text — matching
	// iam-realm-provisioner. Inbound HTTP must not import pgconn.
	if pgcommon.IsConnectionException(err) || pgcommon.IsInsufficientResources(err) || isOperatorOrSystemErrorSQLState(err) {
		writeError(c, http.StatusServiceUnavailable, domain.ErrDBUnavailable.Error())
		return
	}
	if errorLogger != nil {
		fields := map[string]interface{}{
			"error_type": fmt.Sprintf("%T", err),
			"error":      err.Error(),
			"trace_id":   gincommon.TraceIDFromContext(c),
			"request_id": gincommon.RequestIDFromContext(c),
		}
		if c.Request != nil {
			fields["path"] = c.FullPath()
			if fields["path"] == "" {
				fields["path"] = c.Request.URL.Path
			}
			fields["method"] = c.Request.Method
		}
		errorLogger.Error("unhandled 500 error", fields)
	}
	writeError(c, http.StatusInternalServerError, "internal_server_error")
}

// isOperatorOrSystemErrorSQLState reports whether err is a Postgres error
// in SQLSTATE class 57 or 58. pgcommon v1.3.0 has no dedicated helper for
// these two, so we match the "(SQLSTATE 57…)" / "(SQLSTATE 58…)" text
// pgconn puts in Error().
func isOperatorOrSystemErrorSQLState(err error) bool {
	if !pgcommon.IsPgError(err) {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLSTATE 57") || strings.Contains(msg, "SQLSTATE 58")
}
