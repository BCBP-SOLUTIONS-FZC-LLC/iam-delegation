package jobs

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
)

// Activation is DLG-D25 (delegation-activation, */5 * * * *) — the
// cross-service future-OOO bug fix's forward counterpart to Expiry.
// Availability-first and self-retrying, mirroring Expiry's DEL-6 discipline:
// a UP set-availability failure leaves the row 'scheduled' for the next
// tick rather than activating it with a stale/missing OOO pointer.
//
// A delegation is created 'scheduled' (rather than 'active') when its
// starts_at is genuinely in the future (DelegationService.Create) — no call
// to User Profile and no DelegationStarted event happen at creation time
// for that row. This job is where both finally happen, once starts_at is
// actually reached.
func Activation(ctx context.Context, jctx *Context) (Result, error) {
	var res Result

	targets, err := jctx.Delegations.ListScheduledBefore(ctx, time.Now().UTC(), batchLimit(jctx))
	if err != nil {
		return res, err
	}

	for _, d := range targets {
		res.Attempted++
		oooStatus := "ooo"
		if err := jctx.UserProfile.SetAvailability(ctx, port.SetAvailabilityRequest{
			TenantID: d.TenantID, UserID: d.DelegatorID,
			Status: &oooStatus, OOOFrom: &d.StartsAt, OOOUntil: d.EndsAt,
			DelegateID: &d.DelegateID, Note: d.Reason,
		}); err != nil {
			jctx.Logger.Warn("delegation-activation: UP set-availability failed — DLG-D25 defer",
				map[string]interface{}{"delegation_id": d.ID, "error": err.Error()})
			res.Deferred++
			if jctx.Metrics != nil {
				jctx.Metrics.RecordActivationDeferred()
			}
			continue
		}
		if raced, err := activateScheduled(ctx, jctx, d); err != nil {
			jctx.Logger.Warn("delegation-activation: local activate failed",
				map[string]interface{}{"delegation_id": d.ID, "error": err.Error()})
			res.Failed++
		} else if !raced {
			res.Succeeded++
		}
		// raced (already cancelled, or already activated by a racing tick)
		// counts toward neither succeeded nor failed — the desired
		// end-state already holds.
	}

	jctx.Logger.Info("delegation-activation complete", map[string]interface{}{
		"attempted": res.Attempted, "succeeded": res.Succeeded,
		"failed": res.Failed, "deferred": res.Deferred,
	})
	return res, nil
}

// activateScheduled flips one delegation to active and enqueues
// DelegationStarted inside a single tenant-GUC-bound transaction (RLS-6).
// raced=true means the row no longer matches (Delegations.Activate returns
// (nil, nil) — e.g. an admin cancelled it, or a concurrent tick already
// activated it) — not a failure.
func activateScheduled(ctx context.Context, jctx *Context, d domain.Delegation) (raced bool, err error) {
	gucCtx := jctx.BindTenantGUC(ctx, d.TenantID, systemUserID)
	err = jctx.TxRunner.RunInTx(gucCtx, func(txCtx context.Context) error {
		activated, aerr := jctx.Delegations.Activate(txCtx, d.TenantID, d.ID, d.RecordVersion)
		if aerr != nil {
			return aerr
		}
		if activated == nil {
			raced = true
			return nil
		}
		return enqueueEvent(txCtx, domain.EventDelegationStarted, d.TenantID, d.ID.String(),
			domain.DelegationStartedPayload{
				DelegationID: d.ID, TenantID: d.TenantID,
				DelegatorID: d.DelegatorID, DelegateID: d.DelegateID,
				Scope: d.Scope, ScopeID: d.ScopeID,
				StartsAt: d.StartsAt, EndsAt: d.EndsAt,
				ActorID: domain.SystemActorID,
			}, "delegation-activation")
	})
	return raced, err
}
