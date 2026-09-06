package service

import (
	"context"
	"errors"
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

// ── Create: deferred/scheduled creation (DLG-D25) ───────────────────────

func TestDelegationService_Create_FutureStartsAt_DoesNotActivateImmediately(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID := uuid.New(), uuid.New()
	req := validCreateInput()
	h.activeBoth(delegatorID, req.DelegateID)
	future := time.Now().UTC().Add(48 * time.Hour)
	req.StartsAt = &future

	got, err := h.svc.Create(context.Background(), tenantID, delegatorID, "", req)
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, 0, h.up.callCount(), "User Profile must not be called for a genuinely future starts_at")

	require.Len(t, h.repo.insertCalls, 1)
	assert.Equal(t, domain.DelegationScheduled, h.repo.insertCalls[0].Status)

	events := h.pub.snapshot()
	assert.Empty(t, events, "no DelegationStarted event should be enqueued until the row actually activates")
}

func TestDelegationService_Create_FutureStartsAt_TxFailure_NoCompensatingClear(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID := uuid.New(), uuid.New()
	req := validCreateInput()
	h.activeBoth(delegatorID, req.DelegateID)
	future := time.Now().UTC().Add(48 * time.Hour)
	req.StartsAt = &future
	h.repo.insertErr = errors.New("insert failed")

	_, err := h.svc.Create(context.Background(), tenantID, delegatorID, "", req)
	require.Error(t, err)

	assert.Equal(t, 0, h.up.callCount(), "User Profile must never be called for a scheduled create, even on tx failure")
}

func TestDelegationService_Create_PastOrImmediateStartsAt_StillActivatesImmediately(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID := uuid.New(), uuid.New()
	req := validCreateInput()
	h.activeBoth(delegatorID, req.DelegateID)
	// Within skewTolerance of now — must NOT be treated as scheduled.
	almostNow := time.Now().UTC()
	req.StartsAt = &almostNow

	got, err := h.svc.Create(context.Background(), tenantID, delegatorID, "", req)
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, 1, h.up.callCount())
	require.Len(t, h.repo.insertCalls, 1)
	assert.Equal(t, domain.DelegationActive, h.repo.insertCalls[0].Status)

	events := h.pub.snapshot()
	require.Len(t, events, 1)
	assert.Equal(t, domain.EventDelegationStarted, events[0].Type)
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

// TestDelegationService_Create_OpenEnded_SendsReviewDueAtAsOOOUntil is a
// regression test for a confirmed cross-service bug: open-ended delegations
// (EndsAt == nil, DEL-8 — DLG-4 Extend's and DLG-I2's entire reason for
// existing, not an edge case) used to send OOOUntil: nil to User Profile.
// User Profile's real ValidateOOOWindow unconditionally requires ooo_until
// whenever status="ooo" (reproduced directly against the real HTTP handler
// in iam-user-profile's TestPutAvailability_OOO_NoUntil_IamSystemCaller_
// ReproducesOpenEndedDelegationCreateFailure — 422 "ooo_until is required
// when status is ooo") — so every open-ended Create silently failed in
// production while this repo's own fake UserProfileClient let it pass.
// Fixed by sending the already-computed review_due_at as OOOUntil instead.
func TestDelegationService_Create_OpenEnded_SendsReviewDueAtAsOOOUntil(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID := uuid.New(), uuid.New()
	req := validCreateInput() // EndsAt is nil — open-ended, per validCreateInput's own doc comment
	h.activeBoth(delegatorID, req.DelegateID)

	before := time.Now().UTC()
	got, err := h.svc.Create(context.Background(), tenantID, delegatorID, "", req)
	require.NoError(t, err)
	require.NotNil(t, got)

	require.Len(t, h.up.calls, 1)
	upCall := h.up.calls[0]
	require.NotNil(t, upCall.OOOUntil,
		"OOOUntil must never be nil when Status is ooo — User Profile's real "+
			"ValidateOOOWindow rejects that combination unconditionally")
	settings := domain.DefaultDelegationTenantSettings(tenantID)
	wantUntil := before.Add(time.Duration(settings.ReviewWindowDays) * 24 * time.Hour)
	assert.WithinDuration(t, wantUntil, *upCall.OOOUntil, 5*time.Second,
		"OOOUntil must equal starts_at + tenant review_window_days (the same value stored as review_due_at)")

	// The stored row's own review_due_at must match what was sent to UP —
	// both must stay in sync, or Extend (which re-syncs from the stored
	// value) would silently diverge from what UP actually has.
	require.Len(t, h.repo.insertCalls, 1)
	require.NotNil(t, h.repo.insertCalls[0].ReviewDueAt)
	assert.True(t, upCall.OOOUntil.Equal(*h.repo.insertCalls[0].ReviewDueAt),
		"the ooo_until sent to User Profile must be the exact same value stored as review_due_at")
}

// TestDelegationService_Create_FixedEnd_StillSendsEndsAtAsOOOUntil confirms
// the open-ended fix above didn't regress the ordinary fixed-end case: UP
// still gets the delegation's real ends_at, not review_due_at (which is nil
// for a fixed-end delegation in the first place — only open-ended rows get
// a review_due_at at all).
func TestDelegationService_Create_FixedEnd_StillSendsEndsAtAsOOOUntil(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID := uuid.New(), uuid.New()
	req := validCreateInput()
	endsAt := time.Now().UTC().Add(48 * time.Hour)
	req.EndsAt = &endsAt
	h.activeBoth(delegatorID, req.DelegateID)

	_, err := h.svc.Create(context.Background(), tenantID, delegatorID, "", req)
	require.NoError(t, err)

	require.Len(t, h.up.calls, 1)
	require.NotNil(t, h.up.calls[0].OOOUntil)
	assert.True(t, endsAt.Equal(*h.up.calls[0].OOOUntil))

	require.Len(t, h.repo.insertCalls, 1)
	assert.Nil(t, h.repo.insertCalls[0].ReviewDueAt, "a fixed-end delegation has no review_due_at")
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

// ── List / WithMetrics ───────────────────────────────────────────────────

func TestDelegationService_WithMetrics_ReturnsSameInstance(t *testing.T) {
	h := newDelegationHarness()
	fm := &fakeMetrics{}
	got := h.svc.WithMetrics(fm)
	assert.Same(t, h.svc, got)
}

func TestDelegationService_List_CacheMiss_PopulatesCache(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID := uuid.New(), uuid.New()

	list, err := h.svc.List(context.Background(), tenantID, delegatorID)
	require.NoError(t, err)
	assert.Nil(t, list)

	cached, hit := h.cache.GetDelegatorList(context.Background(), tenantID, delegatorID)
	assert.True(t, hit, "List must populate the cache on a miss")
	assert.Nil(t, cached)
}

func TestDelegationService_List_CacheHit_SkipsRepository(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID := uuid.New(), uuid.New()
	want := []domain.Delegation{{ID: uuid.New(), TenantID: tenantID, DelegatorID: delegatorID}}
	h.cache.SetDelegatorList(context.Background(), tenantID, delegatorID, want)

	got, err := h.svc.List(context.Background(), tenantID, delegatorID)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.NotContains(t, h.rec.snapshot(), "delegationRepo.ListByDelegator", "cache hit must not fall through to the repository")
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

func TestDelegationService_Cancel_RecordsMetrics(t *testing.T) {
	h := newDelegationHarness()
	fm := &fakeMetrics{}
	h.svc.WithMetrics(fm)
	tenantID := uuid.New()
	d := h.repo.seed(domain.Delegation{
		TenantID: tenantID, DelegatorID: uuid.New(), DelegateID: uuid.New(),
		Scope: domain.ScopeAll, Status: domain.DelegationActive, RecordVersion: 1,
	})
	h.up.fn = func(ctx context.Context, r port.SetAvailabilityRequest) error {
		return errors.New("user-profile down")
	}

	_, err := h.svc.Cancel(context.Background(), tenantID, d.ID, d.RecordVersion)
	require.NoError(t, err)

	assert.Equal(t, []string{"cancel"}, fm.upAvailabilityFailures)
	assert.Equal(t, []string{string(domain.EndReasonCancelled)}, fm.ended)
}

func TestDelegationService_Create_RecordsCreatedAndIdempotencyHitMetrics(t *testing.T) {
	h := newDelegationHarness()
	fm := &fakeMetrics{}
	h.svc.WithMetrics(fm)
	tenantID, delegatorID := uuid.New(), uuid.New()
	input := validCreateInput()
	h.activeBoth(delegatorID, input.DelegateID)

	_, err := h.svc.Create(context.Background(), tenantID, delegatorID, "idem-key-1", input)
	require.NoError(t, err)
	assert.Equal(t, []string{string(domain.ScopeAll)}, fm.created)

	// Replay with the same idempotency key.
	_, err = h.svc.Create(context.Background(), tenantID, delegatorID, "idem-key-1", input)
	require.NoError(t, err)
	assert.Equal(t, 1, fm.idempotencyHits)
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

// TestDelegationService_Extend_ResyncsUserProfileOOOUntil is a regression
// test for the Extend half of the open-ended OOOUntil fix: without this,
// extending an open-ended delegation moved review_due_at forward in this
// service's own DB but left User Profile's ooo_until at the OLD value —
// User Profile's own expiry sweep would then reset the delegator to
// 'available' once that stale bound passed, even though the delegation was
// still active (the same class of cross-service desync Create's fix
// closes, reintroduced via a different code path).
func TestDelegationService_Extend_ResyncsUserProfileOOOUntil(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID, delegateID := uuid.New(), uuid.New(), uuid.New()
	starts := time.Now().UTC().Add(-72 * time.Hour)
	newReviewDueAt := time.Now().UTC().Add(20 * 24 * time.Hour)
	d := h.repo.seed(domain.Delegation{
		TenantID: tenantID, DelegatorID: delegatorID, DelegateID: delegateID,
		Status: domain.DelegationActive, EndsAt: nil, StartsAt: starts,
		Reason: "annual leave", RecordVersion: 1,
	})
	h.repo.extendResult = &domain.Delegation{
		ID: d.ID, TenantID: tenantID, DelegatorID: delegatorID, DelegateID: delegateID,
		Status: domain.DelegationActive, EndsAt: nil, StartsAt: starts,
		Reason: "annual leave", ReviewDueAt: &newReviewDueAt, RecordVersion: 2,
	}

	_, err := h.svc.Extend(context.Background(), tenantID, d.ID, ptrInt(20), 1)
	require.NoError(t, err)

	require.Len(t, h.up.calls, 1, "Extend must re-sync User Profile's OOO window")
	upCall := h.up.calls[0]
	require.NotNil(t, upCall.Status)
	assert.Equal(t, "ooo", *upCall.Status)
	assert.Equal(t, delegatorID, upCall.UserID)
	require.NotNil(t, upCall.DelegateID)
	assert.Equal(t, delegateID, *upCall.DelegateID)
	require.NotNil(t, upCall.OOOUntil)
	assert.True(t, newReviewDueAt.Equal(*upCall.OOOUntil),
		"must send the NEW review_due_at, not the pre-extend value")
	require.NotNil(t, upCall.OOOFrom)
	assert.True(t, starts.Equal(*upCall.OOOFrom))
}

// TestDelegationService_Extend_UserProfileFailure_FailsOpen confirms the
// re-sync call is best-effort: a User Profile outage must not block the
// user's own Extend action, matching Cancel's existing DEL-6 fail-open
// pattern (though unlike Cancel, there is no later cron that retries this
// specific re-sync if it fails here — an accepted residual risk).
func TestDelegationService_Extend_UserProfileFailure_FailsOpen(t *testing.T) {
	h := newDelegationHarness()
	fm := &fakeMetrics{}
	h.svc.WithMetrics(fm)
	tenantID := uuid.New()
	d := h.repo.seed(domain.Delegation{
		TenantID: tenantID, Status: domain.DelegationActive, EndsAt: nil, RecordVersion: 1,
	})
	h.up.fn = func(ctx context.Context, r port.SetAvailabilityRequest) error {
		return errors.New("user-profile down")
	}

	got, err := h.svc.Extend(context.Background(), tenantID, d.ID, ptrInt(20), 1)
	require.NoError(t, err, "Extend must fail-open on a User Profile error")
	require.NotNil(t, got)
	assert.Equal(t, []string{"extend"}, fm.upAvailabilityFailures)
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

func TestDelegationService_Reassign_NotActiveRejected(t *testing.T) {
	h := newDelegationHarness()
	tenantID := uuid.New()
	d := h.repo.seed(domain.Delegation{
		TenantID: tenantID, DelegatorID: uuid.New(), DelegateID: uuid.New(),
		Scope: domain.ScopeAll, Status: domain.DelegationCancelled, RecordVersion: 1,
	})

	_, err := h.svc.Reassign(context.Background(), tenantID, d.ID, d.RecordVersion, ReassignInput{})
	requireDomainCode(t, err, domain.ErrDelegationNotFound.Error())
}

func TestDelegationService_Reassign_FindByIDError_Propagates(t *testing.T) {
	h := newDelegationHarness()
	tenantID := uuid.New()
	h.repo.findByIDFn = func(ctx context.Context, gotTenant, gotID uuid.UUID) (*domain.Delegation, error) {
		return nil, domain.NewError(domain.ErrDelegationNotFound, "gone")
	}

	_, err := h.svc.Reassign(context.Background(), tenantID, uuid.New(), 1, ReassignInput{})
	requireDomainCode(t, err, domain.ErrDelegationNotFound.Error())
}

func TestDelegationService_Reassign_CancelError_Propagates(t *testing.T) {
	h := newDelegationHarness()
	tenantID := uuid.New()
	d, delegatorID, delegateID := seedReassignable(h, tenantID)
	h.activeBoth(delegatorID, delegateID)
	h.repo.endErr = domain.NewError(domain.ErrOptimisticLockConflict, "stale version")

	_, err := h.svc.Reassign(context.Background(), tenantID, d.ID, d.RecordVersion, ReassignInput{})
	requireDomainCode(t, err, domain.ErrOptimisticLockConflict.Error())
	assert.Empty(t, h.repo.insertCalls, "a new delegation must never be created when the cancel leg fails")
}

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
				"Reassign must emit ended_reason=reassigned, not cancelled")
			foundEnded = true
		}
	}
	require.True(t, foundEnded, "Reassign must emit a DelegationEnded event")
}

// ── enqueue ──────────────────────────────────────────────────────────────

// ── new gap-coverage tests ────────────────────────────────────────────────

func TestDelegationService_List_RepositoryError(t *testing.T) {
	h := newDelegationHarness()
	wantErr := errors.New("db down")
	h.repo.listByDelegatorErr = wantErr

	_, err := h.svc.List(context.Background(), uuid.New(), uuid.New())
	require.ErrorIs(t, err, wantErr)
}

func TestDelegationService_Create_SettingsGetError(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID := uuid.New(), uuid.New()
	req := validCreateInput()
	h.activeBoth(delegatorID, req.DelegateID)
	wantErr := errors.New("settings unavailable")
	h.settings.getErr = wantErr

	_, err := h.svc.Create(context.Background(), tenantID, delegatorID, "", req)
	require.ErrorIs(t, err, wantErr)
}

func TestDelegationService_Create_PastStartsAtClampsOOOFrom(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID := uuid.New(), uuid.New()
	req := validCreateInput()
	h.activeBoth(delegatorID, req.DelegateID)
	// 3s in the past is within the 5s skewTolerance so it passes validation
	// yet is deterministically before service-internal now, exercising the clamp.
	slightlyPast := time.Now().UTC().Add(-3 * time.Second)
	req.StartsAt = &slightlyPast

	got, err := h.svc.Create(context.Background(), tenantID, delegatorID, "", req)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, 1, h.up.callCount())
	// OOOFrom must be clamped to >= now (not starts_at which is 3s in the past)
	upReq := h.up.calls[0]
	require.NotNil(t, upReq.OOOFrom)
	assert.False(t, upReq.OOOFrom.Before(slightlyPast),
		"OOOFrom must never be before starts_at")
}

func TestDelegationService_Create_UPFailureRecordsMetric(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID := uuid.New(), uuid.New()
	req := validCreateInput()
	h.activeBoth(delegatorID, req.DelegateID)
	fm := &fakeMetrics{}
	h.svc.WithMetrics(fm)
	upErr := errors.New("user profile unavailable")
	h.up.fn = func(_ context.Context, _ port.SetAvailabilityRequest) error { return upErr }

	_, err := h.svc.Create(context.Background(), tenantID, delegatorID, "", req)
	require.Error(t, err)
	assert.Contains(t, fm.upAvailabilityFailures, "create")
}

func TestDelegationService_Create_TxFailure_CompensatesUserProfile(t *testing.T) {
	h := newDelegationHarness()
	tenantID, delegatorID := uuid.New(), uuid.New()
	req := validCreateInput()
	h.activeBoth(delegatorID, req.DelegateID)
	h.repo.insertErr = errors.New("insert failed")

	_, err := h.svc.Create(context.Background(), tenantID, delegatorID, "", req)
	require.Error(t, err)
	// UP called twice: once for OOO set, once for compensating clear
	require.Equal(t, 2, h.up.callCount())
	assert.True(t, h.up.calls[1].ClearDelegate, "second UP call must be a compensating clear")
}

func TestDelegationService_CancelInternal_FindByIDError(t *testing.T) {
	h := newDelegationHarness()
	wantErr := errors.New("not found")
	h.repo.findByIDFn = func(_ context.Context, _, _ uuid.UUID) (*domain.Delegation, error) {
		return nil, wantErr
	}

	_, err := h.svc.Cancel(context.Background(), uuid.New(), uuid.New(), 1)
	require.ErrorIs(t, err, wantErr)
}

func TestDelegationService_CancelInternal_ActorFromRequestContext(t *testing.T) {
	h := newDelegationHarness()
	tenantID := uuid.New()
	actorID := uuid.New()
	d := h.repo.seed(domain.Delegation{
		TenantID: tenantID, DelegatorID: uuid.New(), DelegateID: uuid.New(),
		Scope: domain.ScopeAll, Status: domain.DelegationActive, RecordVersion: 1,
	})
	h.repo.endResult = &d

	ctx := requestctx.WithContext(context.Background(), &requestctx.Context{
		UserID: actorID.String(), TenantID: tenantID.String(),
	})
	got, err := h.svc.Cancel(ctx, tenantID, d.ID, 1)
	require.NoError(t, err)
	require.NotNil(t, got)

	events := h.pub.snapshot()
	require.Len(t, events, 1)
	// requestctx actor flows into the payload's ActorID (not the envelope Actor field)
	payload, ok := events[0].Data.(domain.DelegationEndedPayload)
	require.True(t, ok)
	assert.Equal(t, actorID, payload.ActorID)
}

func TestDelegationService_Extend_FindByIDError(t *testing.T) {
	h := newDelegationHarness()
	wantErr := errors.New("db error")
	h.repo.findByIDFn = func(_ context.Context, _, _ uuid.UUID) (*domain.Delegation, error) {
		return nil, wantErr
	}

	_, err := h.svc.Extend(context.Background(), uuid.New(), uuid.New(), nil, 1)
	require.ErrorIs(t, err, wantErr)
}

func TestDelegationService_Extend_SettingsGetError(t *testing.T) {
	h := newDelegationHarness()
	tenantID := uuid.New()
	d := h.repo.seed(domain.Delegation{
		TenantID: tenantID, DelegatorID: uuid.New(), DelegateID: uuid.New(),
		Scope: domain.ScopeAll, Status: domain.DelegationActive, RecordVersion: 1,
	})
	wantErr := errors.New("settings unavailable")
	h.settings.getErr = wantErr

	_, err := h.svc.Extend(context.Background(), tenantID, d.ID, nil, 1)
	require.ErrorIs(t, err, wantErr)
}

func TestDelegationService_Extend_ExtendReviewError(t *testing.T) {
	h := newDelegationHarness()
	tenantID := uuid.New()
	d := h.repo.seed(domain.Delegation{
		TenantID: tenantID, DelegatorID: uuid.New(), DelegateID: uuid.New(),
		Scope: domain.ScopeAll, Status: domain.DelegationActive, RecordVersion: 1,
	})
	wantErr := errors.New("extend failed")
	h.repo.extendErr = wantErr

	_, err := h.svc.Extend(context.Background(), tenantID, d.ID, nil, 1)
	require.ErrorIs(t, err, wantErr)
}

func ptrStr(s string) *string { return &s }

func TestDelegationService_Reassign_FieldOverrides(t *testing.T) {
	h := newDelegationHarness()
	tenantID := uuid.New()
	newDelegate := uuid.New()
	newScopeID := uuid.New()
	existing := h.repo.seed(domain.Delegation{
		TenantID: tenantID, DelegatorID: uuid.New(), DelegateID: uuid.New(),
		Scope: domain.ScopeAll, Status: domain.DelegationActive, RecordVersion: 1,
		Reason: "old reason",
	})
	h.activeBoth(existing.DelegatorID, newDelegate)
	h.repo.endResult = &existing

	endsAt := time.Now().UTC().Add(7 * 24 * time.Hour)
	req := ReassignInput{
		NewDelegateID:  &newDelegate,
		Scope:          ptrStr(string(domain.ScopeAll)), // covers: scope = *req.Scope
		ScopeIDSet:     true,                            // covers: scopeID = req.ScopeID (nil is valid for scope=all)
		ScopeID:        nil,
		Reason:         ptrStr("new reason"), // covers: reason = *req.Reason
		EndsAtProvided: true,
		EndsAt:         &endsAt,
	}
	_ = newScopeID // declared above but not used with nil ScopeID

	got, err := h.svc.Reassign(context.Background(), tenantID, existing.ID, 1, req)
	require.NoError(t, err)
	require.NotNil(t, got)
}

func TestEnqueue_NoPublisherInContext_NoOp(t *testing.T) {
	err := enqueue(context.Background(), domain.EventDelegationStarted, uuid.New(), "subj", "actor", map[string]string{"k": "v"})
	require.NoError(t, err)
}

func TestEnqueue_CopiesRequestContextIPAndUserAgent(t *testing.T) {
	pub := &fakeEventPublisher{}
	ctx := port.WithEventPublisher(context.Background(), pub)
	ctx = requestctx.WithContext(ctx, &requestctx.Context{ClientIP: "10.0.0.1", UserAgent: "test-agent"})

	err := enqueue(ctx, domain.EventDelegationStarted, uuid.New(), "subj", "actor", map[string]string{"k": "v"})
	require.NoError(t, err)

	events := pub.snapshot()
	require.Len(t, events, 1)
	assert.Equal(t, "10.0.0.1", events[0].IPAddress)
	assert.Equal(t, "test-agent", events[0].UserAgent)
}
