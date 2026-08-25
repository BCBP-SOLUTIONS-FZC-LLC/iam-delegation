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

func (fakeLogger) Info(string, map[string]interface{}) {}
func (fakeLogger) Warn(string, map[string]interface{}) {}

type fakeUserProfile struct {
	failFor map[uuid.UUID]bool // delegatorID -> fail SetAvailability
	calls   []uuid.UUID
}

func (f *fakeUserProfile) SetAvailability(_ context.Context, req port.SetAvailabilityRequest) error {
	f.calls = append(f.calls, req.UserID)
	if f.failFor != nil && f.failFor[req.UserID] {
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

	expiring  []domain.Delegation
	dailyWarn []domain.Delegation
	autoEnd   []domain.Delegation
	endErr    error
	warnErr   error
	endCalls  []uuid.UUID
	warnCalls []struct {
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

func noopBind(ctx context.Context, _ uuid.UUID, _ string) context.Context { return ctx }

// fakeMetrics records calls to the two deferred-counter methods so tests can
// assert GAP-27 closure: the reconciler jobs must actually call these, not
// just have them registered (DLG-D19).
type fakeMetrics struct {
	expiryDeferred int
	reviewDeferred int
}

func (f *fakeMetrics) RecordExpiryDeferred() { f.expiryDeferred++ }
func (f *fakeMetrics) RecordReviewDeferred() { f.reviewDeferred++ }

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

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
