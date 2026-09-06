package http

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func newTestGinContext() (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	return c, w
}

// TestHandleError_EverySentinel asserts every domain.Err* sentinel maps to
// the exact HTTP status in LLD §20's error taxonomy table.
func TestHandleError_EverySentinel(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{"validation", domain.ErrValidation, http.StatusBadRequest},
		{"missing_identity", domain.ErrMissingIdentity, http.StatusUnauthorized},
		{"insufficient_role", domain.ErrInsufficientRole, http.StatusForbidden},
		{"optimistic_lock_conflict", domain.ErrOptimisticLockConflict, http.StatusConflict},

		{"invalid_delegation_scope", domain.ErrInvalidDelegationScope, http.StatusBadRequest},
		{"invalid_delegation_max_duration_days", domain.ErrInvalidDelegationMaxDurationDays, http.StatusBadRequest},
		{"invalid_delegation_review_window_days", domain.ErrInvalidDelegationReviewWindow, http.StatusBadRequest},

		{"delegation_not_found", domain.ErrDelegationNotFound, http.StatusNotFound},

		{"scope_id_required", domain.ErrScopeIDRequired, http.StatusUnprocessableEntity},
		{"invalid_scope_id", domain.ErrInvalidScopeID, http.StatusUnprocessableEntity},
		{"self_delegation", domain.ErrSelfDelegation, http.StatusUnprocessableEntity},
		{"invalid_delegate", domain.ErrInvalidDelegate, http.StatusUnprocessableEntity},
		{"delegate_unavailable", domain.ErrDelegateUnavailable, http.StatusUnprocessableEntity},
		{"reason_too_long", domain.ErrReasonTooLong, http.StatusUnprocessableEntity},
		{"delegation_window_inverted", domain.ErrDelegationWindowInverted, http.StatusUnprocessableEntity},
		{"delegation_window_too_long", domain.ErrDelegationWindowTooLong, http.StatusUnprocessableEntity},
		{"delegation_start_in_past", domain.ErrDelegationStartInPast, http.StatusUnprocessableEntity},
		{"delegation_start_too_far_future", domain.ErrDelegationStartTooFarFuture, http.StatusUnprocessableEntity},
		{"not_review_tracked", domain.ErrNotReviewTracked, http.StatusUnprocessableEntity},
		{"extend_days_out_of_range", domain.ErrExtendDaysOutOfRange, http.StatusUnprocessableEntity},

		{"org_membership_unavailable", domain.ErrOrgMembershipUnavailable, http.StatusServiceUnavailable},
		{"user_profile_unavailable", domain.ErrUserProfileUnavailable, http.StatusServiceUnavailable},
		{"db_unavailable", domain.ErrDBUnavailable, http.StatusServiceUnavailable},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, w := newTestGinContext()
			HandleError(c, domain.NewError(tc.err, "boom"))
			if w.Code != tc.status {
				t.Fatalf("code=%s: got status %d, want %d", tc.name, w.Code, tc.status)
			}
		})
	}
}

// TestHandleError_UnrecognizedError asserts a plain (non-*domain.Error)
// error becomes a generic 500 rather than leaking implementation detail.
func TestHandleError_UnrecognizedError(t *testing.T) {
	c, w := newTestGinContext()
	HandleError(c, errors.New("some internal plumbing failure"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

// TestHandleError_WrappedSentinel asserts errors.As still finds the
// *domain.Error even when it has been wrapped by an intermediate caller
// (fmt.Errorf("...: %w", derr)).
func TestHandleError_WrappedSentinel(t *testing.T) {
	c, w := newTestGinContext()
	wrapped := errors.Join(domain.NewError(domain.ErrDelegationNotFound, "gone"))
	HandleError(c, wrapped)
	if w.Code != http.StatusNotFound {
		t.Fatalf("got status %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleError_DBConnectivitySQLState_Returns503(t *testing.T) {
	c, w := newTestGinContext()
	HandleError(c, &pgconn.PgError{Code: "08006"})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got status %d, want 503", w.Code)
	}
}

func TestHandleError_ConstraintViolationSQLState_Returns500(t *testing.T) {
	c, w := newTestGinContext()
	HandleError(c, &pgconn.PgError{Code: "23505"})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("got status %d, want 500", w.Code)
	}
}

func TestWriteErrorWithDetails_NilDetails_FallsBackToWriteError(t *testing.T) {
	c, w := newTestGinContext()
	writeErrorWithDetails(c, http.StatusBadRequest, "validation_error", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestWriteErrorWithDetails_WithDetails(t *testing.T) {
	c, w := newTestGinContext()
	writeErrorWithDetails(c, http.StatusConflict, "optimistic_lock_conflict", map[string]any{"record_version": int64(3)})
	if w.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d", http.StatusConflict, w.Code)
	}
}

// TestHandleError_UnmappedDomainErrorCode covers lines 104–106: a *domain.Error
// whose code has no entry in errorStatusByCode falls back to 500.
func TestHandleError_UnmappedDomainErrorCode_Returns500(t *testing.T) {
	c, w := newTestGinContext()
	HandleError(c, &domain.Error{Code: "no_such_code_xyz_for_testing"})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for unmapped code, got %d", w.Code)
	}
}
