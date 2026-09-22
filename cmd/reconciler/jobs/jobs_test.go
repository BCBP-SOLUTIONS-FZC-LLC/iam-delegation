package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
	"github.com/google/uuid"
)

// ── fakes ───────────────────────────────────────────────────────────────

type fakeLogger struct{}

func (fakeLogger) Debug(string, map[string]interface{}) {}
func (fakeLogger) Info(string, map[string]interface{})  {}
func (fakeLogger) Warn(string, map[string]interface{})  {}
func (fakeLogger) Error(string, map[string]interface{}) {}

type fakeUserProfile struct {
	failFor    map[uuid.UUID]bool            // userID -> fail any UP call
	calls      []uuid.UUID                   // userIDs from SetAvailability
	requests   []port.SetAvailabilityRequest // full requests from SetAvailability
	clearCalls []uuid.UUID                   // userIDs from ClearDelegatePointer
}

func (f *fakeUserProfile) SetAvailability(_ context.Context, req port.SetAvailabilityRequest) error {
	f.calls = append(f.calls, req.UserID)
	f.requests = append(f.requests, req)
	if f.failFor != nil && f.failFor[req.UserID] {
		return errors.New("user profile down")
	}
	return nil
}

func (f *fakeUserProfile) GetAvailability(_ context.Context, _, _ uuid.UUID) (*port.AvailabilitySnapshot, error) {
	return &port.AvailabilitySnapshot{Status: "available"}, nil
}

func (f *fakeUserProfile) ClearDelegatePointer(_ context.Context, _, userID uuid.UUID) error {
	f.clearCalls = append(f.clearCalls, userID)
	if f.failFor != nil && f.failFor[userID] {
		return errors.New("user profile down")
	}
	return nil
}

// fakeTxRunner runs fn directly and binds a recording EventPublisher.
type fakeTxRunner struct {
	published []*domain.DomainEvent
}

func (f *fakeTxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	ctx = port.WithEventPublisher(ctx, f)
	return fn(ctx)
}
func (f *fakeTxRunner) EnqueueCtx(_ context.Context, evt *domain.DomainEvent) error {
	f.published = append(f.published, evt)
	return nil
}

// fakeDelegationRepo implements only what the jobs need; embedding
// port.DelegationRepository leaves every other method nil-panicking if
// ever called, which is deliberate — a job calling an unexpected method is
// a bug this should surface loudly.
type fakeDelegationRepo struct {
	port.DelegationRepository

	expiring      []domain.Delegation
	scheduled     []domain.Delegation
	dailyWarn     []domain.Delegation
	autoEnd       []domain.Delegation
	endErr        error
	activateErr   error
	activateNil   bool // Activate returns (nil, nil) — "raced", not an error
	warnErr       error
	endCalls      []uuid.UUID
	activateCalls []uuid.UUID
	warnCalls     []struct {
		id     uuid.UUID
		bucket int
	}
	purgeCount  int
	purgeErr    error
	purgeBefore time.Time
}

func (f *fakeDelegationRepo) ListExpiringBefore(context.Context, time.Time, int) ([]domain.Delegation, error) {
	return f.expiring, nil
}
func (f *fakeDelegationRepo) ListScheduledBefore(context.Context, time.Time, int) ([]domain.Delegation, error) {
	return f.scheduled, nil
}
func (f *fakeDelegationRepo) Activate(_ context.Context, _, id uuid.UUID, _ int64) (*domain.Delegation, error) {
	f.activateCalls = append(f.activateCalls, id)
	if f.activateErr != nil {
		return nil, f.activateErr
	}
	if f.activateNil {
		return nil, nil
	}
	return &domain.Delegation{ID: id}, nil
}
func (f *fakeDelegationRepo) FindDueForDailyWarn(context.Context, time.Time, int) ([]domain.Delegation, error) {
	return f.dailyWarn, nil
}
func (f *fakeDelegationRepo) FindDueForAutoEnd(context.Context, time.Time, int) ([]domain.Delegation, error) {
	return f.autoEnd, nil
}
func (f *fakeDelegationRepo) End(_ context.Context, _, id uuid.UUID, _ domain.DelegationStatus, _ int64) (*domain.Delegation, error) {
	f.endCalls = append(f.endCalls, id)
	if f.endErr != nil {
		return nil, f.endErr
	}
	return &domain.Delegation{ID: id}, nil
}
func (f *fakeDelegationRepo) MarkReviewWarned(_ context.Context, _, id uuid.UUID, bucket int, _ int64) error {
	f.warnCalls = append(f.warnCalls, struct {
		id     uuid.UUID
		bucket int
	}{id, bucket})
	return f.warnErr
}
func (f *fakeDelegationRepo) HardPurgeSoftDeletedBefore(_ context.Context, before time.Time, _ int) (int, error) {
	f.purgeBefore = before
	return f.purgeCount, f.purgeErr
}

type fakeProcessedEvents struct {
	ttlDays int
	limit   int
	err     error
}

func (f *fakeProcessedEvents) Prune(_ context.Context, ttlDays, limit int) (int, error) {
	f.ttlDays = ttlDays
	f.limit = limit
	if f.err != nil {
		return 0, f.err
	}
	return 0, nil
}

func noopBind(ctx context.Context, _ uuid.UUID, _ string) context.Context { return ctx }

// fakeMetrics records calls to the three deferred-counter methods so tests
// can assert GAP-27 closure: the reconciler jobs must actually call these,
// not just have them registered (DLG-D19).
type fakeMetrics struct {
	expiryDeferred     int
	activationDeferred int
	reviewDeferred     int
}

func (f *fakeMetrics) RecordExpiryDeferred()              { f.expiryDeferred++ }
func (f *fakeMetrics) RecordActivationDeferred()          { f.activationDeferred++ }
func (f *fakeMetrics) RecordReviewDeferred()              { f.reviewDeferred++ }
func (f *fakeMetrics) RecordReviewWarned(string)          {}
func (f *fakeMetrics) RecordReviewExpired()               {}
func (f *fakeMetrics) RecordEnded(string)                 {}
func (f *fakeMetrics) RecordCreated(string)               {}
func (f *fakeMetrics) RecordUPAvailabilityFailure(string) {}

func newDelegation(delegatorID uuid.UUID) domain.Delegation {
	return domain.Delegation{
		ID: uuid.New(), TenantID: uuid.New(), DelegatorID: delegatorID, DelegateID: uuid.New(),
		Scope: domain.ScopeAll, Status: domain.DelegationActive, RecordVersion: 1,
	}
}

// ── Expiry ──────────────────────────────────────────────────────────────

func TestExpiry_HappyPath_EndsAndEmits(t *testing.T) {
	d := newDelegation(uuid.New())
	repo := &fakeDelegationRepo{expiring: []domain.Delegation{d}}
	up := &fakeUserProfile{}
	tx := &fakeTxRunner{}
	jctx := &Context{Delegations: repo, UserProfile: up, TxRunner: tx, BindTenantGUC: noopBind, Logger: fakeLogger{}}

	res, err := Expiry(context.Background(), jctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Attempted != 1 || res.Succeeded != 1 || res.Failed != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(repo.endCalls) != 1 || repo.endCalls[0] != d.ID {
		t.Fatalf("End not called with expected id: %v", repo.endCalls)
	}
	if len(tx.published) != 1 || tx.published[0].Type != domain.EventDelegationEnded {
		t.Fatalf("expected one DelegationEnded event, got %+v", tx.published)
	}
	payload := tx.published[0].Data.(domain.DelegationEndedPayload)
	if payload.EndedReason != domain.EndReasonExpired {
		t.Fatalf("expected ended_reason=expired, got %q", payload.EndedReason)
	}
}

func TestExpiry_UPFailure_Defers_NoEndCall(t *testing.T) {
	d := newDelegation(uuid.New())
	repo := &fakeDelegationRepo{expiring: []domain.Delegation{d}}
	up := &fakeUserProfile{failFor: map[uuid.UUID]bool{d.DelegatorID: true}}
	tx := &fakeTxRunner{}
	m := &fakeMetrics{}
	jctx := &Context{Delegations: repo, UserProfile: up, TxRunner: tx, BindTenantGUC: noopBind, Logger: fakeLogger{}, Metrics: m}

	res, err := Expiry(context.Background(), jctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Deferred != 1 || res.Succeeded != 0 || res.Failed != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(repo.endCalls) != 0 {
		t.Fatalf("End must not be called when UP pointer-clear fails (DEL-6): %v", repo.endCalls)
	}
	if len(tx.published) != 0 {
		t.Fatalf("no event should be emitted when the row is deferred: %v", tx.published)
	}
	if m.expiryDeferred != 1 {
		t.Fatalf("expected iam_delegation_expiry_deferred_total to be incremented once (GAP-27), got %d", m.expiryDeferred)
	}
}

func TestExpiry_UPFailure_Defers_NilMetricsDoesNotPanic(t *testing.T) {
	d := newDelegation(uuid.New())
	repo := &fakeDelegationRepo{expiring: []domain.Delegation{d}}
	up := &fakeUserProfile{failFor: map[uuid.UUID]bool{d.DelegatorID: true}}
	tx := &fakeTxRunner{}
	jctx := &Context{Delegations: repo, UserProfile: up, TxRunner: tx, BindTenantGUC: noopBind, Logger: fakeLogger{}}

	if _, err := Expiry(context.Background(), jctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExpiry_Raced_NeitherSucceededNorFailed(t *testing.T) {
	d := newDelegation(uuid.New())
	repo := &fakeDelegationRepo{expiring: []domain.Delegation{d}, endErr: domain.NewError(domain.ErrOptimisticLockConflict, "raced")}
	up := &fakeUserProfile{}
	tx := &fakeTxRunner{}
	jctx := &Context{Delegations: repo, UserProfile: up, TxRunner: tx, BindTenantGUC: noopBind, Logger: fakeLogger{}}

	res, err := Expiry(context.Background(), jctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Succeeded != 0 || res.Failed != 0 || res.Attempted != 1 {
		t.Fatalf("a race should count toward neither succeeded nor failed: %+v", res)
	}
	if len(tx.published) != 0 {
		t.Fatalf("no event should be emitted on a race: %v", tx.published)
	}
}

func TestExpiry_UnexpectedRepoError_CountsAsFailed(t *testing.T) {
	d := newDelegation(uuid.New())
	repo := &fakeDelegationRepo{expiring: []domain.Delegation{d}, endErr: errors.New("db exploded")}
	up := &fakeUserProfile{}
	tx := &fakeTxRunner{}
	jctx := &Context{Delegations: repo, UserProfile: up, TxRunner: tx, BindTenantGUC: noopBind, Logger: fakeLogger{}}

	res, err := Expiry(context.Background(), jctx)
	if err != nil {
		t.Fatalf("unexpected top-level error: %v", err)
	}
	if res.Failed != 1 {
		t.Fatalf("expected 1 failure, got %+v", res)
	}
}

// ── Activation (DLG-D25, cross-service future-OOO bug fix) ────────────────

func newScheduledDelegation(delegatorID uuid.UUID) domain.Delegation {
	d := newDelegation(delegatorID)
	d.Status = domain.DelegationScheduled
	return d
}

func TestActivation_HappyPath_ActivatesAndEmits(t *testing.T) {
	d := newScheduledDelegation(uuid.New())
	repo := &fakeDelegationRepo{scheduled: []domain.Delegation{d}}
	up := &fakeUserProfile{}
	tx := &fakeTxRunner{}
	jctx := &Context{Delegations: repo, UserProfile: up, TxRunner: tx, BindTenantGUC: noopBind, Logger: fakeLogger{}}

	res, err := Activation(context.Background(), jctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Attempted != 1 || res.Succeeded != 1 || res.Failed != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(repo.activateCalls) != 1 || repo.activateCalls[0] != d.ID {
		t.Fatalf("Activate not called with expected id: %v", repo.activateCalls)
	}
	if len(up.calls) != 1 || up.calls[0] != d.DelegatorID {
		t.Fatalf("expected UP SetAvailability to be called for the delegator: %v", up.calls)
	}
	if len(tx.published) != 1 || tx.published[0].Type != domain.EventDelegationStarted {
		t.Fatalf("expected one DelegationStarted event, got %+v", tx.published)
	}
	payload := tx.published[0].Data.(domain.DelegationStartedPayload)
	if payload.ActorID != domain.SystemActorID {
		t.Fatalf("expected actor_id=system for a cron-origin activation, got %q", payload.ActorID)
	}
}

func TestActivation_UPFailure_Defers_NoActivateCall(t *testing.T) {
	d := newScheduledDelegation(uuid.New())
	repo := &fakeDelegationRepo{scheduled: []domain.Delegation{d}}
	up := &fakeUserProfile{failFor: map[uuid.UUID]bool{d.DelegatorID: true}}
	tx := &fakeTxRunner{}
	m := &fakeMetrics{}
	jctx := &Context{Delegations: repo, UserProfile: up, TxRunner: tx, BindTenantGUC: noopBind, Logger: fakeLogger{}, Metrics: m}

	res, err := Activation(context.Background(), jctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Deferred != 1 || res.Succeeded != 0 || res.Failed != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(repo.activateCalls) != 0 {
		t.Fatalf("Activate must not be called when UP set-availability fails (DLG-D25): %v", repo.activateCalls)
	}
	if len(tx.published) != 0 {
		t.Fatalf("no event should be emitted when the row is deferred: %v", tx.published)
	}
	if m.activationDeferred != 1 {
		t.Fatalf("expected iam_delegation_activation_deferred_total to be incremented once, got %d", m.activationDeferred)
	}
}

func TestActivation_UPFailure_Defers_NilMetricsDoesNotPanic(t *testing.T) {
	d := newScheduledDelegation(uuid.New())
	repo := &fakeDelegationRepo{scheduled: []domain.Delegation{d}}
	up := &fakeUserProfile{failFor: map[uuid.UUID]bool{d.DelegatorID: true}}
	tx := &fakeTxRunner{}
	jctx := &Context{Delegations: repo, UserProfile: up, TxRunner: tx, BindTenantGUC: noopBind, Logger: fakeLogger{}}

	if _, err := Activation(context.Background(), jctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestActivation_Raced_NeitherSucceededNorFailed(t *testing.T) {
	d := newScheduledDelegation(uuid.New())
	repo := &fakeDelegationRepo{scheduled: []domain.Delegation{d}, activateNil: true}
	up := &fakeUserProfile{}
	tx := &fakeTxRunner{}
	jctx := &Context{Delegations: repo, UserProfile: up, TxRunner: tx, BindTenantGUC: noopBind, Logger: fakeLogger{}}

	res, err := Activation(context.Background(), jctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Succeeded != 0 || res.Failed != 0 || res.Attempted != 1 {
		t.Fatalf("a race should count toward neither succeeded nor failed: %+v", res)
	}
	if len(tx.published) != 0 {
		t.Fatalf("no event should be emitted on a race: %v", tx.published)
	}
}

func TestActivation_UnexpectedRepoError_CountsAsFailed(t *testing.T) {
	d := newScheduledDelegation(uuid.New())
	repo := &fakeDelegationRepo{scheduled: []domain.Delegation{d}, activateErr: errors.New("db exploded")}
	up := &fakeUserProfile{}
	tx := &fakeTxRunner{}
	jctx := &Context{Delegations: repo, UserProfile: up, TxRunner: tx, BindTenantGUC: noopBind, Logger: fakeLogger{}}

	res, err := Activation(context.Background(), jctx)
	if err != nil {
		t.Fatalf("unexpected top-level error: %v", err)
	}
	if res.Failed != 1 {
		t.Fatalf("expected 1 failure, got %+v", res)
	}
}

func TestActivation_NoScheduledRows_NoOp(t *testing.T) {
	repo := &fakeDelegationRepo{}
	up := &fakeUserProfile{}
	tx := &fakeTxRunner{}
	jctx := &Context{Delegations: repo, UserProfile: up, TxRunner: tx, BindTenantGUC: noopBind, Logger: fakeLogger{}}

	res, err := Activation(context.Background(), jctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Attempted != 0 || res.Succeeded != 0 {
		t.Fatalf("expected a no-op result, got %+v", res)
	}
}

// TestActivation_OpenEnded_SendsReviewDueAtAsOOOUntil is a regression test
// for DLG-D29 found during a third production-readiness pass: a scheduled,
// open-ended delegation (EndsAt == nil — a first-class case, not an edge
// case) sent OOOUntil: nil to User Profile on activation, the exact same
// bug DelegationService.Create was fixed for, missed here because
// Activation is a separate call site. User Profile's real ValidateOOOWindow
// unconditionally rejects status="ooo" with no ooo_until, so this would
// have deferred every single tick forever — the same wrong payload
// rejected the same way each time — never actually activating an
// open-ended scheduled delegation. newDelegation's default (EndsAt/
// ReviewDueAt both nil) is what let this slip past every other Activation
// test: the fake UserProfile never validated the payload shape.
func TestActivation_OpenEnded_SendsReviewDueAtAsOOOUntil(t *testing.T) {
	d := newScheduledDelegation(uuid.New())
	reviewDueAt := time.Now().UTC().Add(90 * 24 * time.Hour)
	d.EndsAt = nil
	d.ReviewDueAt = &reviewDueAt
	repo := &fakeDelegationRepo{scheduled: []domain.Delegation{d}}
	up := &fakeUserProfile{}
	tx := &fakeTxRunner{}
	jctx := &Context{Delegations: repo, UserProfile: up, TxRunner: tx, BindTenantGUC: noopBind, Logger: fakeLogger{}}

	res, err := Activation(context.Background(), jctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Succeeded != 1 {
		t.Fatalf("expected activation to succeed, got %+v", res)
	}
	if len(up.requests) != 1 {
		t.Fatalf("expected 1 SetAvailability call, got %d", len(up.requests))
	}
	got := up.requests[0]
	if got.OOOUntil == nil {
		t.Fatalf("OOOUntil must never be nil when Status is ooo — User Profile's real ValidateOOOWindow rejects that combination unconditionally")
	}
	if !got.OOOUntil.Equal(reviewDueAt) {
		t.Fatalf("expected OOOUntil to fall back to ReviewDueAt %v, got %v", reviewDueAt, *got.OOOUntil)
	}
}

// TestActivation_FixedEnd_StillSendsEndsAtAsOOOUntil confirms the
// open-ended fallback above didn't regress the ordinary fixed-end case.
func TestActivation_FixedEnd_StillSendsEndsAtAsOOOUntil(t *testing.T) {
	d := newScheduledDelegation(uuid.New())
	endsAt := time.Now().UTC().Add(48 * time.Hour)
	d.EndsAt = &endsAt
	repo := &fakeDelegationRepo{scheduled: []domain.Delegation{d}}
	up := &fakeUserProfile{}
	tx := &fakeTxRunner{}
	jctx := &Context{Delegations: repo, UserProfile: up, TxRunner: tx, BindTenantGUC: noopBind, Logger: fakeLogger{}}

	if _, err := Activation(context.Background(), jctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(up.requests) != 1 || up.requests[0].OOOUntil == nil || !up.requests[0].OOOUntil.Equal(endsAt) {
		t.Fatalf("expected OOOUntil to equal the delegation's own ends_at %v, got %+v", endsAt, up.requests)
	}
}

// ── ReviewSweep ─────────────────────────────────────────────────────────

func TestReviewSweep_WarnsDailyCascadeAndAutoEnds(t *testing.T) {
	now := time.Now().UTC()
	// d3 has review_due_at in ~3 days -> daysRemaining=3
	due3d := now.Add(3 * 24 * time.Hour)
	d3 := newDelegation(uuid.New())
	d3.ReviewDueAt = &due3d

	// d2 has review_due_at in ~2 days -> daysRemaining=2
	due2d := now.Add(2 * 24 * time.Hour)
	d2 := newDelegation(uuid.New())
	d2.ReviewDueAt = &due2d

	dEnd := newDelegation(uuid.New())
	repo := &fakeDelegationRepo{
		dailyWarn: []domain.Delegation{d3, d2},
		autoEnd:   []domain.Delegation{dEnd},
	}
	up := &fakeUserProfile{}
	tx := &fakeTxRunner{}
	jctx := &Context{Delegations: repo, UserProfile: up, TxRunner: tx, BindTenantGUC: noopBind, Logger: fakeLogger{}}

	res, err := ReviewSweep(context.Background(), jctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Warned3d != 1 || res.Warned2d != 1 || res.Expired != 1 || res.Failed != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}

	var gotBuckets []int
	for _, c := range repo.warnCalls {
		gotBuckets = append(gotBuckets, c.bucket)
	}
	if len(gotBuckets) != 2 || !containsInt(gotBuckets, 3) || !containsInt(gotBuckets, 2) {
		t.Fatalf("expected MarkReviewWarned called with buckets 3 and 2, got %v", gotBuckets)
	}

	var endedReviewExpired, reviewRequested3, reviewRequested2 int
	for _, evt := range tx.published {
		switch evt.Type {
		case domain.EventDelegationEnded:
			p := evt.Data.(domain.DelegationEndedPayload)
			if p.EndedReason == domain.EndReasonReviewExpired {
				endedReviewExpired++
			}
		case domain.EventDelegationReviewRequested:
			p := evt.Data.(domain.DelegationReviewRequestedPayload)
			switch p.DaysRemaining {
			case 3:
				reviewRequested3++
			case 2:
				reviewRequested2++
			}
		}
	}
	if endedReviewExpired != 1 {
		t.Fatalf("expected one DelegationEnded{review_expired}, got %d", endedReviewExpired)
	}
	if reviewRequested3 != 1 || reviewRequested2 != 1 {
		t.Fatalf("expected one DelegationReviewRequested per bucket, got 3d=%d 2d=%d", reviewRequested3, reviewRequested2)
	}
}

func TestReviewSweep_AutoEnd_UPFailure_Defers(t *testing.T) {
	dEnd := newDelegation(uuid.New())
	repo := &fakeDelegationRepo{autoEnd: []domain.Delegation{dEnd}}
	up := &fakeUserProfile{failFor: map[uuid.UUID]bool{dEnd.DelegatorID: true}}
	tx := &fakeTxRunner{}
	m := &fakeMetrics{}
	jctx := &Context{Delegations: repo, UserProfile: up, TxRunner: tx, BindTenantGUC: noopBind, Logger: fakeLogger{}, Metrics: m}

	res, err := ReviewSweep(context.Background(), jctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Deferred != 1 || res.Expired != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(repo.endCalls) != 0 {
		t.Fatalf("End must not be called when UP pointer-clear fails: %v", repo.endCalls)
	}
	if m.reviewDeferred != 1 {
		t.Fatalf("expected iam_delegation_review_deferred_total to be incremented once (GAP-27), got %d", m.reviewDeferred)
	}
}

func TestReviewSweep_WarnRace_NotCountedAsWarned(t *testing.T) {
	now := time.Now().UTC()
	due3d := now.Add(3 * 24 * time.Hour)
	d := newDelegation(uuid.New())
	d.ReviewDueAt = &due3d
	repo := &fakeDelegationRepo{dailyWarn: []domain.Delegation{d}, warnErr: domain.NewError(domain.ErrDelegationNotFound, "raced")}
	up := &fakeUserProfile{}
	tx := &fakeTxRunner{}
	jctx := &Context{Delegations: repo, UserProfile: up, TxRunner: tx, BindTenantGUC: noopBind, Logger: fakeLogger{}}

	res, err := ReviewSweep(context.Background(), jctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Warned3d != 0 || res.Warned2d != 0 || res.Warned1d != 0 || res.Failed != 0 {
		t.Fatalf("a raced warn should count toward neither warned nor failed: %+v", res)
	}
}

func TestReviewSweep_AlreadyWarnedSameBucket_Skipped(t *testing.T) {
	now := time.Now().UTC()
	due3d := now.Add(3 * 24 * time.Hour)
	bucket3 := 3
	d := newDelegation(uuid.New())
	d.ReviewDueAt = &due3d
	d.ReviewLastWarnedBucket = &bucket3 // already warned at 3d
	repo := &fakeDelegationRepo{dailyWarn: []domain.Delegation{d}}
	up := &fakeUserProfile{}
	tx := &fakeTxRunner{}
	jctx := &Context{Delegations: repo, UserProfile: up, TxRunner: tx, BindTenantGUC: noopBind, Logger: fakeLogger{}}

	res, err := ReviewSweep(context.Background(), jctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Warned3d != 0 || len(repo.warnCalls) != 0 {
		t.Fatalf("already-warned bucket must be skipped: %+v, warnCalls=%v", res, repo.warnCalls)
	}
}

// ── Cleanup ─────────────────────────────────────────────────────────────

func TestCleanup_UsesRetentionDays(t *testing.T) {
	repo := &fakeDelegationRepo{purgeCount: 7}
	jctx := &Context{Delegations: repo, Logger: fakeLogger{}, RetentionDays: 30}

	before := time.Now().UTC()
	res, err := Cleanup(context.Background(), jctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Purged != 7 {
		t.Fatalf("expected Purged=7, got %d", res.Purged)
	}
	wantBefore := before.AddDate(0, 0, -30)
	if diff := repo.purgeBefore.Sub(wantBefore); diff < -time.Minute || diff > time.Minute {
		t.Fatalf("expected purge threshold ~%v, got %v", wantBefore, repo.purgeBefore)
	}
}

func TestCleanup_DefaultsRetentionWhenUnset(t *testing.T) {
	repo := &fakeDelegationRepo{}
	jctx := &Context{Delegations: repo, Logger: fakeLogger{}} // RetentionDays: 0 -> default 90

	before := time.Now().UTC()
	_, err := Cleanup(context.Background(), jctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantBefore := before.AddDate(0, 0, -defaultRetentionDays)
	if diff := repo.purgeBefore.Sub(wantBefore); diff < -time.Minute || diff > time.Minute {
		t.Fatalf("expected default retention ~90d, got threshold %v", repo.purgeBefore)
	}
}

func TestCleanup_UsesProcessedEventsTTLDays(t *testing.T) {
	repo := &fakeDelegationRepo{}
	pe := &fakeProcessedEvents{}
	jctx := &Context{
		Delegations:            repo,
		Logger:                 fakeLogger{},
		ProcessedEvents:        pe,
		ProcessedEventsTTLDays: 8,
		BatchLimit:             25,
	}
	if _, err := Cleanup(context.Background(), jctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pe.ttlDays != 8 || pe.limit != 25 {
		t.Fatalf("Prune called with ttl=%d limit=%d, want 8/25", pe.ttlDays, pe.limit)
	}
}

func TestCleanup_DefaultsProcessedEventsTTLWhenUnset(t *testing.T) {
	repo := &fakeDelegationRepo{}
	pe := &fakeProcessedEvents{}
	jctx := &Context{Delegations: repo, Logger: fakeLogger{}, ProcessedEvents: pe}
	if _, err := Cleanup(context.Background(), jctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pe.ttlDays != defaultProcessedEventsTTLDays {
		t.Fatalf("expected default TTL %d, got %d", defaultProcessedEventsTTLDays, pe.ttlDays)
	}
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// ── computeDaysRemaining ─────────────────────────────────────────────────

// NOTIF-TC-16: computeDaysRemaining returns 3 when review_due_at is 3 days away
func TestComputeDaysRemaining_Returns3ForThreeDays(t *testing.T) {
	now := time.Now().UTC()
	due := now.Add(3 * 24 * time.Hour)
	got := computeDaysRemaining(&due, now)
	if got != 3 {
		t.Fatalf("expected 3, got %d", got)
	}
}

// NOTIF-TC-17: computeDaysRemaining returns 1 (minimum) when review_due_at is past but > 0
func TestComputeDaysRemaining_ReturnsMinimum1WhenPast(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-1 * time.Hour) // past, but auto-end handles exactly <= now
	got := computeDaysRemaining(&past, now)
	if got != 1 {
		t.Fatalf("expected minimum 1, got %d", got)
	}
}

// FM-NOTIF-03: review_due_at = now+48h+1s → days_remaining = ceil((48h+1s)/24h) = 3
func TestComputeDaysRemaining_BucketCeil3(t *testing.T) {
	now := time.Now().UTC()
	due := now.Add(48*time.Hour + time.Second) // slightly over 2 days → ceil = 3
	got := computeDaysRemaining(&due, now)
	if got != 3 {
		t.Fatalf("expected 3 (ceil of 48h+1s), got %d", got)
	}
}

// FM-NOTIF-04: review_due_at exactly now+48h → days_remaining = ceil(2.0) = 2
func TestComputeDaysRemaining_BucketCeil2(t *testing.T) {
	now := time.Now().UTC()
	due := now.Add(48 * time.Hour) // exactly 2 days → ceil = 2
	got := computeDaysRemaining(&due, now)
	if got != 2 {
		t.Fatalf("expected 2 (ceil of exactly 48h), got %d", got)
	}
}

// NOTIF-TC-12: sequential state machine 3d→2d→1d each fires exactly once
func TestReviewSweep_SequentialStateMachine(t *testing.T) {
	now := time.Now().UTC()

	// Run 1: delegation at bucket 3 (3 days remaining)
	due3d := now.Add(3 * 24 * time.Hour)
	d := newDelegation(uuid.New())
	d.ReviewDueAt = &due3d
	repo := &fakeDelegationRepo{dailyWarn: []domain.Delegation{d}}
	tx := &fakeTxRunner{}
	jctx := &Context{Delegations: repo, UserProfile: &fakeUserProfile{}, TxRunner: tx, BindTenantGUC: noopBind, Logger: fakeLogger{}}

	res, err := ReviewSweep(context.Background(), jctx)
	if err != nil {
		t.Fatalf("run 1: unexpected error: %v", err)
	}
	if res.Warned3d != 1 {
		t.Fatalf("run 1: expected Warned3d=1, got %+v", res)
	}

	// Run 2: same delegation now at bucket 2 (bucket=3 already set, 2 days left)
	due2d := now.Add(2 * 24 * time.Hour)
	bucket3 := 3
	d2 := newDelegation(d.DelegatorID)
	d2.ID = d.ID
	d2.ReviewDueAt = &due2d
	d2.ReviewLastWarnedBucket = &bucket3
	repo2 := &fakeDelegationRepo{dailyWarn: []domain.Delegation{d2}}
	tx2 := &fakeTxRunner{}
	jctx2 := &Context{Delegations: repo2, UserProfile: &fakeUserProfile{}, TxRunner: tx2, BindTenantGUC: noopBind, Logger: fakeLogger{}}

	res2, err := ReviewSweep(context.Background(), jctx2)
	if err != nil {
		t.Fatalf("run 2: unexpected error: %v", err)
	}
	if res2.Warned2d != 1 {
		t.Fatalf("run 2: expected Warned2d=1, got %+v", res2)
	}

	// Run 3: same delegation at bucket 1 (bucket=2 already set, 1 day left)
	due1d := now.Add(24 * time.Hour)
	bucket2 := 2
	d3 := newDelegation(d.DelegatorID)
	d3.ID = d.ID
	d3.ReviewDueAt = &due1d
	d3.ReviewLastWarnedBucket = &bucket2
	repo3 := &fakeDelegationRepo{dailyWarn: []domain.Delegation{d3}}
	tx3 := &fakeTxRunner{}
	jctx3 := &Context{Delegations: repo3, UserProfile: &fakeUserProfile{}, TxRunner: tx3, BindTenantGUC: noopBind, Logger: fakeLogger{}}

	res3, err := ReviewSweep(context.Background(), jctx3)
	if err != nil {
		t.Fatalf("run 3: unexpected error: %v", err)
	}
	if res3.Warned1d != 1 {
		t.Fatalf("run 3: expected Warned1d=1, got %+v", res3)
	}

	// Run 4: auto-end (review_due_at passed)
	repo4 := &fakeDelegationRepo{autoEnd: []domain.Delegation{d}}
	tx4 := &fakeTxRunner{}
	jctx4 := &Context{Delegations: repo4, UserProfile: &fakeUserProfile{}, TxRunner: tx4, BindTenantGUC: noopBind, Logger: fakeLogger{}}

	res4, err := ReviewSweep(context.Background(), jctx4)
	if err != nil {
		t.Fatalf("run 4: unexpected error: %v", err)
	}
	if res4.Expired != 1 {
		t.Fatalf("run 4: expected Expired=1, got %+v", res4)
	}
}

// GAP09-TC-03: Cleanup purge error is non-fatal — job returns the error for logging but doesn't panic
func TestCleanup_PurgeErrorIsNonFatal(t *testing.T) {
	repo := &fakeDelegationRepo{purgeErr: errors.New("db timeout"), purgeCount: 0}
	jctx := &Context{Delegations: repo, Logger: fakeLogger{}, RetentionDays: 90}

	_, err := Cleanup(context.Background(), jctx)
	if err == nil {
		t.Fatalf("expected Cleanup to surface the purge error so the caller can log it")
	}
}
