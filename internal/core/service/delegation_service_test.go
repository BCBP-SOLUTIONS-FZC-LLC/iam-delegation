package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/pkg/requestctx"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── test helpers ─────────────────────────────────────────────────────────

func ptrTime(t time.Time) *time.Time { return &t }
func ptrInt(i int) *int              { return &i }
func ptrUUID(u uuid.UUID) *uuid.UUID { return &u }

// validCreateInput returns a CreateInput that passes every validation rule
// in validateCreateInput, for a delegator that differs from the delegate.
func validCreateInput() CreateInput {
	return CreateInput{
		DelegateID: uuid.New(),
		Scope:      string(domain.ScopeAll),
		Reason:     "annual leave",
	}
}

// newDelegationServiceHarness wires a DelegationService with fresh
// hand-written fakes, all sharing one callRecorder so cross-fake ordering
// can be asserted.
type delegationHarness struct {
	rec        *callRecorder
	repo       *fakeDelegationRepository
	settings   *fakeSettingsRepository
	membership *fakeMembershipCheckClient
	up         *fakeUserProfileClient
	idem       *fakeIdempotencyStore
	cache      *fakeCache
	pub        *fakeEventPublisher
	tx         *fakeTxRunner
	svc        *DelegationService
}

func newDelegationHarness() *delegationHarness {
	rec := &callRecorder{}
	repo := newFakeDelegationRepository(rec)
	settings := &fakeSettingsRepository{rec: rec}
	membership := newFakeMembershipCheckClient(rec)
	up := &fakeUserProfileClient{rec: rec}
	idem := &fakeIdempotencyStore{}
	cache := &fakeCache{}
	pub := &fakeEventPublisher{}
	tx := &fakeTxRunner{rec: rec, publisher: pub}
	svc := NewDelegationService(repo, settings, membership, up, idem, cache, tx)
	return &delegationHarness{rec: rec, repo: repo, settings: settings, membership: membership, up: up, idem: idem, cache: cache, pub: pub, tx: tx, svc: svc}
}

// activeBoth configures both delegatorID and delegateID as active members
// with fresh membership IDs, returning those IDs.
func (h *delegationHarness) activeBoth(delegatorID, delegateID uuid.UUID) (delegatorMembershipID, delegateMembershipID uuid.UUID) {
	delegatorMembershipID = uuid.New()
	delegateMembershipID = uuid.New()
	h.membership.results[delegatorID] = membershipResult{active: true, id: delegatorMembershipID}
	h.membership.results[delegateID] = membershipResult{active: true, id: delegateMembershipID}
	return
}

func requireDomainCode(t *testing.T, err error, wantCode string) {
	t.Helper()
	require.Error(t, err)
	var de *domain.Error
	require.True(t, errors.As(err, &de), "expected *domain.Error, got %T: %v", err, err)
	assert.Equal(t, wantCode, de.Code)
}

// ── Create: validation rejections ───────────────────────────────────────

func TestDelegationService_Create_ValidationRejections(t *testing.T) {
	delegatorID := uuid.New()

	tests := []struct {
		name     string
		mutate   func(req *CreateInput)
		settings *domain.DelegationTenantSettings
		wantCode string
	}{
		{
			name: "self_delegation",
			mutate: func(req *CreateInput) {
				req.DelegateID = delegatorID
			},
			wantCode: domain.ErrSelfDelegation.Error(),
		},
		{
			name: "invalid scope string",
			mutate: func(req *CreateInput) {
				req.Scope = "bogus"
			},
			wantCode: domain.ErrInvalidDelegationScope.Error(),
		},
		{
			name: "scope=department nil ScopeID",
			mutate: func(req *CreateInput) {
				req.Scope = string(domain.ScopeDepartment)
				req.ScopeID = nil
			},
			wantCode: domain.ErrScopeIDRequired.Error(),
		},
		{
			name: "scope=tender nil ScopeID",
			mutate: func(req *CreateInput) {
				req.Scope = string(domain.ScopeTender)
				req.ScopeID = nil
			},
			wantCode: domain.ErrScopeIDRequired.Error(),
		},
		{
			name: "scope=all non-nil ScopeID",
			mutate: func(req *CreateInput) {
				req.Scope = string(domain.ScopeAll)
				req.ScopeID = ptrUUID(uuid.New())
			},
			wantCode: domain.ErrInvalidScopeID.Error(),
		},
		{
			name: "reason > 500 chars",
			mutate: func(req *CreateInput) {
				long := make([]byte, 501)
				for i := range long {
					long[i] = 'a'
				}
				req.Reason = string(long)
			},
			wantCode: domain.ErrReasonTooLong.Error(),
		},
		{
			name: "starts_at in the past beyond skew",
			mutate: func(req *CreateInput) {
				req.StartsAt = ptrTime(time.Now().UTC().Add(-1 * time.Hour))
			},
			wantCode: domain.ErrDelegationStartInPast.Error(),
		},
		{
			name: "starts_at more than 1 year in the future",
			mutate: func(req *CreateInput) {
				req.StartsAt = ptrTime(time.Now().UTC().Add(400 * 24 * time.Hour))
			},
			wantCode: domain.ErrDelegationStartTooFarFuture.Error(),
		},
		{
			name: "ends_at <= starts_at",
			mutate: func(req *CreateInput) {
				req.EndsAt = ptrTime(time.Now().UTC().Add(-time.Minute))
			},
			wantCode: domain.ErrDelegationWindowInverted.Error(),
		},
		{
			name: "fixed span exceeds tenant MaxDurationDays",
			mutate: func(req *CreateInput) {
				req.StartsAt = ptrTime(time.Now().UTC())
				req.EndsAt = ptrTime(time.Now().UTC().Add(100 * 24 * time.Hour))
			},
			settings: &domain.DelegationTenantSettings{MaxDurationDays: 30, ReviewWindowDays: 30},
			wantCode: domain.ErrDelegationWindowTooLong.Error(),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newDelegationHarness()
			if tc.settings != nil {
				h.settings.getResult = tc.settings
			}
			req := validCreateInput()
			tc.mutate(&req)

			got, err := h.svc.Create(context.Background(), uuid.New(), delegatorID, "", req)
			requireDomainCode(t, err, tc.wantCode)
			assert.Nil(t, got)
			assert.Empty(t, h.repo.insertCalls, "no write should happen on a rejected create")
		})
	}
}

// ── Create: membership checks ───────────────────────────────────────────

func TestDelegationService_Create_BothMembershipChecksCalledBeforeUserProfile(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID := uuid.New(), uuid.New()
	req := validCreateInput()
	h.activeBoth(delegatorID, req.DelegateID)

	_, err := h.svc.Create(context.Background(), tenantID, delegatorID, "", req)
	require.NoError(t, err)

	calls := h.rec.snapshot()
	require.Equal(t, 2, countPrefix(calls, "membership.Exists:"), "both membership checks must run")
	upIdx := firstIndex(calls, "userprofile.SetAvailability")
	require.GreaterOrEqual(t, upIdx, 0)
	for i, c := range calls {
		if len(c) >= len("membership.Exists:") && c[:len("membership.Exists:")] == "membership.Exists:" {
			assert.Less(t, i, upIdx, "membership check %q must happen before the User Profile call", c)
		}
	}
	insertIdx := firstIndex(calls, "delegationRepo.Insert")
	require.GreaterOrEqual(t, insertIdx, 0)
	assert.Less(t, upIdx, insertIdx, "User Profile call must happen before the repository write (availability-first)")
}

func TestDelegationService_Create_MembershipCheckErrorFailsClosed(t *testing.T) {
	tests := []struct {
		name          string
		delegatorErr  error
		delegateErr   error
		delegatorGood bool
		delegateGood  bool
	}{
		{name: "delegator check errors", delegatorErr: errors.New("boom")},
		{name: "delegate check errors", delegateErr: errors.New("boom")},
		{name: "both checks error", delegatorErr: errors.New("boom"), delegateErr: errors.New("boom")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newDelegationHarness()
			tenantID, delegatorID := uuid.New(), uuid.New()
			req := validCreateInput()
			h.membership.results[delegatorID] = membershipResult{active: true, id: uuid.New(), err: tc.delegatorErr}
			h.membership.results[req.DelegateID] = membershipResult{active: true, id: uuid.New(), err: tc.delegateErr}

			_, err := h.svc.Create(context.Background(), tenantID, delegatorID, "", req)
			requireDomainCode(t, err, domain.ErrOrgMembershipUnavailable.Error())
			assert.Empty(t, h.repo.insertCalls)
			assert.Zero(t, h.up.callCount(), "user profile must not be called when a membership check errors")
		})
	}
}

func TestDelegationService_Create_MembershipInactiveRejected(t *testing.T) {
	tests := []struct {
		name            string
		delegatorActive bool
		delegateActive  bool
	}{
		{name: "delegator inactive", delegatorActive: false, delegateActive: true},
		{name: "delegate inactive", delegatorActive: true, delegateActive: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newDelegationHarness()
			tenantID, delegatorID := uuid.New(), uuid.New()
			req := validCreateInput()
			h.membership.results[delegatorID] = membershipResult{active: tc.delegatorActive, id: uuid.New()}
			h.membership.results[req.DelegateID] = membershipResult{active: tc.delegateActive, id: uuid.New()}

			_, err := h.svc.Create(context.Background(), tenantID, delegatorID, "", req)
			requireDomainCode(t, err, domain.ErrInvalidDelegate.Error())
			assert.Empty(t, h.repo.insertCalls)
		})
	}
}

// ── Create: availability-first ordering ─────────────────────────────────

func TestDelegationService_Create_UserProfileErrorMapping(t *testing.T) {
	tests := []struct {
		name     string
		upErr    error
		wantCode string
	}{
		{
			name:     "dependency unavailable",
			upErr:    errors.Join(port.ErrDependencyUnavailable, errors.New("timeout")),
			wantCode: domain.ErrUserProfileUnavailable.Error(),
		},
		{
			name:     "delegate_unavailable business rejection",
			upErr:    errors.New("422: delegate_unavailable: delegate is OOO"),
			wantCode: domain.ErrDelegateUnavailable.Error(),
		},
		{
			name:     "other plain error",
			upErr:    errors.New("some other rejection"),
			wantCode: domain.ErrInvalidDelegate.Error(),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newDelegationHarness()
			tenantID, delegatorID := uuid.New(), uuid.New()
			req := validCreateInput()
			h.activeBoth(delegatorID, req.DelegateID)
			h.up.fn = func(ctx context.Context, r port.SetAvailabilityRequest) error { return tc.upErr }

			_, err := h.svc.Create(context.Background(), tenantID, delegatorID, "", req)
			requireDomainCode(t, err, tc.wantCode)
			assert.Empty(t, h.repo.insertCalls, "no write should happen when User Profile rejects availability")
		})
	}
}

// ── Create: happy path ───────────────────────────────────────────────────

func TestDelegationService_Create_HappyPath(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID := uuid.New(), uuid.New()
	req := validCreateInput()
	delegatorMembershipID, delegateMembershipID := h.activeBoth(delegatorID, req.DelegateID)

	got, err := h.svc.Create(context.Background(), tenantID, delegatorID, "", req)
	require.NoError(t, err)
	require.NotNil(t, got)

	require.Len(t, h.repo.insertCalls, 1)
	inserted := h.repo.insertCalls[0]
	assert.Equal(t, delegatorMembershipID, inserted.DelegatorMembershipID)
	assert.Equal(t, delegateMembershipID, inserted.DelegateMembershipID)

	events := h.pub.snapshot()
	require.Len(t, events, 1)
	assert.Equal(t, domain.EventDelegationStarted, events[0].Type)
	payload, ok := events[0].Data.(domain.DelegationStartedPayload)
	require.True(t, ok, "expected DelegationStartedPayload, got %T", events[0].Data)
	assert.Equal(t, got.ID, payload.DelegationID)
	assert.Equal(t, tenantID, payload.TenantID)
	assert.Equal(t, delegatorID, payload.DelegatorID)
	assert.Equal(t, req.DelegateID, payload.DelegateID)
	assert.Equal(t, domain.ScopeAll, payload.Scope)
	assert.WithinDuration(t, got.StartsAt, payload.StartsAt, time.Second)
}

// ── Create: idempotency replay (DLG-D8) ─────────────────────────────────

func TestDelegationService_Create_IdempotencyReplay(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID := uuid.New(), uuid.New()
	req := validCreateInput()
	h.activeBoth(delegatorID, req.DelegateID)

	const key = "idem-key-1"
	first, err := h.svc.Create(context.Background(), tenantID, delegatorID, key, req)
	require.NoError(t, err)

	membershipCallsBefore := h.membership.callCount()
	upCallsBefore := h.up.callCount()
	insertCallsBefore := len(h.repo.insertCalls)

	second, err := h.svc.Create(context.Background(), tenantID, delegatorID, key, req)
	require.NoError(t, err)

	assert.Equal(t, first.ID, second.ID)
	assert.Equal(t, membershipCallsBefore, h.membership.callCount(), "replay must not re-check membership")
	assert.Equal(t, upCallsBefore, h.up.callCount(), "replay must not re-call user profile")
	assert.Equal(t, insertCallsBefore, len(h.repo.insertCalls), "replay must not write again")
}

// ── Cancel ────────────────────────────────────────────────────────────────

func TestDelegationService_Cancel_FailOpenOnUserProfileFailure(t *testing.T) {
	h := newDelegationHarness()
	tenantID := uuid.New()
	d := h.repo.seed(domain.Delegation{
		TenantID: tenantID, DelegatorID: uuid.New(), DelegateID: uuid.New(),
		Scope: domain.ScopeAll, Status: domain.DelegationActive, RecordVersion: 1,
	})
	h.up.fn = func(ctx context.Context, r port.SetAvailabilityRequest) error {
		return errors.New("user-profile down")
	}

	got, err := h.svc.Cancel(context.Background(), tenantID, d.ID, d.RecordVersion)
	require.NoError(t, err, "Cancel must fail-open on a User Profile error")
	require.NotNil(t, got)
	require.Len(t, h.repo.endCalls, 1)
	assert.Equal(t, domain.DelegationCancelled, h.repo.endCalls[0].status)

	events := h.pub.snapshot()
	require.Len(t, events, 1)
	assert.Equal(t, domain.EventDelegationEnded, events[0].Type)
	payload, ok := events[0].Data.(domain.DelegationEndedPayload)
	require.True(t, ok)
	assert.Equal(t, domain.EndReasonCancelled, payload.EndedReason)
}

func TestDelegationService_Cancel_PropagatesOptimisticLockAndNotFound(t *testing.T) {
	tests := []struct {
		name    string
		repoErr error
		want    string
	}{
		{name: "optimistic lock conflict", repoErr: domain.NewError(domain.ErrOptimisticLockConflict, "stale version"), want: domain.ErrOptimisticLockConflict.Error()},
		{name: "delegation not found", repoErr: domain.NewError(domain.ErrDelegationNotFound, "gone"), want: domain.ErrDelegationNotFound.Error()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newDelegationHarness()
			tenantID := uuid.New()
			d := h.repo.seed(domain.Delegation{
				TenantID: tenantID, DelegatorID: uuid.New(), DelegateID: uuid.New(),
				Scope: domain.ScopeAll, Status: domain.DelegationActive, RecordVersion: 1,
			})
			h.repo.endErr = tc.repoErr

			_, err := h.svc.Cancel(context.Background(), tenantID, d.ID, d.RecordVersion)
			requireDomainCode(t, err, tc.want)
		})
	}
}

// ── Extend ────────────────────────────────────────────────────────────────

func TestDelegationService_Extend_DaysOutOfRange(t *testing.T) {
	for _, days := range []int{0, -1, 181, 500} {
		h := newDelegationHarness()
		_, err := h.svc.Extend(context.Background(), uuid.New(), uuid.New(), ptrInt(days), 1)
		requireDomainCode(t, err, domain.ErrExtendDaysOutOfRange.Error())
		assert.Zero(t, h.repo.findByIDCalls, "range check must happen before any repository call")
	}
}

func TestDelegationService_Extend_NonOpenEndedRejected(t *testing.T) {
	h := newDelegationHarness()
	tenantID := uuid.New()
	d := h.repo.seed(domain.Delegation{
		TenantID: tenantID, Status: domain.DelegationActive, EndsAt: ptrTime(time.Now().Add(24 * time.Hour)),
	})
	_, err := h.svc.Extend(context.Background(), tenantID, d.ID, nil, 1)
	requireDomainCode(t, err, domain.ErrNotReviewTracked.Error())
}

func TestDelegationService_Extend_NotActiveRejected(t *testing.T) {
	h := newDelegationHarness()
	tenantID := uuid.New()
	d := h.repo.seed(domain.Delegation{
		TenantID: tenantID, Status: domain.DelegationEnded, EndsAt: nil,
	})
	_, err := h.svc.Extend(context.Background(), tenantID, d.ID, nil, 1)
	requireDomainCode(t, err, domain.ErrDelegationNotFound.Error())
}

func TestDelegationService_Extend_WindowDaysPriority(t *testing.T) {
	rowOverride := 20
	tests := []struct {
		name       string
		extendDays *int
		rowWindow  *int
		tenantDays int
		want       int
	}{
		{name: "caller extendDays wins", extendDays: ptrInt(45), rowWindow: &rowOverride, tenantDays: 10, want: 45},
		{name: "row override wins over tenant default", extendDays: nil, rowWindow: &rowOverride, tenantDays: 10, want: 20},
		{name: "tenant default used when nothing else set", extendDays: nil, rowWindow: nil, tenantDays: 10, want: 10},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newDelegationHarness()
			tenantID := uuid.New()
			h.settings.getResult = &domain.DelegationTenantSettings{MaxDurationDays: 90, ReviewWindowDays: tc.tenantDays}
			d := h.repo.seed(domain.Delegation{
				TenantID: tenantID, Status: domain.DelegationActive, EndsAt: nil, ReviewWindowDays: tc.rowWindow,
			})

			_, err := h.svc.Extend(context.Background(), tenantID, d.ID, tc.extendDays, 1)
			require.NoError(t, err)
			require.Len(t, h.repo.extendCalls, 1)
			assert.Equal(t, tc.want, h.repo.extendCalls[0].windowDays)
		})
	}
}

// ── Reassign ─────────────────────────────────────────────────────────────

func seedReassignable(h *delegationHarness, tenantID uuid.UUID) (domain.Delegation, uuid.UUID, uuid.UUID) {
	delegatorID, delegateID := uuid.New(), uuid.New()
	scopeID := uuid.New()
	d := h.repo.seed(domain.Delegation{
		TenantID: tenantID, DelegatorID: delegatorID, DelegateID: delegateID,
		Scope: domain.ScopeDepartment, ScopeID: &scopeID, Reason: "orig reason",
		Status: domain.DelegationActive, RecordVersion: 1,
		EndsAt: ptrTime(time.Now().Add(48 * time.Hour)),
	})
	return d, delegatorID, delegateID
}

func TestDelegationService_Reassign_CancelThenCreateOrder(t *testing.T) {
	h := newDelegationHarness()
	tenantID := uuid.New()
	d, delegatorID, delegateID := seedReassignable(h, tenantID)
	h.activeBoth(delegatorID, delegateID)

	_, err := h.svc.Reassign(context.Background(), tenantID, d.ID, d.RecordVersion, ReassignInput{})
	require.NoError(t, err)

	events := h.pub.snapshot()
	require.Len(t, events, 2)
	assert.Equal(t, domain.EventDelegationEnded, events[0].Type, "old delegation must be ended first")
	assert.Equal(t, domain.EventDelegationStarted, events[1].Type, "new delegation must be created second")
}

func TestDelegationService_Reassign_OldNotResurrectedOnCreateFailure(t *testing.T) {
	h := newDelegationHarness()
	tenantID := uuid.New()
	d, delegatorID, delegateID := seedReassignable(h, tenantID)
	// Delegator check succeeds but the (unchanged) delegate check fails,
	// forcing the Create leg of Reassign to fail after Cancel has already
	// succeeded.
	h.membership.results[delegatorID] = membershipResult{active: true, id: uuid.New()}
	h.membership.results[delegateID] = membershipResult{active: false, id: uuid.New()}

	_, err := h.svc.Reassign(context.Background(), tenantID, d.ID, d.RecordVersion, ReassignInput{})
	requireDomainCode(t, err, domain.ErrInvalidDelegate.Error())

	assert.Len(t, h.repo.endCalls, 1, "the old delegation must be ended exactly once")
	assert.Empty(t, h.repo.insertCalls, "no new row should ever be inserted when the create leg fails")
}

func TestDelegationService_Reassign_DefaultsFromExistingDelegation(t *testing.T) {
	h := newDelegationHarness()
	tenantID := uuid.New()
	d, delegatorID, delegateID := seedReassignable(h, tenantID)
	h.activeBoth(delegatorID, delegateID)

	_, err := h.svc.Reassign(context.Background(), tenantID, d.ID, d.RecordVersion, ReassignInput{})
	require.NoError(t, err)

	require.Len(t, h.repo.insertCalls, 1)
	inserted := h.repo.insertCalls[0]
	assert.Equal(t, delegateID, inserted.DelegateID)
	assert.Equal(t, domain.ScopeDepartment, inserted.Scope)
	require.NotNil(t, inserted.ScopeID)
	assert.Equal(t, *d.ScopeID, *inserted.ScopeID)
	assert.Equal(t, "orig reason", inserted.Reason)
	require.NotNil(t, inserted.EndsAt, "reassign with no ends_at override must preserve the old delegation's ends_at (LLD §8.4 DLG-5)")
	assert.Equal(t, *d.EndsAt, *inserted.EndsAt)
	assert.Nil(t, inserted.ReviewDueAt, "a fixed-ends_at reassign must not set a review due date")
}

func TestDelegationService_Reassign_OmittedEndsAtOpenEndedPreservesOpenEnded(t *testing.T) {
	h := newDelegationHarness()
	tenantID := uuid.New()
	d, delegatorID, delegateID := seedReassignable(h, tenantID)
	d.EndsAt = nil
	h.repo.seed(d)
	h.activeBoth(delegatorID, delegateID)

	_, err := h.svc.Reassign(context.Background(), tenantID, d.ID, d.RecordVersion, ReassignInput{})
	require.NoError(t, err)

	require.Len(t, h.repo.insertCalls, 1)
	inserted := h.repo.insertCalls[0]
	assert.Nil(t, inserted.EndsAt, "an already open-ended delegation must stay open-ended when ends_at is omitted")
	assert.NotNil(t, inserted.ReviewDueAt, "open-ended reassign must set a review due date")
}

func TestDelegationService_Reassign_EndsAtProvidedNilIsOpenEnded(t *testing.T) {
	h := newDelegationHarness()
	tenantID := uuid.New()
	d, delegatorID, delegateID := seedReassignable(h, tenantID)
	h.activeBoth(delegatorID, delegateID)

	_, err := h.svc.Reassign(context.Background(), tenantID, d.ID, d.RecordVersion, ReassignInput{
		EndsAtProvided: true,
		EndsAt:         nil,
	})
	require.NoError(t, err)

	require.Len(t, h.repo.insertCalls, 1)
	inserted := h.repo.insertCalls[0]
	assert.Nil(t, inserted.EndsAt)
	assert.NotNil(t, inserted.ReviewDueAt)
}

// ── List: cache behavior ──────────────────────────────────────────────────

// DLG1-CACHE-02: cache miss → repo read + cache populated → second call hits cache
func TestDelegationService_List_CacheMissPopulatesCache(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID := uuid.New(), uuid.New()

	// first call: cache miss → repo ListByDelegator called once
	_, err := h.svc.List(context.Background(), tenantID, delegatorID)
	require.NoError(t, err)
	require.Equal(t, 1, countPrefix(h.rec.snapshot(), "delegationRepo.ListByDelegator"),
		"first call must read from repo on a cache miss")

	// second call: cache hit → repo ListByDelegator must NOT be called again
	_, err = h.svc.List(context.Background(), tenantID, delegatorID)
	require.NoError(t, err)
	require.Equal(t, 1, countPrefix(h.rec.snapshot(), "delegationRepo.ListByDelegator"),
		"second call must be served from cache without hitting the repository")
}

// DLG1-CACHE-03: after Create, cache for delegator is invalidated
func TestDelegationService_List_CreateInvalidatesCache(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID := uuid.New(), uuid.New()
	req := validCreateInput()
	h.activeBoth(delegatorID, req.DelegateID)

	// prime the cache
	_, _ = h.svc.List(context.Background(), tenantID, delegatorID)
	require.Equal(t, 0, h.cache.invalidateCalls, "no invalidation yet")

	_, err := h.svc.Create(context.Background(), tenantID, delegatorID, "key1", req)
	require.NoError(t, err)
	require.Equal(t, 1, h.cache.invalidateCalls, "create must invalidate the delegator's list cache")
}

// FM-SVC-08 / BUG02-TC-04: List calls ListByDelegator (delegator-scoped), not the unscoped List
func TestDelegationService_List_RoutesToListByDelegator(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID := uuid.New(), uuid.New()

	_, err := h.svc.List(context.Background(), tenantID, delegatorID)
	require.NoError(t, err)

	calls := h.rec.snapshot()
	require.Equal(t, 1, countPrefix(calls, "delegationRepo.ListByDelegator"),
		"service.List must call ListByDelegator (delegator-scoped), not the unscoped List method")
	// Verify the unscoped List method was never called (exact match to avoid
	// the "delegationRepo.List" prefix also matching "delegationRepo.ListByDelegator").
	var unscopedListCalls int
	for _, c := range calls {
		if c == "delegationRepo.List" {
			unscopedListCalls++
		}
	}
	require.Equal(t, 0, unscopedListCalls, "unscoped List must never be called by service.List")
}

// ── Create: compensating UP clear on tx failure ───────────────────────────

// FM-SVC-01 / FM-SVC-02 / BUG05-TC-01/02/03 / DEV-01:
// When RunInTx fails after UP.SetAvailability succeeded, the service issues
// a compensating ClearDelegate call before returning the error.
func TestDelegationService_Create_CompensatingUPClearOnTxFailure(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID := uuid.New(), uuid.New()
	req := validCreateInput()
	h.activeBoth(delegatorID, req.DelegateID)
	h.repo.insertErr = errors.New("db exploded")

	var upCalls []port.SetAvailabilityRequest
	h.up.fn = func(ctx context.Context, r port.SetAvailabilityRequest) error {
		upCalls = append(upCalls, r)
		return nil
	}

	_, err := h.svc.Create(context.Background(), tenantID, delegatorID, "", req)
	require.Error(t, err)

	// BUG05-TC-01: compensating call was issued
	require.GreaterOrEqual(t, len(upCalls), 2, "UP must be called at least twice: create + compensating clear")

	// FM-SVC-02 / BUG05-TC-03: compensating call shape is ClearDelegate=true, Status=nil
	var clearCall *port.SetAvailabilityRequest
	for i := range upCalls {
		if upCalls[i].ClearDelegate {
			clearCall = &upCalls[i]
		}
	}
	require.NotNil(t, clearCall, "compensating UP clear must be issued")
	assert.True(t, clearCall.ClearDelegate)
	assert.Nil(t, clearCall.Status, "compensating clear must never send {status:available} — pointer-clear only")

	// BUG05-TC-02: compensating clear failure (already nil err here) doesn't mask original error
	require.Error(t, err, "original error must propagate even when compensating clear succeeds")
}

// FM-SVC-05: Create happy path — ReviewWindowDays set from tenant settings
func TestDelegationService_Create_ReviewWindowDaysFromSettings(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID := uuid.New(), uuid.New()
	h.settings.getResult = &domain.DelegationTenantSettings{MaxDurationDays: 90, ReviewWindowDays: 45}
	req := validCreateInput() // open-ended (no EndsAt)
	h.activeBoth(delegatorID, req.DelegateID)

	got, err := h.svc.Create(context.Background(), tenantID, delegatorID, "", req)
	require.NoError(t, err)
	require.NotNil(t, got)

	require.Len(t, h.repo.insertCalls, 1)
	inserted := h.repo.insertCalls[0]
	require.NotNil(t, inserted.ReviewDueAt, "open-ended create must set review_due_at")
	require.NotNil(t, inserted.ReviewWindowDays, "review_window_days must be stored")
	require.Equal(t, 45, *inserted.ReviewWindowDays)
}

// FM-SVC-06 / GAP08-TC-01: DelegationStarted event OccurredAt is non-zero
func TestDelegationService_Create_OccurredAtNonZero(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID := uuid.New(), uuid.New()
	req := validCreateInput()
	h.activeBoth(delegatorID, req.DelegateID)

	_, err := h.svc.Create(context.Background(), tenantID, delegatorID, "k", req)
	require.NoError(t, err)

	events := h.pub.snapshot()
	require.Len(t, events, 1)
	require.Equal(t, domain.EventDelegationStarted, events[0].Type)
	assert.False(t, events[0].OccurredAt.IsZero(), "DomainEvent.OccurredAt must be set by enqueue helper")
}

// FM-SVC-07: idempotency store hit but FindByID returns not-found → error
func TestDelegationService_Create_IdempotencyReplayFindNotFound(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID := uuid.New(), uuid.New()
	req := validCreateInput()

	// pre-populate idempotency store with a delegation ID that doesn't exist in repo
	phantomID := uuid.New()
	_ = h.idem.Save(context.Background(), tenantID, "ghost-key", port.IdempotencyRecord{DelegationID: phantomID})

	_, err := h.svc.Create(context.Background(), tenantID, delegatorID, "ghost-key", req)
	require.Error(t, err, "idempotency replay with missing delegation must return an error, not nil")
}

// DEV-02: idempotency Save() fail is best-effort — delegation still returned
func TestDelegationService_Create_IdempotencySaveFailureIsNonFatal(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID := uuid.New(), uuid.New()
	req := validCreateInput()
	h.activeBoth(delegatorID, req.DelegateID)
	h.idem.saveErr = errors.New("valkey down")

	got, err := h.svc.Create(context.Background(), tenantID, delegatorID, "key1", req)
	require.NoError(t, err, "idempotency Save failure must not fail the create (best-effort §9.2)")
	require.NotNil(t, got)
}

// ── Cancel: actor-id routing ──────────────────────────────────────────────

// FM-SVC-03 / GAP04-TC-02: admin cancels → DelegationEnded.actor_id = admin's ID
func TestDelegationService_Cancel_ActorIDFromContext(t *testing.T) {
	h := newDelegationHarness()
	tenantID := uuid.New()
	adminID := uuid.New()
	d := h.repo.seed(domain.Delegation{
		TenantID: tenantID, DelegatorID: uuid.New(), DelegateID: uuid.New(),
		Scope: domain.ScopeAll, Status: domain.DelegationActive, RecordVersion: 1,
	})

	ctx := requestctx.WithContext(context.Background(), &requestctx.Context{
		UserID: adminID.String(), TenantID: tenantID.String(), Roles: []string{"tenant_admin"},
	})
	_, err := h.svc.Cancel(ctx, tenantID, d.ID, d.RecordVersion)
	require.NoError(t, err)

	events := h.pub.snapshot()
	require.Len(t, events, 1)
	payload, ok := events[0].Data.(domain.DelegationEndedPayload)
	require.True(t, ok)
	assert.Equal(t, adminID, payload.ActorID, "DelegationEnded.actor_id must reflect the actual caller, not the delegator")
}

// GAP04-TC-03: no identity in context → actor_id falls back to delegator_id
func TestDelegationService_Cancel_NoIdentityFallsBackToDelegator(t *testing.T) {
	h := newDelegationHarness()
	tenantID := uuid.New()
	delegatorID := uuid.New()
	d := h.repo.seed(domain.Delegation{
		TenantID: tenantID, DelegatorID: delegatorID, DelegateID: uuid.New(),
		Scope: domain.ScopeAll, Status: domain.DelegationActive, RecordVersion: 1,
	})

	// no requestctx in context (cron/internal path)
	_, err := h.svc.Cancel(context.Background(), tenantID, d.ID, d.RecordVersion)
	require.NoError(t, err)

	events := h.pub.snapshot()
	require.Len(t, events, 1)
	payload, ok := events[0].Data.(domain.DelegationEndedPayload)
	require.True(t, ok)
	assert.Equal(t, delegatorID, payload.ActorID, "without identity context actor_id must fall back to delegator_id")
}

// DLG3-UP-02: Cancel UP clear shape is ClearDelegate=true, Status=nil — never {status:available}
func TestDelegationService_Cancel_UPClearShape(t *testing.T) {
	h := newDelegationHarness()
	tenantID := uuid.New()
	d := h.repo.seed(domain.Delegation{
		TenantID: tenantID, DelegatorID: uuid.New(), DelegateID: uuid.New(),
		Scope: domain.ScopeAll, Status: domain.DelegationActive, RecordVersion: 1,
	})

	var capturedReq port.SetAvailabilityRequest
	h.up.fn = func(ctx context.Context, r port.SetAvailabilityRequest) error {
		capturedReq = r
		return nil
	}

	_, err := h.svc.Cancel(context.Background(), tenantID, d.ID, d.RecordVersion)
	require.NoError(t, err)

	assert.True(t, capturedReq.ClearDelegate, "Cancel must send ClearDelegate=true")
	assert.Nil(t, capturedReq.Status, "Cancel must never send {status:available} — pointer-clear only (DEL-6)")
}

// ── Extend: cache invalidation ────────────────────────────────────────────

// FM-SVC-04 / GAP06-TC-02: Extend calls cache.InvalidateDelegatorList
func TestDelegationService_Extend_CacheInvalidation(t *testing.T) {
	h := newDelegationHarness()
	tenantID := uuid.New()
	d := h.repo.seed(domain.Delegation{
		TenantID: tenantID, DelegatorID: uuid.New(), DelegateID: uuid.New(),
		Status: domain.DelegationActive, Scope: domain.ScopeAll, EndsAt: nil,
	})

	_, err := h.svc.Extend(context.Background(), tenantID, d.ID, nil, 1)
	require.NoError(t, err)
	require.Equal(t, 1, h.cache.invalidateCalls, "Extend must invalidate the delegator's list cache")
}

// ── Reassign: end reason + scope change ──────────────────────────────────

// DLG5-EVT-01: Reassign emits DelegationEnded with ended_reason=reassigned (not cancelled)
func TestDelegationService_Reassign_EmitsEndReasonReassigned(t *testing.T) {
	h := newDelegationHarness()
	tenantID := uuid.New()
	d, delegatorID, delegateID := seedReassignable(h, tenantID)
	h.activeBoth(delegatorID, delegateID)

	_, err := h.svc.Reassign(context.Background(), tenantID, d.ID, d.RecordVersion, ReassignInput{})
	require.NoError(t, err)

	events := h.pub.snapshot()
	var foundEnded bool
	for _, e := range events {
		if e.Type == domain.EventDelegationEnded {
			payload, ok := e.Data.(domain.DelegationEndedPayload)
			require.True(t, ok)
			assert.Equal(t, domain.EndReasonReassigned, payload.EndedReason,
				"Reassign must emit ended_reason=reassigned, not cancelled (DLG5-EVT-01)")
			foundEnded = true
		}
	}
	require.True(t, foundEnded, "Reassign must emit a DelegationEnded event")
}

// USER-08: Reassign allows changing scope from department to all
func TestDelegationService_Reassign_ScopeChange(t *testing.T) {
	h := newDelegationHarness()
	tenantID := uuid.New()
	d, delegatorID, delegateID := seedReassignable(h, tenantID) // scope=department
	h.activeBoth(delegatorID, delegateID)

	newScope := string(domain.ScopeAll)
	_, err := h.svc.Reassign(context.Background(), tenantID, d.ID, d.RecordVersion, ReassignInput{
		Scope:          &newScope,
		ScopeIDSet:     true,
		ScopeID:        nil, // scope=all requires nil ScopeID
		EndsAtProvided: true,
		EndsAt:         d.EndsAt,
	})
	require.NoError(t, err)

	require.Len(t, h.repo.insertCalls, 1)
	inserted := h.repo.insertCalls[0]
	assert.Equal(t, domain.ScopeAll, inserted.Scope, "Reassign must allow scope change from department to all")
	assert.Nil(t, inserted.ScopeID, "scope=all must have nil ScopeID")
}

// DLG7-INTEG-01: after settings max_duration_days=50, create with 51-day span → 422
func TestDelegationService_Settings_MaxDurationAffectsNewDelegations(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID := uuid.New(), uuid.New()
	h.settings.getResult = &domain.DelegationTenantSettings{MaxDurationDays: 50, ReviewWindowDays: 30}
	req := validCreateInput()
	req.StartsAt = ptrTime(time.Now().UTC())
	req.EndsAt = ptrTime(time.Now().UTC().Add(51 * 24 * time.Hour)) // exceeds 50-day limit
	h.activeBoth(delegatorID, req.DelegateID)

	_, err := h.svc.Create(context.Background(), tenantID, delegatorID, "", req)
	requireDomainCode(t, err, domain.ErrDelegationWindowTooLong.Error())
}

// DLG7-INTEG-02: after settings review_window_days=30, open-ended create → review_due_at = starts_at+30d
func TestDelegationService_Settings_ReviewWindowDaysAffectsOpenEndedDelegations(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID := uuid.New(), uuid.New()
	h.settings.getResult = &domain.DelegationTenantSettings{MaxDurationDays: 90, ReviewWindowDays: 30}
	now := time.Now().UTC()
	req := validCreateInput() // open-ended (no EndsAt)
	req.StartsAt = ptrTime(now)
	h.activeBoth(delegatorID, req.DelegateID)

	_, err := h.svc.Create(context.Background(), tenantID, delegatorID, "", req)
	require.NoError(t, err)

	require.Len(t, h.repo.insertCalls, 1)
	inserted := h.repo.insertCalls[0]
	require.NotNil(t, inserted.ReviewDueAt)
	wantDue := now.Add(30 * 24 * time.Hour)
	assert.WithinDuration(t, wantDue, *inserted.ReviewDueAt, time.Minute,
		"review_due_at must be ~starts_at + review_window_days")
}

// BUG-03: reason byte-length limit rejects strings that exceed 500 bytes
func TestDelegationService_Create_ReasonByteLimitRejected(t *testing.T) {
	h := newDelegationHarness()
	delegatorID := uuid.New()
	req := validCreateInput()
	// 501 ASCII bytes → 501 bytes len() → exceeds 500 byte cap
	req.Reason = strings.Repeat("a", 501)

	_, err := h.svc.Create(context.Background(), uuid.New(), delegatorID, "", req)
	requireDomainCode(t, err, domain.ErrReasonTooLong.Error())
}

// HACK-09: Unicode / multi-byte reason within 500 bytes is accepted
func TestDelegationService_Create_UnicodeReasonAccepted(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID := uuid.New(), uuid.New()
	req := validCreateInput()
	// "休暇" is 3 bytes each — 10 runes = 30 bytes, well under 500
	req.Reason = strings.Repeat("休暇", 10)
	h.activeBoth(delegatorID, req.DelegateID)

	got, err := h.svc.Create(context.Background(), tenantID, delegatorID, "", req)
	require.NoError(t, err, "Unicode reason within byte limit must be accepted")
	require.NotNil(t, got)
}

// USER-03 / BUG04-TC-03: using the record_version returned by Extend to Cancel succeeds
func TestDelegationService_Cancel_AfterExtendUsesNewVersion(t *testing.T) {
	h := newDelegationHarness()
	tenantID := uuid.New()
	d := h.repo.seed(domain.Delegation{
		TenantID: tenantID, DelegatorID: uuid.New(), DelegateID: uuid.New(),
		Status: domain.DelegationActive, Scope: domain.ScopeAll, EndsAt: nil,
		RecordVersion: 1,
	})

	extended, err := h.svc.Extend(context.Background(), tenantID, d.ID, nil, d.RecordVersion)
	require.NoError(t, err)

	// cancel using the version returned by extend (not the original)
	_, err = h.svc.Cancel(context.Background(), tenantID, d.ID, extended.RecordVersion)
	require.NoError(t, err)
	require.Len(t, h.repo.endCalls, 1)
}

// ── Reassign: additional coverage ────────────────────────────────────────

// TestDelegationService_Reassign_NonActiveDelegationRejected covers the
// status check at the top of Reassign.
func TestDelegationService_Reassign_NonActiveDelegationRejected(t *testing.T) {
	h := newDelegationHarness()
	tenantID := uuid.New()
	d := h.repo.seed(domain.Delegation{
		TenantID: tenantID, DelegatorID: uuid.New(), DelegateID: uuid.New(),
		Scope: domain.ScopeAll, Status: domain.DelegationEnded, RecordVersion: 1,
	})

	_, err := h.svc.Reassign(context.Background(), tenantID, d.ID, d.RecordVersion, ReassignInput{})
	requireDomainCode(t, err, domain.ErrDelegationNotFound.Error())
}

// TestDelegationService_Reassign_CancelInternalError covers the error path
// when cancelInternal fails (e.g. optimistic lock conflict on the End call).
func TestDelegationService_Reassign_CancelInternalError(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID, delegateID := uuid.New(), uuid.New(), uuid.New()
	h.activeBoth(delegatorID, delegateID)
	d := h.repo.seed(domain.Delegation{
		TenantID: tenantID, DelegatorID: delegatorID, DelegateID: delegateID,
		Scope: domain.ScopeAll, Status: domain.DelegationActive, RecordVersion: 1,
	})
	h.repo.endErr = domain.NewError(domain.ErrOptimisticLockConflict, "stale")

	_, err := h.svc.Reassign(context.Background(), tenantID, d.ID, d.RecordVersion, ReassignInput{})
	requireDomainCode(t, err, domain.ErrOptimisticLockConflict.Error())
}

// TestDelegationService_Reassign_EndsAtOverrideApplied covers the
// EndsAtProvided=true branch that sets the new delegation's EndsAt.
func TestDelegationService_Reassign_EndsAtOverrideApplied(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID, delegateID := uuid.New(), uuid.New(), uuid.New()
	h.activeBoth(delegatorID, delegateID)
	d := h.repo.seed(domain.Delegation{
		TenantID: tenantID, DelegatorID: delegatorID, DelegateID: delegateID,
		Scope: domain.ScopeAll, Status: domain.DelegationActive, RecordVersion: 1,
	})

	newEndsAt := time.Now().Add(72 * time.Hour).UTC()
	_, err := h.svc.Reassign(context.Background(), tenantID, d.ID, d.RecordVersion, ReassignInput{
		EndsAtProvided: true,
		EndsAt:         &newEndsAt,
	})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(h.repo.insertCalls), 1)
	inserted := h.repo.insertCalls[len(h.repo.insertCalls)-1]
	require.NotNil(t, inserted.EndsAt)
	require.True(t, inserted.EndsAt.Equal(newEndsAt) || inserted.EndsAt.After(newEndsAt.Add(-time.Second)))
}

// ── Extend: additional coverage ──────────────────────────────────────────

// TestDelegationService_Extend_NonActiveDelegationRejected covers the
// status check inside Extend.
func TestDelegationService_Extend_NonActiveDelegationRejected(t *testing.T) {
	h := newDelegationHarness()
	tenantID := uuid.New()
	d := h.repo.seed(domain.Delegation{
		TenantID: tenantID, Status: domain.DelegationEnded, RecordVersion: 1,
	})
	_, err := h.svc.Extend(context.Background(), tenantID, d.ID, nil, d.RecordVersion)
	requireDomainCode(t, err, domain.ErrDelegationNotFound.Error())
}

// TestDelegationService_Extend_PerDelegationReviewWindowDays covers the branch
// where the per-delegation ReviewWindowDays override is used (no extendDays
// supplied by the caller and the row has its own override).
func TestDelegationService_Extend_PerDelegationReviewWindowDays(t *testing.T) {
	h := newDelegationHarness()
	tenantID := uuid.New()
	perRowWindow := 30
	d := h.repo.seed(domain.Delegation{
		TenantID: tenantID, Status: domain.DelegationActive, RecordVersion: 1,
		ReviewWindowDays: &perRowWindow,
	})

	_, err := h.svc.Extend(context.Background(), tenantID, d.ID, nil, d.RecordVersion)
	require.NoError(t, err)
	require.Len(t, h.repo.extendCalls, 1)
	require.Equal(t, perRowWindow, h.repo.extendCalls[0].windowDays)
}

// TestDelegationService_Extend_NilCache_NoPanic covers the nil-cache guard.
func TestDelegationService_Extend_NilCache_NoPanic(t *testing.T) {
	rec := &callRecorder{}
	repo := newFakeDelegationRepository(rec)
	settings := &fakeSettingsRepository{rec: rec}
	membership := newFakeMembershipCheckClient(rec)
	up := &fakeUserProfileClient{rec: rec}
	idem := &fakeIdempotencyStore{}
	pub := &fakeEventPublisher{}
	tx := &fakeTxRunner{rec: rec, publisher: pub}
	svc := NewDelegationService(repo, settings, membership, up, idem, nil, tx) // nil cache
	tenantID := uuid.New()
	d := repo.seed(domain.Delegation{
		TenantID: tenantID, Status: domain.DelegationActive, RecordVersion: 1,
	})
	_, err := svc.Extend(context.Background(), tenantID, d.ID, nil, d.RecordVersion)
	require.NoError(t, err) // no panic with nil cache
}

// ── List: nil cache ───────────────────────────────────────────────────────

// TestDelegationService_List_NilCache covers the nil-cache fast-path.
func TestDelegationService_List_NilCache_GoesDirectToRepo(t *testing.T) {
	rec := &callRecorder{}
	repo := newFakeDelegationRepository(rec)
	settings := &fakeSettingsRepository{rec: rec}
	membership := newFakeMembershipCheckClient(rec)
	up := &fakeUserProfileClient{rec: rec}
	idem := &fakeIdempotencyStore{}
	pub := &fakeEventPublisher{}
	tx := &fakeTxRunner{rec: rec, publisher: pub}
	svc := NewDelegationService(repo, settings, membership, up, idem, nil, tx) // nil cache

	tenantID, delegatorID := uuid.New(), uuid.New()
	_, err := svc.List(context.Background(), tenantID, delegatorID)
	require.NoError(t, err)
	require.Equal(t, 1, countPrefix(rec.snapshot(), "delegationRepo.ListByDelegator"))
}

// ── cancelInternal: invalid UserID in context ────────────────────────────

// TestDelegationService_Cancel_InvalidUserIDInContextFallsBackToDelegator
// covers the uuid.Parse(rc.UserID) error branch inside cancelInternal.
func TestDelegationService_Cancel_InvalidUserIDInContextFallsBackToDelegator(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID := uuid.New(), uuid.New()
	d := h.repo.seed(domain.Delegation{
		TenantID: tenantID, DelegatorID: delegatorID, DelegateID: uuid.New(),
		Scope: domain.ScopeAll, Status: domain.DelegationActive, RecordVersion: 1,
	})

	// Put an unparseable UserID into the request context
	rc := &requestctx.Context{TenantID: tenantID.String(), UserID: "not-a-uuid"}
	ctx := requestctx.WithContext(context.Background(), rc)
	ended, err := h.svc.Cancel(ctx, tenantID, d.ID, d.RecordVersion)
	require.NoError(t, err)
	require.NotNil(t, ended)

	events := h.pub.snapshot()
	require.NotEmpty(t, events)
	// The event actor_id must fall back to the delegator
	var endedPayload domain.DelegationEndedPayload
	found := false
	for _, e := range events {
		if p, ok := e.Data.(domain.DelegationEndedPayload); ok {
			endedPayload = p
			found = true
		}
	}
	require.True(t, found)
	require.Equal(t, delegatorID, endedPayload.ActorID)
}

// ── enqueue: IP/UA enrichment from requestctx ───────────────────────────

// TestDelegationService_Create_EnqueuedEventCarriesIPUA covers the
// requestctx.FromContext branch inside enqueue that sets IP/UA on the event.
func TestDelegationService_Create_EnqueuedEventCarriesIPUA(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID := uuid.New(), uuid.New()
	req := validCreateInput()
	h.activeBoth(delegatorID, req.DelegateID)

	rc := &requestctx.Context{
		TenantID:  tenantID.String(),
		UserID:    delegatorID.String(),
		ClientIP:  "203.0.113.1",
		UserAgent: "TestClient/1.0",
	}
	ctx := requestctx.WithContext(context.Background(), rc)

	_, err := h.svc.Create(ctx, tenantID, delegatorID, "idem-key", req)
	require.NoError(t, err)

	events := h.pub.snapshot()
	require.NotEmpty(t, events)
	for _, e := range events {
		if e.Type == domain.EventDelegationStarted {
			require.Equal(t, "203.0.113.1", e.IPAddress)
			require.Equal(t, "TestClient/1.0", e.UserAgent)
			return
		}
	}
	t.Fatal("DelegationStarted event not found")
}

// TestDelegationService_Create_EnqueueCtxError covers the enqueue error
// propagation path when the event publisher returns an error.
func TestDelegationService_Create_EnqueueCtxError(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID := uuid.New(), uuid.New()
	req := validCreateInput()
	h.activeBoth(delegatorID, req.DelegateID)
	h.pub.err = errors.New("event bus down")

	_, err := h.svc.Create(context.Background(), tenantID, delegatorID, "k", req)
	require.Error(t, err)
}
