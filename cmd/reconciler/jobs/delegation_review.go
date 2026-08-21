package jobs

import (
	"context"
	"errors"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
)

// ReviewSweep is DLG-I2 (delegation-review, hourly, LLD §11.4). Three
// passes per tick: warn at the 7-day mark, warn at the 3-day mark, then
// auto-end anything whose review_due_at has passed — each bucket fires at
// most once per cycle via review_last_warned_bucket (DLG-D7/DLG-EVT-5).
// Fixed-ends_at delegations are never considered (the finders all select
// ends_at IS NULL).
func ReviewSweep(ctx context.Context, jctx *Context) (Result, error) {
	var res Result
	now := time.Now().UTC()

	warned7d, failed7d, err := warnBucket(ctx, jctx, 7, func() ([]domain.Delegation, error) {
		return jctx.Delegations.FindDueForWarning7d(ctx, now, batchLimit(jctx))
	})
	if err != nil {
		return res, err
	}
	res.Warned7d, res.Failed = warned7d, res.Failed+failed7d

	warned3d, failed3d, err := warnBucket(ctx, jctx, 3, func() ([]domain.Delegation, error) {
		return jctx.Delegations.FindDueForWarning3d(ctx, now, batchLimit(jctx))
	})
	if err != nil {
		return res, err
	}
	res.Warned3d, res.Failed = warned3d, res.Failed+failed3d

	endTargets, err := jctx.Delegations.FindDueForAutoEnd(ctx, now, batchLimit(jctx))
	if err != nil {
		return res, err
	}
	for _, d := range endTargets {
		if err := jctx.UserProfile.SetAvailability(ctx, port.SetAvailabilityRequest{
			TenantID: d.TenantID, UserID: d.DelegatorID, ClearDelegate: true,
		}); err != nil {
			jctx.Logger.Warn("delegation-review: UP pointer-clear failed — DEL-6 defer",
				map[string]interface{}{"delegation_id": d.ID, "error": err.Error()})
			res.Deferred++
			continue
		}
		raced, err := endReviewExpired(ctx, jctx, d)
		if err != nil {
			jctx.Logger.Warn("delegation-review: auto-end failed",
				map[string]interface{}{"delegation_id": d.ID, "error": err.Error()})
			res.Failed++
			continue
		}
		if !raced {
			res.Expired++
		}
	}

	jctx.Logger.Info("delegation-review complete", map[string]interface{}{
		"warned_7d": res.Warned7d, "warned_3d": res.Warned3d,
		"expired": res.Expired, "deferred": res.Deferred, "failed": res.Failed,
	})
	return res, nil
}

// warnBucket marks and emits DelegationReviewRequested{days_remaining:
// bucket} for every row find returns, inside a per-row tenant-GUC-bound tx.
// A race (another tick/an extend already advanced the row) is skipped, not
// a failure — read back MarkReviewWarned's own optimistic-lock semantics.
func warnBucket(ctx context.Context, jctx *Context, bucket int, find func() ([]domain.Delegation, error)) (warned, failed int, err error) {
	targets, err := find()
	if err != nil {
		return 0, 0, err
	}
	for _, d := range targets {
		gucCtx := jctx.BindTenantGUC(ctx, d.TenantID, systemUserID)
		raced := false
		txErr := jctx.TxRunner.RunInTx(gucCtx, func(txCtx context.Context) error {
			if err := jctx.Delegations.MarkReviewWarned(txCtx, d.TenantID, d.ID, bucket, d.RecordVersion); err != nil {
				if errors.Is(err, domain.ErrDelegationNotFound) || errors.Is(err, domain.ErrOptimisticLockConflict) {
					raced = true
					return nil
				}
				return err
			}
			return enqueueEvent(txCtx, domain.EventDelegationReviewRequested, d.TenantID, d.ID.String(),
				domain.DelegationReviewRequestedPayload{
					DelegationID: d.ID, TenantID: d.TenantID,
					DelegatorID: d.DelegatorID, DelegateID: d.DelegateID,
					Scope: d.Scope, ScopeID: d.ScopeID,
					DaysRemaining: bucket,
					ActorID:       domain.SystemActorID,
				}, "delegation-review")
		})
		if txErr != nil {
			jctx.Logger.Warn("delegation-review: warn failed",
				map[string]interface{}{"delegation_id": d.ID, "bucket": bucket, "error": txErr.Error()})
			failed++
			continue
		}
		if !raced {
			warned++
		}
	}
	return warned, failed, nil
}

func endReviewExpired(ctx context.Context, jctx *Context, d domain.Delegation) (raced bool, err error) {
	gucCtx := jctx.BindTenantGUC(ctx, d.TenantID, systemUserID)
	err = jctx.TxRunner.RunInTx(gucCtx, func(txCtx context.Context) error {
		_, err := jctx.Delegations.End(txCtx, d.TenantID, d.ID, domain.DelegationEnded, d.RecordVersion)
		if err != nil {
			if errors.Is(err, domain.ErrDelegationNotFound) || errors.Is(err, domain.ErrOptimisticLockConflict) {
				raced = true
				return nil
			}
			return err
		}
		return enqueueEvent(txCtx, domain.EventDelegationEnded, d.TenantID, d.ID.String(),
			domain.DelegationEndedPayload{
				DelegationID: d.ID, TenantID: d.TenantID,
				DelegatorID: d.DelegatorID, DelegateID: d.DelegateID,
				Scope: d.Scope, ScopeID: d.ScopeID,
				EndedReason: domain.EndReasonReviewExpired,
				ActorID:     domain.SystemActorID,
			}, "delegation-review")
	})
	return raced, err
}
