package port

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/google/uuid"
)

// DelegationRepository owns the delegations aggregate (LLD §7.2.1).
type DelegationRepository interface {
	List(ctx context.Context, tenantID uuid.UUID) ([]domain.Delegation, error)
	ListByDelegator(ctx context.Context, tenantID, delegatorID uuid.UUID) ([]domain.Delegation, error)
	FindByID(ctx context.Context, tenantID, id uuid.UUID) (*domain.Delegation, error)

	Insert(ctx context.Context, d *domain.Delegation) (*domain.Delegation, error)

	// End flips status → 'ended'/'cancelled' with optimistic locking and
	// returns the resulting row.
	End(ctx context.Context, tenantID, id uuid.UUID, status domain.DelegationStatus, expectedVersion int64) (*domain.Delegation, error)

	// ExtendReview pushes review_due_at forward by windowDays and resets
	// ReviewLastWarnedBucket to nil, re-arming both warnings (DEL-13, DLG-4,
	// DLG-EVT-5).
	ExtendReview(ctx context.Context, tenantID, id uuid.UUID, windowDays int, expectedVersion int64) (*domain.Delegation, error)

	// ListExpiringBefore is the delegation-expiry cron query (DLG-I1,
	// idx_delegations_ends_at).
	ListExpiringBefore(ctx context.Context, before time.Time, limit int) ([]domain.Delegation, error)

	// FindActiveDeptDelegateForUser returns the active delegation (if any)
	// where the given user is the delegate for a scope='department' grant
	// on the given department. Backs DLG-I3 (Core's §8.8.4 WFI-11
	// department-scope precision). Returns (nil, nil) when none exists.
	FindActiveDeptDelegateForUser(ctx context.Context, tenantID, userID, deptID uuid.UUID) (*domain.Delegation, error)

	// FindActiveByDelegator backs DLG-I4, the escape hatch replacing I-8's
	// removed active_delegations[] (LLD §8.4 DLG-I4, §6.1).
	FindActiveByDelegator(ctx context.Context, tenantID, delegatorID uuid.UUID) ([]domain.Delegation, error)

	// ── DLG-I2 review sweep (LLD §11.4, DLG-D7) ──────────────────────────

	// FindDueForWarning7d returns open-ended active delegations whose
	// review_due_at falls in (now+3d, now+7d] and have not yet been warned
	// this cycle (review_last_warned_bucket IS NULL).
	FindDueForWarning7d(ctx context.Context, now time.Time, limit int) ([]domain.Delegation, error)

	// FindDueForWarning3d returns open-ended active delegations whose
	// review_due_at falls in (now, now+3d] and have not yet received the
	// 3-day warning (review_last_warned_bucket IS DISTINCT FROM 3).
	FindDueForWarning3d(ctx context.Context, now time.Time, limit int) ([]domain.Delegation, error)

	// FindDueForAutoEnd returns open-ended active delegations whose
	// review_due_at has passed (pass 2 — auto-end, ended_reason=review_expired).
	FindDueForAutoEnd(ctx context.Context, now time.Time, limit int) ([]domain.Delegation, error)

	// MarkReviewWarned sets review_last_warned_bucket = bucket (7 or 3).
	MarkReviewWarned(ctx context.Context, tenantID, id uuid.UUID, bucket int, expectedVersion int64) error

	// ── Cascade (LLD §11.5/§11.6) ─────────────────────────────────────────

	// EndForUser cascades on MembershipRevoked — closes (status=ended,
	// deleted_at=now()) any active delegations where the removed user is
	// delegator or delegate. Deliberately not optimistic-locked (terminal
	// bulk cascade, §12.1).
	EndForUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.Delegation, error)

	// SoftDeleteTenant cascades on TenantOffboarded — soft-deletes every
	// delegation row for the tenant (§11.6). No event emission.
	SoftDeleteTenant(ctx context.Context, tenantID uuid.UUID) error

	// HardPurgeSoftDeletedBefore hard-deletes rows soft-deleted more than
	// the retention window ago (delegation-cleanup, §18.4).
	HardPurgeSoftDeletedBefore(ctx context.Context, before time.Time, limit int) (int, error)
}
