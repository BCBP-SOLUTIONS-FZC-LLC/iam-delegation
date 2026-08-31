package service

import (
	"context"
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newCascadeHarness() (*fakeDelegationRepository, *fakeSettingsRepository, *fakeUserProfileClient, *fakeEventPublisher, *CascadeService) {
	rec := &callRecorder{}
	repo := newFakeDelegationRepository(rec)
	settings := &fakeSettingsRepository{rec: rec}
	up := &fakeUserProfileClient{rec: rec}
	pub := &fakeEventPublisher{}
	tx := &fakeTxRunner{rec: rec, publisher: pub}
	svc := NewCascadeService(repo, settings, up, tx)
	return repo, settings, up, pub, svc
}

// TestCascadeService_EndForUser_DelegatorDelegateAsymmetry is the DEL-7 /
// DLG-EVT-4 asymmetry test: a delegate-side row (removed user is the
// delegate) emits DelegationEnded{delegate_removed}; a delegator-side row
// (removed user is the delegator) ends silently with no event, and the
// removed user's own pointer is never targeted for a clear.
func TestCascadeService_EndForUser_DelegatorDelegateAsymmetry(t *testing.T) {
	repo, _, up, pub, svc := newCascadeHarness()
	tenantID := uuid.New()
	removedUserID := uuid.New()
	otherUser1 := uuid.New() // delegate on the delegator-side row
	otherUser2 := uuid.New() // delegator on the delegate-side row

	delegatorSideRow := domain.Delegation{
		ID: uuid.New(), TenantID: tenantID,
		DelegatorID: removedUserID, DelegateID: otherUser1,
		Scope: domain.ScopeAll, Status: domain.DelegationEnded,
	}
	delegateSideRow := domain.Delegation{
		ID: uuid.New(), TenantID: tenantID,
		DelegatorID: otherUser2, DelegateID: removedUserID,
		Scope: domain.ScopeAll, Status: domain.DelegationEnded,
	}
	repo.endForUserResult = []domain.Delegation{delegatorSideRow, delegateSideRow}

	err := svc.EndForUser(context.Background(), tenantID, removedUserID)
	require.NoError(t, err)

	events := pub.snapshot()
	require.Len(t, events, 1, "only the delegate-side row emits an event")
	assert.Equal(t, domain.EventDelegationEnded, events[0].Type)
	payload, ok := events[0].Data.(domain.DelegationEndedPayload)
	require.True(t, ok)
	assert.Equal(t, removedUserID, payload.DelegateID)
	assert.Equal(t, domain.EndReasonDelegateRemoved, payload.EndedReason)
	assert.Equal(t, delegateSideRow.ID, payload.DelegationID)

	require.Len(t, up.calls, 1, "exactly one pointer-clear call, for the delegate-side row's delegator")
	assert.Equal(t, otherUser2, up.calls[0].UserID)
	assert.True(t, up.calls[0].ClearDelegate)

	for _, c := range up.calls {
		assert.NotEqual(t, removedUserID, c.UserID, "the removed user's own record must never be targeted for a clear")
	}
}

// TestCascadeService_EndForUser_RepoError covers the repo.EndForUser error path.
func TestCascadeService_EndForUser_RepoError(t *testing.T) {
	repo, _, _, _, svc := newCascadeHarness()
	wantErr := errors.New("db error")
	repo.endForUserErr = wantErr

	err := svc.EndForUser(context.Background(), uuid.New(), uuid.New())
	require.ErrorIs(t, err, wantErr)
}

// TestCascadeService_EndForUser_DelegatorSideSkipsUPClear covers the
// d.DelegatorID == userID continue branch: the removed user is the delegator,
// so their own pointer must never be cleared.
func TestCascadeService_EndForUser_DelegatorSideSkipsUPClear(t *testing.T) {
	repo, _, up, _, svc := newCascadeHarness()
	tenantID := uuid.New()
	removedUser := uuid.New()

	// Only row: removed user is the delegator (not delegate)
	repo.endForUserResult = []domain.Delegation{{
		ID: uuid.New(), TenantID: tenantID,
		DelegatorID: removedUser, // removed user is delegator
		DelegateID:  uuid.New(),
		Scope:       domain.ScopeAll, Status: domain.DelegationEnded,
	}}

	err := svc.EndForUser(context.Background(), tenantID, removedUser)
	require.NoError(t, err)
	require.Empty(t, up.calls, "delegator-side removal must not trigger UP pointer-clear")
}

func TestCascadeService_ScrubTenant_OrderAndShortCircuit(t *testing.T) {
	t.Run("delegation repository error short-circuits before settings", func(t *testing.T) {
		repo, settings, _, _, svc := newCascadeHarness()
		wantErr := errors.New("db down")
		repo.softDeleteTenantErr = wantErr

		err := svc.ScrubTenant(context.Background(), uuid.New())
		require.ErrorIs(t, err, wantErr)
		assert.Equal(t, 1, repo.softDeleteTenantCalls)
		assert.Equal(t, 0, settings.softDeleteTenantCalls, "settings must never be scrubbed when the delegation scrub fails")
	})

	t.Run("both scrubbed on success", func(t *testing.T) {
		repo, settings, _, _, svc := newCascadeHarness()

		err := svc.ScrubTenant(context.Background(), uuid.New())
		require.NoError(t, err)
		assert.Equal(t, 1, repo.softDeleteTenantCalls)
		assert.Equal(t, 1, settings.softDeleteTenantCalls)
	})
}
