package http

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

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

// TestHandleError_DetailsPassedThrough confirms BUG-01 is NOT a bug:
// HandleError correctly passes WithDetails data through to the response.
func TestHandleError_DetailsPassedThrough(t *testing.T) {
	c, w := newTestGinContext()
	err := domain.NewError(domain.ErrDelegationWindowTooLong, "too long").
		WithDetails(map[string]any{"max_duration_days": 90})
	HandleError(c, err)
	require.Equal(t, http.StatusUnprocessableEntity, w.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	details, ok := body["details"].(map[string]any)
	require.True(t, ok, "response must include 'details' field when set on the error")
	require.Equal(t, float64(90), details["max_duration_days"])
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

// TestHandleError_UnknownDomainCode_Returns500 covers the fallback branch in
// HandleError when a *domain.Error carries a code not present in errorStatusByCode
// (e.g. a future sentinel added before the map is updated).
func TestHandleError_UnknownDomainCode_Returns500(t *testing.T) {
	c, w := newTestGinContext()
	HandleError(c, &domain.Error{Code: "some_future_code_not_in_map", Message: "future"})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("got status %d, want 500 for unrecognized code", w.Code)
	}
}
