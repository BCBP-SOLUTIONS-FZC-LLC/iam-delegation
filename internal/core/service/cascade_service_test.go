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

// TestCascadeService_EndForDisabledDelegate_EmitsEndedForEveryRow is Bug 2's
// service-level test: every row EndForDisabledDelegate's repository call
// returns is, by construction, delegate-side (delegateID is the disabled
// user), so — unlike EndForUser's asymmetric silence — every row emits
// DelegationEnded{delegate_disabled}. No User Profile call is made: the
// delegator's pointer was already cleared by User Profile itself before
// this event was even published.
func TestCascadeService_EndForDisabledDelegate_EmitsEndedForEveryRow(t *testing.T) {
	repo, _, up, pub, svc := newCascadeHarness()
	tenantID := uuid.New()
	disabledDelegate := uuid.New()
	delegatorA := uuid.New()
	delegatorB := uuid.New()

	rowA := domain.Delegation{
		ID: uuid.New(), TenantID: tenantID,
		DelegatorID: delegatorA, DelegateID: disabledDelegate,
		Scope: domain.ScopeAll, Status: domain.DelegationEnded,
	}
	rowB := domain.Delegation{
		ID: uuid.New(), TenantID: tenantID,
		DelegatorID: delegatorB, DelegateID: disabledDelegate,
		Scope: domain.ScopeDepartment, Status: domain.DelegationEnded,
	}
	repo.endForDisabledDelegateResult = []domain.Delegation{rowA, rowB}

	err := svc.EndForDisabledDelegate(context.Background(), tenantID, disabledDelegate)
	require.NoError(t, err)

	events := pub.snapshot()
	require.Len(t, events, 2, "every row is delegate-side and must emit an event")
	for _, e := range events {
		assert.Equal(t, domain.EventDelegationEnded, e.Type)
		payload, ok := e.Data.(domain.DelegationEndedPayload)
		require.True(t, ok)
		assert.Equal(t, disabledDelegate, payload.DelegateID)
		assert.Equal(t, domain.EndReasonDelegateDisabled, payload.EndedReason,
			"must be delegate_disabled, distinct from EndForUser's delegate_removed")
	}

	assert.Empty(t, up.calls, "User Profile's delegate pointer was already cleared atomically by its own disable flow — no redundant call")
}

func TestCascadeService_EndForDisabledDelegate_NoMatchingRows_NoEvents(t *testing.T) {
	repo, _, up, pub, svc := newCascadeHarness()
	repo.endForDisabledDelegateResult = nil

	err := svc.EndForDisabledDelegate(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.Empty(t, pub.snapshot())
	assert.Empty(t, up.calls)
}

func TestCascadeService_EndForDisabledDelegate_RepoError_Propagates(t *testing.T) {
	repo, _, _, pub, svc := newCascadeHarness()
	wantErr := errors.New("db down")
	repo.endForDisabledDelegateErr = wantErr

	err := svc.EndForDisabledDelegate(context.Background(), uuid.New(), uuid.New())
	require.ErrorIs(t, err, wantErr)
	assert.Empty(t, pub.snapshot())
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
