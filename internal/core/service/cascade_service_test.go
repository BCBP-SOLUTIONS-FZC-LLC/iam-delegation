package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
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

func TestCascadeService_WithMetrics_ReturnsSameInstance(t *testing.T) {
	_, _, _, _, svc := newCascadeHarness()
	fm := &fakeMetrics{}
	got := svc.WithMetrics(fm)
	assert.Same(t, svc, got)
}

func TestCascadeService_EndForUser_RecordsMetrics(t *testing.T) {
	repo, _, up, _, svc := newCascadeHarness()
	fm := &fakeMetrics{}
	svc.WithMetrics(fm)
	tenantID := uuid.New()
	removedUserID := uuid.New()
	delegateSideRow := domain.Delegation{
		ID: uuid.New(), TenantID: tenantID,
		DelegatorID: uuid.New(), DelegateID: removedUserID,
		Scope: domain.ScopeAll, Status: domain.DelegationEnded,
	}
	repo.endForUserResult = []domain.Delegation{delegateSideRow}
	up.fn = func(ctx context.Context, r port.SetAvailabilityRequest) error {
		return errors.New("user-profile down")
	}

	err := svc.EndForUser(context.Background(), tenantID, removedUserID)
	require.NoError(t, err)

	assert.Equal(t, []string{"cascade"}, fm.upAvailabilityFailures)
	assert.Equal(t, []string{string(domain.EndReasonDelegateRemoved)}, fm.ended)
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
	require.Len(t, events, 4, "every row is delegate-side and must emit BOTH DelegationEnded and DelegationEscalationRequested (Bug 2a)")
	endedCount, escalationCount := 0, 0
	for _, e := range events {
		switch e.Type {
		case domain.EventDelegationEnded:
			endedCount++
			payload, ok := e.Data.(domain.DelegationEndedPayload)
			require.True(t, ok)
			assert.Equal(t, disabledDelegate, payload.DelegateID)
			assert.Equal(t, domain.EndReasonDelegateDisabled, payload.EndedReason,
				"must be delegate_disabled, distinct from EndForUser's delegate_removed")
		case domain.EventDelegationEscalationRequested:
			escalationCount++
			payload, ok := e.Data.(domain.DelegationEscalationRequestedPayload)
			require.True(t, ok)
			assert.Equal(t, disabledDelegate, payload.DelegateID)
			assert.Equal(t, domain.EndReasonDelegateDisabled, payload.Reason)
			assert.Equal(t, domain.SystemActorID, payload.ActorID)
		default:
			t.Fatalf("unexpected event type %q", e.Type)
		}
	}
	assert.Equal(t, 2, endedCount)
	assert.Equal(t, 2, escalationCount)

	assert.Empty(t, up.calls, "User Profile's delegate pointer was already cleared atomically by its own disable flow — no redundant call")
}

// TestCascadeService_EndForDisabledDelegate_EscalationMatchesEndedRow is Bug
// 2a's field-correspondence test: DelegationEscalationRequested must carry
// the SAME delegation_id/delegator_id/scope as the DelegationEnded it rides
// alongside, so a consumer never needs to guess which grant is being
// escalated.
func TestCascadeService_EndForDisabledDelegate_EscalationMatchesEndedRow(t *testing.T) {
	repo, _, _, pub, svc := newCascadeHarness()
	tenantID := uuid.New()
	disabledDelegate := uuid.New()
	delegator := uuid.New()
	scopeID := uuid.New()

	row := domain.Delegation{
		ID: uuid.New(), TenantID: tenantID,
		DelegatorID: delegator, DelegateID: disabledDelegate,
		Scope: domain.ScopeDepartment, ScopeID: &scopeID,
		Status: domain.DelegationEnded,
	}
	repo.endForDisabledDelegateResult = []domain.Delegation{row}

	err := svc.EndForDisabledDelegate(context.Background(), tenantID, disabledDelegate)
	require.NoError(t, err)

	events := pub.snapshot()
	require.Len(t, events, 2)

	var endedPayload domain.DelegationEndedPayload
	var escalationPayload domain.DelegationEscalationRequestedPayload
	for _, e := range events {
		switch p := e.Data.(type) {
		case domain.DelegationEndedPayload:
			endedPayload = p
		case domain.DelegationEscalationRequestedPayload:
			escalationPayload = p
		}
	}

	assert.Equal(t, endedPayload.DelegationID, escalationPayload.DelegationID)
	assert.Equal(t, endedPayload.TenantID, escalationPayload.TenantID)
	assert.Equal(t, endedPayload.DelegatorID, escalationPayload.DelegatorID)
	assert.Equal(t, endedPayload.DelegateID, escalationPayload.DelegateID)
	assert.Equal(t, endedPayload.Scope, escalationPayload.Scope)
	assert.Equal(t, endedPayload.ScopeID, escalationPayload.ScopeID)
}

func TestCascadeService_EndForDisabledDelegate_RecordsEndedMetricPerRow(t *testing.T) {
	repo, _, _, _, svc := newCascadeHarness()
	fm := &fakeMetrics{}
	svc.WithMetrics(fm)
	tenantID := uuid.New()
	disabledDelegate := uuid.New()
	repo.endForDisabledDelegateResult = []domain.Delegation{
		{ID: uuid.New(), TenantID: tenantID, DelegatorID: uuid.New(), DelegateID: disabledDelegate, Scope: domain.ScopeAll},
		{ID: uuid.New(), TenantID: tenantID, DelegatorID: uuid.New(), DelegateID: disabledDelegate, Scope: domain.ScopeAll},
	}

	err := svc.EndForDisabledDelegate(context.Background(), tenantID, disabledDelegate)
	require.NoError(t, err)
	assert.Equal(t, []string{string(domain.EndReasonDelegateDisabled), string(domain.EndReasonDelegateDisabled)}, fm.ended)
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

// countingPublisher succeeds for the first (failAt-1) EnqueueCtx calls then
// returns err on the failAt-th call and beyond.
type countingPublisher struct {
	mu     sync.Mutex
	count  int
	failAt int
	err    error
}

func (p *countingPublisher) EnqueueCtx(_ context.Context, _ *domain.DomainEvent) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.count++
	if p.count >= p.failAt {
		return p.err
	}
	return nil
}

func TestCascadeService_EndForUser_RepoError_PropagatesFromTx(t *testing.T) {
	repo, _, _, _, svc := newCascadeHarness()
	wantErr := errors.New("db down")
	repo.endForUserErr = wantErr

	err := svc.EndForUser(context.Background(), uuid.New(), uuid.New())
	require.ErrorIs(t, err, wantErr)
}

func TestCascadeService_EndForUser_EnqueueError_PropagatesFromTx(t *testing.T) {
	repo, _, _, pub, svc := newCascadeHarness()
	tenantID := uuid.New()
	delegateID := uuid.New()
	repo.endForUserResult = []domain.Delegation{
		{ID: uuid.New(), TenantID: tenantID, DelegatorID: uuid.New(), DelegateID: delegateID, Scope: domain.ScopeAll},
	}
	pub.err = errors.New("enqueue fail")

	err := svc.EndForUser(context.Background(), tenantID, delegateID)
	require.ErrorIs(t, err, pub.err)
}

func TestCascadeService_EndForDisabledDelegate_EnqueueEndedError_Propagates(t *testing.T) {
	repo, _, _, pub, svc := newCascadeHarness()
	tenantID := uuid.New()
	delegateID := uuid.New()
	repo.endForDisabledDelegateResult = []domain.Delegation{
		{ID: uuid.New(), TenantID: tenantID, DelegatorID: uuid.New(), DelegateID: delegateID, Scope: domain.ScopeAll},
	}
	pub.err = errors.New("enqueue failed")

	err := svc.EndForDisabledDelegate(context.Background(), tenantID, delegateID)
	require.ErrorIs(t, err, pub.err)
}

func TestCascadeService_EndForDisabledDelegate_EnqueueEscalationError_Propagates(t *testing.T) {
	rec := &callRecorder{}
	repo := newFakeDelegationRepository(rec)
	settings := &fakeSettingsRepository{rec: rec}
	up := &fakeUserProfileClient{rec: rec}
	wantErr := errors.New("escalation enqueue fail")
	cpub := &countingPublisher{failAt: 2, err: wantErr}
	tx := &fakeTxRunner{rec: rec, publisher: cpub}
	svc := NewCascadeService(repo, settings, up, tx)

	tenantID := uuid.New()
	delegateID := uuid.New()
	repo.endForDisabledDelegateResult = []domain.Delegation{
		{ID: uuid.New(), TenantID: tenantID, DelegatorID: uuid.New(), DelegateID: delegateID, Scope: domain.ScopeAll},
	}

	err := svc.EndForDisabledDelegate(context.Background(), tenantID, delegateID)
	require.ErrorIs(t, err, wantErr)
}
