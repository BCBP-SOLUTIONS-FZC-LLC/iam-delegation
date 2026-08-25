package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// defaultFindLimit is applied whenever a caller passes limit <= 0 to one of
// the bounded finder methods below.
const defaultFindLimit = 100

// DelegationRepository implements port.DelegationRepository against the
// delegations table (LLD §7.2.1). Reads/writes issued from inside a
// port.TxRunner.RunInTx callback join that transaction via txFromContext;
// calls made outside one open a short-lived transaction of their own
// (withPool, db.go).
//
// FindDueForDailyWarn, FindDueForAutoEnd, and HardPurgeSoftDeletedBefore
// deliberately do not filter by tenant_id — they are cross-tenant sweep
// queries (LLD §11.4/§18.4) and therefore require pool to be bound to a
// BYPASSRLS role (delegation_migrator); wiring that pool is the composition
// root's responsibility (cmd/reconciler), not this type's.
type DelegationRepository struct {
	pool *pgcommon.Pool
}

var _ port.DelegationRepository = (*DelegationRepository)(nil)

// NewDelegationRepository builds a DelegationRepository over pool.
func NewDelegationRepository(pool *pgcommon.Pool) *DelegationRepository {
	return &DelegationRepository{pool: pool}
}

const delegationCols = `id, tenant_id, delegator_id, delegate_id,
	delegator_membership_id, delegate_membership_id,
	scope, scope_id, reason, starts_at, ends_at, status,
	record_version, created_at, updated_at, deleted_at,
	review_due_at, review_last_warned_bucket, review_window_days`

func scanDelegation(row pgx.Row) (*domain.Delegation, error) {
	var d domain.Delegation
	var scope, status string
	var reason *string
	if err := row.Scan(
		&d.ID, &d.TenantID, &d.DelegatorID, &d.DelegateID,
		&d.DelegatorMembershipID, &d.DelegateMembershipID,
		&scope, &d.ScopeID, &reason, &d.StartsAt, &d.EndsAt, &status,
		&d.RecordVersion, &d.CreatedAt, &d.UpdatedAt, &d.DeletedAt,
		&d.ReviewDueAt, &d.ReviewLastWarnedBucket, &d.ReviewWindowDays,
	); err != nil {
		return nil, err
	}
	d.Scope = domain.DelegationScope(scope)
	d.Status = domain.DelegationStatus(status)
	if reason != nil {
		d.Reason = *reason
	}
	return &d, nil
}

func limitOrDefault(limit int) int {
	if limit <= 0 {
		return defaultFindLimit
	}
	return limit
}

// List implements port.DelegationRepository.List.
func (r *DelegationRepository) List(ctx context.Context, tenantID uuid.UUID) ([]domain.Delegation, error) {
	return r.listWhere(ctx, `WHERE tenant_id = $1 AND deleted_at IS NULL ORDER BY starts_at DESC`, tenantID)
}

// ListByDelegator implements port.DelegationRepository.ListByDelegator.
func (r *DelegationRepository) ListByDelegator(ctx context.Context, tenantID, delegatorID uuid.UUID) ([]domain.Delegation, error) {
	return r.listWhere(ctx, `WHERE tenant_id = $1 AND delegator_id = $2 AND status = 'active' AND deleted_at IS NULL ORDER BY starts_at DESC`, tenantID, delegatorID)
}

// FindActiveByDelegator implements port.DelegationRepository.FindActiveByDelegator (DLG-I4).
func (r *DelegationRepository) FindActiveByDelegator(ctx context.Context, tenantID, delegatorID uuid.UUID) ([]domain.Delegation, error) {
	return r.listWhere(ctx,
		`WHERE tenant_id = $1 AND delegator_id = $2 AND status = 'active' AND deleted_at IS NULL ORDER BY starts_at DESC`,
		tenantID, delegatorID)
}

func (r *DelegationRepository) listWhere(ctx context.Context, whereClause string, args ...any) ([]domain.Delegation, error) {
	var out []domain.Delegation
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+delegationCols+` FROM delegations `+whereClause, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			d, err := scanDelegation(rows)
			if err != nil {
				return err
			}
			out = append(out, *d)
		}
		return rows.Err()
	})
	return out, err
}

// FindByID implements port.DelegationRepository.FindByID.
func (r *DelegationRepository) FindByID(ctx context.Context, tenantID, id uuid.UUID) (*domain.Delegation, error) {
	var out *domain.Delegation
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+delegationCols+` FROM delegations WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`, tenantID, id)
		d, err := scanDelegation(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.ErrDelegationNotFound, "delegation not found")
			}
			return err
		}
		out = d
		return nil
	})
	return out, err
}

// FindActiveDeptDelegateForUser implements port.DelegationRepository.FindActiveDeptDelegateForUser (DLG-I3).
func (r *DelegationRepository) FindActiveDeptDelegateForUser(ctx context.Context, tenantID, userID, deptID uuid.UUID) (*domain.Delegation, error) {
	var out *domain.Delegation
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+delegationCols+`
			FROM delegations
			WHERE tenant_id = $1 AND delegate_id = $2 AND scope = 'department' AND scope_id = $3
			  AND status = 'active' AND deleted_at IS NULL
			LIMIT 1`,
			tenantID, userID, deptID)
		d, err := scanDelegation(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		out = d
		return nil
	})
	return out, err
}

// Insert implements port.DelegationRepository.Insert.
func (r *DelegationRepository) Insert(ctx context.Context, d *domain.Delegation) (*domain.Delegation, error) {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	if d.StartsAt.IsZero() {
		d.StartsAt = time.Now().UTC()
	}
	if d.Status == "" {
		d.Status = domain.DelegationActive
	}
	var reason *string
	if d.Reason != "" {
		reason = &d.Reason
	}
	var out *domain.Delegation
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO delegations (id, tenant_id, delegator_id, delegate_id,
				delegator_membership_id, delegate_membership_id,
				scope, scope_id, reason, starts_at, ends_at, status, review_due_at, review_window_days)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
			RETURNING `+delegationCols,
			d.ID, d.TenantID, d.DelegatorID, d.DelegateID,
			d.DelegatorMembershipID, d.DelegateMembershipID,
			string(d.Scope), d.ScopeID, reason, d.StartsAt, d.EndsAt, string(d.Status), d.ReviewDueAt, d.ReviewWindowDays)
		created, err := scanDelegation(row)
		if err != nil {
			return err
		}
		out = created
		return nil
	})
	return out, err
}

// End flips status → 'ended'/'cancelled' with optimistic locking (LLD §12.1).
func (r *DelegationRepository) End(ctx context.Context, tenantID, id uuid.UUID, status domain.DelegationStatus, expectedVersion int64) (*domain.Delegation, error) {
	var out *domain.Delegation
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE delegations SET status = $3
			WHERE tenant_id = $1 AND id = $2 AND record_version = $4 AND deleted_at IS NULL AND status = 'active'
			RETURNING `+delegationCols,
			tenantID, id, string(status), expectedVersion)
		d, err := scanDelegation(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return r.probeVersionConflict(ctx, tx, tenantID, id)
			}
			return err
		}
		out = d
		return nil
	})
	return out, err
}

// ExtendReview pushes review_due_at forward by windowDays and resets
// review_last_warned_bucket to NULL, re-arming both warnings (DEL-13, DLG-4,
// DLG-EVT-5).
func (r *DelegationRepository) ExtendReview(ctx context.Context, tenantID, id uuid.UUID, windowDays int, expectedVersion int64) (*domain.Delegation, error) {
	var out *domain.Delegation
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE delegations
			SET review_due_at = review_due_at + make_interval(days => $3),
			    review_last_warned_bucket = NULL,
			    review_window_days = $3
			WHERE tenant_id = $1 AND id = $2 AND record_version = $4
			  AND deleted_at IS NULL AND status = 'active' AND ends_at IS NULL
			RETURNING `+delegationCols,
			// make_interval(days => $3) takes $3 as a native integer, unlike
			// the earlier ($3::text || ' days')::interval formulation —
			// that made Postgres infer $3's parameter type as text (from the
			// explicit cast) for the WHOLE statement, which then broke the
			// bare int-column assignment below it and pgx's ability to
			// encode a Go int against a text-typed placeholder.
			tenantID, id, windowDays, expectedVersion)
		d, err := scanDelegation(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				var v int64
				var s string
				var endAt *time.Time
				probe := tx.QueryRow(ctx, `SELECT record_version, status, ends_at FROM delegations WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`, tenantID, id)
				if perr := probe.Scan(&v, &s, &endAt); perr != nil {
					if errors.Is(perr, pgx.ErrNoRows) {
						return domain.NewError(domain.ErrDelegationNotFound, "delegation not found")
					}
					return perr
				}
				if endAt != nil {
					return domain.NewError(domain.ErrNotReviewTracked, "extend only applies to open-ended delegations (ends_at IS NULL)")
				}
				if s != "active" {
					return domain.NewError(domain.ErrDelegationNotFound, "delegation not found")
				}
				return domain.NewError(domain.ErrOptimisticLockConflict, "record version conflict").
					WithDetails(map[string]any{"record_version": v})
			}
			return err
		}
		out = d
		return nil
	})
	return out, err
}

// probeVersionConflict distinguishes 404 (absent or already-terminal) from
// 409 (optimistic-lock conflict) after a zero-row UPDATE ... RETURNING.
func (r *DelegationRepository) probeVersionConflict(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) error {
	var v int64
	var s string
	probe := tx.QueryRow(ctx, `SELECT record_version, status FROM delegations WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`, tenantID, id)
	if err := probe.Scan(&v, &s); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.ErrDelegationNotFound, "delegation not found")
		}
		return err
	}
	// Terminal state (cancelled/ended) — treat as not found per DEL-3.
	if s != "active" {
		return domain.NewError(domain.ErrDelegationNotFound, "delegation not found")
	}
	return domain.NewError(domain.ErrOptimisticLockConflict, "record version conflict").
		WithDetails(map[string]any{"record_version": v})
}

// ListExpiringBefore implements port.DelegationRepository.ListExpiringBefore (DLG-I1).
func (r *DelegationRepository) ListExpiringBefore(ctx context.Context, before time.Time, limit int) ([]domain.Delegation, error) {
	return r.listWhere(ctx,
		`WHERE ends_at IS NOT NULL AND ends_at < $1 AND status = 'active' AND deleted_at IS NULL ORDER BY ends_at LIMIT $2`,
		before, limitOrDefault(limit))
}

// ── DLG-I2 review sweep (LLD §11.4, DLG-D7) ────────────────────────────────

// FindDueForDailyWarn returns open-ended active delegations whose
// review_due_at falls in (now, now+3d] — candidates for the daily cascade
// notification. days_remaining filtering (to avoid same-day duplicates) is
// done in the job layer against review_last_warned_bucket.
func (r *DelegationRepository) FindDueForDailyWarn(ctx context.Context, now time.Time, limit int) ([]domain.Delegation, error) {
	return r.listWhere(ctx,
		`WHERE ends_at IS NULL AND status = 'active' AND deleted_at IS NULL
		   AND review_due_at > $1
		   AND review_due_at <= $1::timestamptz + interval '3 days'
		 ORDER BY review_due_at LIMIT $2`,
		now, limitOrDefault(limit))
}

// FindDueForAutoEnd returns open-ended active delegations whose
// review_due_at has passed (pass 2 — auto-end, ended_reason=review_expired).
func (r *DelegationRepository) FindDueForAutoEnd(ctx context.Context, now time.Time, limit int) ([]domain.Delegation, error) {
	return r.listWhere(ctx,
		`WHERE ends_at IS NULL AND status = 'active' AND deleted_at IS NULL AND review_due_at <= $1
		 ORDER BY review_due_at LIMIT $2`,
		now, limitOrDefault(limit))
}

// MarkReviewWarned sets review_last_warned_bucket = bucket (days_remaining:
// 3, 2, or 1), with the same optimistic-lock semantics as End/ExtendReview.
func (r *DelegationRepository) MarkReviewWarned(ctx context.Context, tenantID, id uuid.UUID, bucket int, expectedVersion int64) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		cmd, err := tx.Exec(ctx, `
			UPDATE delegations SET review_last_warned_bucket = $3
			WHERE tenant_id = $1 AND id = $2 AND record_version = $4
			  AND deleted_at IS NULL AND status = 'active'`,
			tenantID, id, bucket, expectedVersion)
		if err != nil {
			return err
		}
		if cmd.RowsAffected() == 0 {
			return r.probeVersionConflict(ctx, tx, tenantID, id)
		}
		return nil
	})
}

// ── Cascade (LLD §11.5/§11.6) ───────────────────────────────────────────────

// EndForUser cascades on MembershipRevoked — closes (status=ended,
// deleted_at=now()) any active delegations where userID is delegator or
// delegate. Deliberately not optimistic-locked (terminal bulk cascade,
// LLD §12.1).
func (r *DelegationRepository) EndForUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.Delegation, error) {
	var out []domain.Delegation
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE delegations SET status = 'ended', deleted_at = now()
			WHERE tenant_id = $1 AND (delegator_id = $2 OR delegate_id = $2) AND status = 'active' AND deleted_at IS NULL
			RETURNING `+delegationCols,
			tenantID, userID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			d, err := scanDelegation(rows)
			if err != nil {
				return err
			}
			out = append(out, *d)
		}
		return rows.Err()
	})
	return out, err
}

// SoftDeleteTenant cascades on TenantMembershipsPurged — soft-deletes every
// non-deleted delegation row for the tenant (LLD §11.6). No event emission.
func (r *DelegationRepository) SoftDeleteTenant(ctx context.Context, tenantID uuid.UUID) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE delegations SET deleted_at = now() WHERE tenant_id = $1 AND deleted_at IS NULL`, tenantID)
		return err
	})
}

// HardPurgeSoftDeletedBefore hard-deletes rows soft-deleted more than the
// retention window ago (delegation-cleanup, LLD §18.4). Returns the number
// of rows deleted.
func (r *DelegationRepository) HardPurgeSoftDeletedBefore(ctx context.Context, before time.Time, limit int) (int, error) {
	var n int
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		cmd, err := tx.Exec(ctx, `
			DELETE FROM delegations
			WHERE id IN (
				SELECT id FROM delegations
				WHERE deleted_at IS NOT NULL AND deleted_at < $1
				ORDER BY deleted_at
				LIMIT $2
			)`,
			before, limitOrDefault(limit))
		if err != nil {
			return err
		}
		n = int(cmd.RowsAffected())
		return nil
	})
	return n, err
}
