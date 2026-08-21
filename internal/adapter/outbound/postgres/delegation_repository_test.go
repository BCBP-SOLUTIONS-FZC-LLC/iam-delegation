package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDelegationRepository_Insert_FindByID_RoundTrip(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewDelegationRepository(db.App)

	tenantID := uuid.New()
	ctxA := withTenant(ctx, tenantID)

	d := &domain.Delegation{
		TenantID:              tenantID,
		DelegatorID:           uuid.New(),
		DelegateID:            uuid.New(),
		DelegatorMembershipID: uuid.New(),
		DelegateMembershipID:  uuid.New(),
		Scope:                 domain.ScopeAll,
		Reason:                "annual leave",
		StartsAt:              time.Now().UTC().Truncate(time.Millisecond),
	}
	created, err := repo.Insert(ctxA, d)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, created.ID)
	assert.Equal(t, domain.DelegationActive, created.Status)
	assert.Equal(t, int64(1), created.RecordVersion)
	assert.Equal(t, "annual leave", created.Reason)
	assert.Nil(t, created.EndsAt)
	assert.Nil(t, created.ReviewDueAt)

	found, err := repo.FindByID(ctxA, tenantID, created.ID)
	require.NoError(t, err)
	assert.Equal(t, created.ID, found.ID)
	assert.Equal(t, created.DelegatorID, found.DelegatorID)
	assert.Equal(t, created.DelegateID, found.DelegateID)
	assert.Equal(t, domain.ScopeAll, found.Scope)

	_, err = repo.FindByID(ctxA, tenantID, uuid.New())
	assert.ErrorIs(t, err, domain.ErrDelegationNotFound)
}

func TestDelegationRepository_Insert_OpenEnded_SetsNoEndsAt(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewDelegationRepository(db.App)
	tenantID := uuid.New()
	ctxA := withTenant(ctx, tenantID)

	due := time.Now().UTC().Add(90 * 24 * time.Hour).Truncate(time.Millisecond)
	created, err := repo.Insert(ctxA, &domain.Delegation{
		TenantID:              tenantID,
		DelegatorID:           uuid.New(),
		DelegateID:            uuid.New(),
		DelegatorMembershipID: uuid.New(),
		DelegateMembershipID:  uuid.New(),
		Scope:                 domain.ScopeAll,
		StartsAt:              time.Now().UTC(),
		ReviewDueAt:           &due,
	})
	require.NoError(t, err)
	assert.Nil(t, created.EndsAt)
	require.NotNil(t, created.ReviewDueAt)
	assert.WithinDuration(t, due, *created.ReviewDueAt, time.Second)
	assert.Nil(t, created.ReviewLastWarnedBucket)
}

func TestDelegationRepository_List_ListByDelegator_FindActiveByDelegator(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewDelegationRepository(db.App)
	tenantID := uuid.New()
	delegatorID := uuid.New()

	id1 := seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantID, DelegatorID: delegatorID, Status: "active"})
	id2 := seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantID, DelegatorID: delegatorID, Status: "ended"})
	_ = seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantID, Status: "active"}) // different delegator

	ctxA := withTenant(ctx, tenantID)

	all, err := repo.List(ctxA, tenantID)
	require.NoError(t, err)
	assert.Len(t, all, 3)

	byDelegator, err := repo.ListByDelegator(ctxA, tenantID, delegatorID)
	require.NoError(t, err)
	assert.Len(t, byDelegator, 2)

	active, err := repo.FindActiveByDelegator(ctxA, tenantID, delegatorID)
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.Equal(t, id1, active[0].ID)
	_ = id2
}

func TestDelegationRepository_End_OptimisticLocking(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewDelegationRepository(db.App)
	tenantID := uuid.New()
	ctxA := withTenant(ctx, tenantID)

	id := seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantID, Status: "active"})

	// Wrong version -> 409 optimistic_lock_conflict.
	_, err := repo.End(ctxA, tenantID, id, domain.DelegationCancelled, 999)
	var derr *domain.Error
	require.ErrorAs(t, err, &derr)
	assert.ErrorIs(t, derr, domain.ErrOptimisticLockConflict)
	assert.Equal(t, int64(1), derr.Details["record_version"])

	// Correct version -> success.
	ended, err := repo.End(ctxA, tenantID, id, domain.DelegationCancelled, 1)
	require.NoError(t, err)
	assert.Equal(t, domain.DelegationCancelled, ended.Status)
	assert.Equal(t, int64(2), ended.RecordVersion)

	// Already terminal -> 404, not 409.
	_, err = repo.End(ctxA, tenantID, id, domain.DelegationCancelled, 2)
	assert.ErrorIs(t, err, domain.ErrDelegationNotFound)

	// Absent id -> 404.
	_, err = repo.End(ctxA, tenantID, uuid.New(), domain.DelegationCancelled, 1)
	assert.ErrorIs(t, err, domain.ErrDelegationNotFound)
}

func TestDelegationRepository_ExtendReview(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewDelegationRepository(db.App)
	tenantID := uuid.New()
	ctxA := withTenant(ctx, tenantID)

	due := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Millisecond)
	id := seedDelegation(t, ctx, db.Raw, seedDelegationOpts{
		TenantID: tenantID, Status: "active", ReviewDueAt: &due, ReviewLastWarnedBucket: intPtr(3),
	})

	extended, err := repo.ExtendReview(ctxA, tenantID, id, 30, 1)
	require.NoError(t, err)
	assert.Nil(t, extended.ReviewLastWarnedBucket, "extend must re-arm both warnings")
	require.NotNil(t, extended.ReviewWindowDays)
	assert.Equal(t, 30, *extended.ReviewWindowDays)
	require.NotNil(t, extended.ReviewDueAt)
	assert.WithinDuration(t, due.Add(30*24*time.Hour), *extended.ReviewDueAt, time.Second)
	assert.Equal(t, int64(2), extended.RecordVersion)

	// Fixed ends_at delegation is not review-tracked.
	fixedID := seedDelegation(t, ctx, db.Raw, seedDelegationOpts{
		TenantID: tenantID, Status: "active", EndsAt: timePtr(time.Now().UTC().Add(48 * time.Hour)),
	})
	_, err = repo.ExtendReview(ctxA, tenantID, fixedID, 30, 1)
	assert.ErrorIs(t, err, domain.ErrNotReviewTracked)
}

func TestDelegationRepository_ListExpiringBefore(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewDelegationRepository(db.App)
	tenantID := uuid.New()

	now := time.Now().UTC()
	inScope := seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantID, Status: "active", EndsAt: timePtr(now.Add(1 * time.Hour))})
	_ = seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantID, Status: "active", EndsAt: timePtr(now.Add(48 * time.Hour))}) // out of scope
	_ = seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantID, Status: "active"})                                           // open-ended, out of scope

	ctxA := withTenant(ctx, tenantID)
	rows, err := repo.ListExpiringBefore(ctxA, now.Add(24*time.Hour), 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, inScope, rows[0].ID)
}

func TestDelegationRepository_FindActiveDeptDelegateForUser(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewDelegationRepository(db.App)
	tenantID := uuid.New()
	delegateID := uuid.New()
	deptID := uuid.New()

	_ = seedDelegation(t, ctx, db.Raw, seedDelegationOpts{
		TenantID: tenantID, DelegateID: delegateID, Scope: "department", ScopeID: &deptID, Status: "active",
	})

	ctxA := withTenant(ctx, tenantID)
	found, err := repo.FindActiveDeptDelegateForUser(ctxA, tenantID, delegateID, deptID)
	require.NoError(t, err)
	require.NotNil(t, found)
	assert.Equal(t, deptID, *found.ScopeID)

	none, err := repo.FindActiveDeptDelegateForUser(ctxA, tenantID, delegateID, uuid.New())
	require.NoError(t, err)
	assert.Nil(t, none)
}

// TestDelegationRepository_ReviewSweepFinders exercises the exact predicates
// of LLD §11.4: rows are seeded straddling each window boundary.
func TestDelegationRepository_ReviewSweepFinders(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	// These three finders have no tenant_id predicate (LLD §11.4 — they are
	// cross-tenant cron sweeps), so they must run against a BYPASSRLS pool
	// exactly like the delegation_migrator role wired for the real cron.
	repo := NewDelegationRepository(db.Bypass)
	tenantID := uuid.New()
	now := time.Now().UTC()

	// 7d window: (now+3d, now+7d], review_last_warned_bucket IS NULL.
	in7d := seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantID, Status: "active", ReviewDueAt: timePtr(now.Add(5 * 24 * time.Hour))})
	// Exactly at the now+7d boundary — inclusive.
	at7dBoundary := seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantID, Status: "active", ReviewDueAt: timePtr(now.Add(7 * 24 * time.Hour))})
	// Just past now+7d — excluded.
	_ = seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantID, Status: "active", ReviewDueAt: timePtr(now.Add(7*24*time.Hour + time.Minute))})
	// Exactly at the now+3d boundary — excluded from the 7d window (boundary
	// is exclusive on the near side there), but this same row legitimately
	// falls inside the 3d window's inclusive far boundary too (a row due in
	// exactly 3 days is a real 3-day-warning candidate) — asserted below.
	sharedAt3dBoundary := seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantID, Status: "active", ReviewDueAt: timePtr(now.Add(3 * 24 * time.Hour))})
	// In-window but already warned this cycle — excluded.
	_ = seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantID, Status: "active", ReviewDueAt: timePtr(now.Add(6 * 24 * time.Hour)), ReviewLastWarnedBucket: intPtr(7)})

	// 3d window: (now, now+3d], review_last_warned_bucket IS DISTINCT FROM 3.
	in3d := seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantID, Status: "active", ReviewDueAt: timePtr(now.Add(2 * 24 * time.Hour))})
	at3dBoundary := seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantID, Status: "active", ReviewDueAt: timePtr(now.Add(3 * 24 * time.Hour))})
	// Already warned at 3 -> excluded.
	_ = seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantID, Status: "active", ReviewDueAt: timePtr(now.Add(1 * 24 * time.Hour)), ReviewLastWarnedBucket: intPtr(3)})
	// Exactly at now -> excluded (near-side exclusive).
	_ = seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantID, Status: "active", ReviewDueAt: timePtr(now)})

	// Auto-end: review_due_at <= now.
	overdue := seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantID, Status: "active", ReviewDueAt: timePtr(now.Add(-time.Hour))})
	exactlyNow := seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantID, Status: "active", ReviewDueAt: timePtr(now)})
	notYetDue := seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantID, Status: "active", ReviewDueAt: timePtr(now.Add(time.Hour))})

	got7d, err := repo.FindDueForWarning7d(ctx, now, 100)
	require.NoError(t, err)
	assert.ElementsMatch(t, []uuid.UUID{in7d, at7dBoundary}, idsOf(got7d))

	got3d, err := repo.FindDueForWarning3d(ctx, now, 100)
	require.NoError(t, err)
	// notYetDue (seeded below for the auto-end pass, due now+1h) also falls
	// legitimately inside the 3d window's (now, now+3d] range — a row can be
	// a genuine 3-day-warning candidate and not-yet-auto-end-due at once.
	assert.ElementsMatch(t, []uuid.UUID{in3d, at3dBoundary, sharedAt3dBoundary, notYetDue}, idsOf(got3d))

	gotAutoEnd, err := repo.FindDueForAutoEnd(ctx, now, 100)
	require.NoError(t, err)
	autoEndIDs := idsOf(gotAutoEnd)
	assert.Contains(t, autoEndIDs, overdue)
	assert.Contains(t, autoEndIDs, exactlyNow)
	assert.NotContains(t, autoEndIDs, notYetDue)
}

func idsOf(ds []domain.Delegation) []uuid.UUID {
	out := make([]uuid.UUID, len(ds))
	for i, d := range ds {
		out[i] = d.ID
	}
	return out
}

func TestDelegationRepository_MarkReviewWarned(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewDelegationRepository(db.App)
	tenantID := uuid.New()
	ctxA := withTenant(ctx, tenantID)

	due := time.Now().UTC().Add(5 * 24 * time.Hour)
	id := seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantID, Status: "active", ReviewDueAt: &due})

	require.NoError(t, repo.MarkReviewWarned(ctxA, tenantID, id, 7, 1))

	found, err := repo.FindByID(ctxA, tenantID, id)
	require.NoError(t, err)
	require.NotNil(t, found.ReviewLastWarnedBucket)
	assert.Equal(t, 7, *found.ReviewLastWarnedBucket)
	assert.Equal(t, int64(2), found.RecordVersion)

	// Stale version -> conflict.
	err = repo.MarkReviewWarned(ctxA, tenantID, id, 3, 1)
	assert.ErrorIs(t, err, domain.ErrOptimisticLockConflict)
}

func TestDelegationRepository_EndForUser(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewDelegationRepository(db.App)
	tenantID := uuid.New()
	userID := uuid.New()
	ctxA := withTenant(ctx, tenantID)

	asDelegator := seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantID, DelegatorID: userID, Status: "active"})
	asDelegate := seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantID, DelegateID: userID, Status: "active"})
	unrelated := seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantID, Status: "active"})

	ended, err := repo.EndForUser(ctxA, tenantID, userID)
	require.NoError(t, err)
	endedIDs := idsOf(ended)
	assert.ElementsMatch(t, []uuid.UUID{asDelegator, asDelegate}, endedIDs)
	for _, d := range ended {
		assert.Equal(t, domain.DelegationEnded, d.Status)
		assert.NotNil(t, d.DeletedAt)
	}

	stillActive, err := repo.FindByID(ctxA, tenantID, unrelated)
	require.NoError(t, err)
	assert.Equal(t, domain.DelegationActive, stillActive.Status)
}

func TestDelegationRepository_SoftDeleteTenant(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewDelegationRepository(db.App)
	tenantID := uuid.New()
	ctxA := withTenant(ctx, tenantID)

	id1 := seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantID, Status: "active"})
	id2 := seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantID, Status: "ended"})

	require.NoError(t, repo.SoftDeleteTenant(ctxA, tenantID))

	var deletedAt1, deletedAt2 *time.Time
	require.NoError(t, db.Raw.QueryRow(ctx, `SELECT deleted_at FROM delegations WHERE id = $1`, id1).Scan(&deletedAt1))
	require.NoError(t, db.Raw.QueryRow(ctx, `SELECT deleted_at FROM delegations WHERE id = $1`, id2).Scan(&deletedAt2))
	assert.NotNil(t, deletedAt1)
	assert.NotNil(t, deletedAt2)
}

func TestDelegationRepository_HardPurgeSoftDeletedBefore(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewDelegationRepository(db.App)
	tenantID := uuid.New()
	ctxA := withTenant(ctx, tenantID)

	old := seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantID, Status: "ended", DeletedAt: timePtr(time.Now().UTC().Add(-100 * 24 * time.Hour))})
	recent := seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantID, Status: "ended", DeletedAt: timePtr(time.Now().UTC().Add(-1 * time.Hour))})
	notDeleted := seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantID, Status: "active"})

	n, err := repo.HardPurgeSoftDeletedBefore(ctxA, time.Now().UTC().Add(-90*24*time.Hour), 100)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	var count int
	require.NoError(t, db.Raw.QueryRow(ctx, `SELECT count(*) FROM delegations WHERE id = $1`, old).Scan(&count))
	assert.Equal(t, 0, count, "old soft-deleted row must be hard-purged")

	require.NoError(t, db.Raw.QueryRow(ctx, `SELECT count(*) FROM delegations WHERE id IN ($1, $2)`, recent, notDeleted).Scan(&count))
	assert.Equal(t, 2, count, "recent and non-deleted rows must survive")
}
