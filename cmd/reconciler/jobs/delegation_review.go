package jobs

import (
	"context"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
)

// ReviewSweep is DLG-I2 (delegation-review, hourly, LLD §11.4).
// Two passes per tick:
//  1. Daily cascade warn — for each open-ended delegation whose review_due_at
//     falls in (now, now+3d], emit DelegationReviewRequested once per
//     calendar-day bucket (days_remaining ∈ {3,2,1}), tracked by
//     review_last_warned_bucket.
//  2. Auto-end — end any delegation whose review_due_at has passed.
func ReviewSweep(ctx context.Context, jctx *Context) (Result, error) {
	var res Result
	now := time.Now().UTC()

	// Pass 1: daily cascade warn
	targets, err := jctx.Delegations.FindDueForDailyWarn(ctx, now, batchLimit(jctx))
	if err != nil {
		return res, err
	}
	for _, d := range targets {
		daysRemaining := computeDaysRemaining(d.ReviewDueAt, now)
		if daysRemaining < 1 {
			continue
		}
		// Already notified for this days_remaining value today — skip
		if d.ReviewLastWarnedBucket != nil && *d.ReviewLastWarnedBucket == daysRemaining {
			continue
		}

		gucCtx := jctx.BindTenantGUC(ctx, d.TenantID, systemUserID)
		raced := false
		txErr := jctx.TxRunner.RunInTx(gucCtx, func(txCtx context.Context) error {
			if err := jctx.Delegations.MarkReviewWarned(txCtx, d.TenantID, d.ID, daysRemaining, d.RecordVersion); err != nil {
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
					DaysRemaining: daysRemaining,
					ActorID:       domain.SystemActorID,
				}, "delegation-review")
		})
		if txErr != nil {
			jctx.Logger.Warn("delegation-review: daily warn failed",
				map[string]interface{}{"delegation_id": d.ID, "days_remaining": daysRemaining, "error": txErr.Error()})
			res.Failed++
			continue
		}
		if !raced {
			switch daysRemaining {
			case 3:
				res.Warned3d++
			case 2:
				res.Warned2d++
			case 1:
				res.Warned1d++
			}
			if jctx.Metrics != nil {
				jctx.Metrics.RecordReviewWarned(strconv.Itoa(daysRemaining))
			}
		}
	}

	// Pass 2: auto-end
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
			if jctx.Metrics != nil {
				jctx.Metrics.RecordReviewDeferred()
				jctx.Metrics.RecordUPAvailabilityFailure("review-cron")
			}
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
			if jctx.Metrics != nil {
				jctx.Metrics.RecordReviewExpired()
				jctx.Metrics.RecordEnded(string(domain.EndReasonReviewExpired))
			}
		}
	}

	jctx.Logger.Info("delegation-review complete", map[string]interface{}{
		"warned_3d": res.Warned3d, "warned_2d": res.Warned2d, "warned_1d": res.Warned1d,
		"expired": res.Expired, "deferred": res.Deferred, "failed": res.Failed,
	})
	return res, nil
}

// computeDaysRemaining returns the number of whole days until reviewDueAt,
// minimum 1 (since auto-end handles the review_due_at <= now case).
func computeDaysRemaining(reviewDueAt *time.Time, now time.Time) int {
	if reviewDueAt == nil {
		return 0
	}
	days := int(math.Ceil(reviewDueAt.Sub(now).Hours() / 24))
	if days < 1 {
		days = 1
	}
	return days
}

// endReviewExpired ends a delegation whose review_due_at has passed.
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
