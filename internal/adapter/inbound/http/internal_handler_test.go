package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
)

// ── Expire (DLG-I1) ─────────────────────────────────────────────────────

func TestInternalHandler_Expire(t *testing.T) {
	t.Run("injected runner invoked, result translated", func(t *testing.T) {
		called := false
		runner := &fakeExpiryRunner{fn: func(ctx context.Context) (int, int, int, error) {
			called = true
			return 10, 8, 2, nil
		}}
		h := NewInternalHandler(runner, &fakeReviewRunner{}, &fakeDelegationReader{})
		c, w := newRequestWithIdentity(http.MethodPost, "/internal/delegations/expire", nil, nil)
		h.Expire(c)
		require.True(t, called)
		require.Equal(t, http.StatusOK, w.Code)
		var resp ExpiryRunResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		require.Equal(t, ExpiryRunResponse{Attempted: 10, Succeeded: 8, Failed: 2}, resp)
	})

	t.Run("runner error mapped", func(t *testing.T) {
		runner := &fakeExpiryRunner{fn: func(ctx context.Context) (int, int, int, error) {
			return 0, 0, 0, domain.NewError(domain.ErrOrgMembershipUnavailable, "down")
		}}
		h := NewInternalHandler(runner, &fakeReviewRunner{}, &fakeDelegationReader{})
		c, w := newRequestWithIdentity(http.MethodPost, "/internal/delegations/expire", nil, nil)
		h.Expire(c)
		require.Equal(t, http.StatusServiceUnavailable, w.Code)
	})

	t.Run("plain (non-domain) error -> 500", func(t *testing.T) {
		runner := &fakeExpiryRunner{fn: func(ctx context.Context) (int, int, int, error) {
			return 0, 0, 0, errors.New("boom")
		}}
		h := NewInternalHandler(runner, &fakeReviewRunner{}, &fakeDelegationReader{})
		c, w := newRequestWithIdentity(http.MethodPost, "/internal/delegations/expire", nil, nil)
		h.Expire(c)
		require.Equal(t, http.StatusInternalServerError, w.Code)
	})
}

// ── ReviewSweep (DLG-I2) ────────────────────────────────────────────────

func TestInternalHandler_ReviewSweep(t *testing.T) {
	t.Run("injected runner invoked, result translated", func(t *testing.T) {
		called := false
		runner := &fakeReviewRunner{fn: func(ctx context.Context) (ReviewSweepResult, error) {
			called = true
			return ReviewSweepResult{Warned3d: 3, Warned2d: 2, Warned1d: 1, Expired: 1}, nil
		}}
		h := NewInternalHandler(&fakeExpiryRunner{}, runner, &fakeDelegationReader{})
		c, w := newRequestWithIdentity(http.MethodPost, "/internal/delegations/review-sweep", nil, nil)
		h.ReviewSweep(c)
		require.True(t, called)
		require.Equal(t, http.StatusOK, w.Code)
		var resp ReviewSweepRunResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		require.Equal(t, ReviewSweepRunResponse{Warned3d: 3, Warned2d: 2, Warned1d: 1, Expired: 1, Deferred: 0, Failed: 0}, resp)
	})

	t.Run("runner error mapped", func(t *testing.T) {
		runner := &fakeReviewRunner{fn: func(ctx context.Context) (ReviewSweepResult, error) {
			return ReviewSweepResult{}, domain.NewError(domain.ErrUserProfileUnavailable, "down")
		}}
		h := NewInternalHandler(&fakeExpiryRunner{}, runner, &fakeDelegationReader{})
		c, w := newRequestWithIdentity(http.MethodPost, "/internal/delegations/review-sweep", nil, nil)
		h.ReviewSweep(c)
		require.Equal(t, http.StatusServiceUnavailable, w.Code)
	})
}

// ── DeptDelegate (DLG-I3) ───────────────────────────────────────────────

func TestInternalHandler_DeptDelegate(t *testing.T) {
	tenantID, userID, deptID := uuid.New(), uuid.New(), uuid.New()

	t.Run("invalid tenant_id -> 400", func(t *testing.T) {
		h := NewInternalHandler(&fakeExpiryRunner{}, &fakeReviewRunner{}, &fakeDelegationReader{})
		c, w := newRequestWithIdentity(http.MethodGet, "/internal/delegations/dept-delegate?tenant_id=bad&user_id="+userID.String()+"&dept_id="+deptID.String(), nil, nil)
		h.DeptDelegate(c)
		require.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("invalid user_id -> 400", func(t *testing.T) {
		h := NewInternalHandler(&fakeExpiryRunner{}, &fakeReviewRunner{}, &fakeDelegationReader{})
		c, w := newRequestWithIdentity(http.MethodGet, "/internal/delegations/dept-delegate?tenant_id="+tenantID.String()+"&user_id=bad&dept_id="+deptID.String(), nil, nil)
		h.DeptDelegate(c)
		require.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("invalid dept_id -> 400", func(t *testing.T) {
		h := NewInternalHandler(&fakeExpiryRunner{}, &fakeReviewRunner{}, &fakeDelegationReader{})
		c, w := newRequestWithIdentity(http.MethodGet, "/internal/delegations/dept-delegate?tenant_id="+tenantID.String()+"&user_id="+userID.String()+"&dept_id=bad", nil, nil)
		h.DeptDelegate(c)
		require.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("not found -> 200 found:false", func(t *testing.T) {
		reader := &fakeDelegationReader{findActiveDeptDelegateFn: func(ctx context.Context, tenantID, userID, deptID uuid.UUID) (*domain.Delegation, error) {
			return nil, nil
		}}
		h := NewInternalHandler(&fakeExpiryRunner{}, &fakeReviewRunner{}, reader)
		c, w := newRequestWithIdentity(http.MethodGet, "/internal/delegations/dept-delegate?tenant_id="+tenantID.String()+"&user_id="+userID.String()+"&dept_id="+deptID.String(), nil, nil)
		h.DeptDelegate(c)
		require.Equal(t, http.StatusOK, w.Code)
		var resp DeptDelegateResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		require.False(t, resp.Found)
		require.Nil(t, resp.DelegationID)
	})

	t.Run("found -> 200 with delegation/delegator/delegate ids", func(t *testing.T) {
		delegationID, delegateID := uuid.New(), uuid.New()
		reader := &fakeDelegationReader{findActiveDeptDelegateFn: func(ctx context.Context, gotTenant, gotUser, gotDept uuid.UUID) (*domain.Delegation, error) {
			require.Equal(t, tenantID, gotTenant)
			require.Equal(t, userID, gotUser)
			require.Equal(t, deptID, gotDept)
			return &domain.Delegation{ID: delegationID, DelegatorID: userID, DelegateID: delegateID}, nil
		}}
		h := NewInternalHandler(&fakeExpiryRunner{}, &fakeReviewRunner{}, reader)
		c, w := newRequestWithIdentity(http.MethodGet, "/internal/delegations/dept-delegate?tenant_id="+tenantID.String()+"&user_id="+userID.String()+"&dept_id="+deptID.String(), nil, nil)
		h.DeptDelegate(c)
		require.Equal(t, http.StatusOK, w.Code)
		var resp DeptDelegateResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		require.True(t, resp.Found)
		require.Equal(t, delegationID, *resp.DelegationID)
		require.Equal(t, delegateID, *resp.DelegateID)
	})

	t.Run("reader error mapped", func(t *testing.T) {
		reader := &fakeDelegationReader{findActiveDeptDelegateFn: func(ctx context.Context, tenantID, userID, deptID uuid.UUID) (*domain.Delegation, error) {
			return nil, domain.NewError(domain.ErrValidation, "boom")
		}}
		h := NewInternalHandler(&fakeExpiryRunner{}, &fakeReviewRunner{}, reader)
		c, w := newRequestWithIdentity(http.MethodGet, "/internal/delegations/dept-delegate?tenant_id="+tenantID.String()+"&user_id="+userID.String()+"&dept_id="+deptID.String(), nil, nil)
		h.DeptDelegate(c)
		require.Equal(t, http.StatusBadRequest, w.Code)
	})
}

// ── ActiveDelegations (DLG-I4) ──────────────────────────────────────────

func TestInternalHandler_ActiveDelegations(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()

	t.Run("invalid path id -> 400", func(t *testing.T) {
		h := NewInternalHandler(&fakeExpiryRunner{}, &fakeReviewRunner{}, &fakeDelegationReader{})
		c, w := newRequestWithIdentity(http.MethodGet, "/internal/users/bad/active-delegations?tenant_id="+tenantID.String(), nil, nil)
		setPathParam(c, "id", "bad")
		h.ActiveDelegations(c)
		require.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("invalid tenant_id query -> 400", func(t *testing.T) {
		h := NewInternalHandler(&fakeExpiryRunner{}, &fakeReviewRunner{}, &fakeDelegationReader{})
		c, w := newRequestWithIdentity(http.MethodGet, "/internal/users/"+userID.String()+"/active-delegations?tenant_id=bad", nil, nil)
		setPathParam(c, "id", userID.String())
		h.ActiveDelegations(c)
		require.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("happy path -> 200 list", func(t *testing.T) {
		delegateID, scopeID := uuid.New(), uuid.New()
		d := domain.Delegation{ID: uuid.New(), DelegatorID: userID, DelegateID: delegateID, Scope: domain.ScopeDepartment, ScopeID: &scopeID}
		reader := &fakeDelegationReader{findActiveByDelegatorFn: func(ctx context.Context, gotTenant, gotDelegator uuid.UUID) ([]domain.Delegation, error) {
			require.Equal(t, tenantID, gotTenant)
			require.Equal(t, userID, gotDelegator)
			return []domain.Delegation{d}, nil
		}}
		h := NewInternalHandler(&fakeExpiryRunner{}, &fakeReviewRunner{}, reader)
		c, w := newRequestWithIdentity(http.MethodGet, "/internal/users/"+userID.String()+"/active-delegations?tenant_id="+tenantID.String(), nil, nil)
		setPathParam(c, "id", userID.String())
		h.ActiveDelegations(c)
		require.Equal(t, http.StatusOK, w.Code)
		var resp ActiveDelegationsResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		require.Len(t, resp.ActiveDelegations, 1)
		require.Equal(t, d.ID, resp.ActiveDelegations[0].DelegationID)
		require.Equal(t, "department", resp.ActiveDelegations[0].Scope)
	})

	t.Run("reader error mapped", func(t *testing.T) {
		reader := &fakeDelegationReader{findActiveByDelegatorFn: func(ctx context.Context, tenantID, delegatorID uuid.UUID) ([]domain.Delegation, error) {
			return nil, domain.NewError(domain.ErrValidation, "boom")
		}}
		h := NewInternalHandler(&fakeExpiryRunner{}, &fakeReviewRunner{}, reader)
		c, w := newRequestWithIdentity(http.MethodGet, "/internal/users/"+userID.String()+"/active-delegations?tenant_id="+tenantID.String(), nil, nil)
		setPathParam(c, "id", userID.String())
		h.ActiveDelegations(c)
		require.Equal(t, http.StatusBadRequest, w.Code)
	})
}
