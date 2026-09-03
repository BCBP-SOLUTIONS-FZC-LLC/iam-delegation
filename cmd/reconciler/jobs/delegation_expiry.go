package jobs

import (
	"context"
	"errors"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
	"github.com/google/uuid"
)

// Expiry is DLG-I1 (delegation-expiry, */5 * * * *, LLD §11.3). Availability-
// first and self-retrying per DEL-6: a UP pointer-clear failure leaves the
// row active for the next tick rather than ending it with a stale pointer.
func Expiry(ctx context.Context, jctx *Context) (Result, error) {
	var res Result

	targets, err := jctx.Delegations.ListExpiringBefore(ctx, time.Now().UTC(), batchLimit(jctx))
	if err != nil {
		return res, err
	}

	for _, d := range targets {
		res.Attempted++
		if err := jctx.UserProfile.SetAvailability(ctx, port.SetAvailabilityRequest{
			TenantID: d.TenantID, UserID: d.DelegatorID, ClearDelegate: true,
		}); err != nil {
			jctx.Logger.Warn("delegation-expiry: UP pointer-clear failed — DEL-6 defer",
				map[string]interface{}{"delegation_id": d.ID, "error": err.Error()})
			res.Deferred++
			if jctx.Metrics != nil {
				jctx.Metrics.RecordExpiryDeferred()
				jctx.Metrics.RecordUPAvailabilityFailure("expiry-cron")
			}
			continue
		}
		if raced, err := endExpired(ctx, jctx, d); err != nil {
			jctx.Logger.Warn("delegation-expiry: local end failed",
				map[string]interface{}{"delegation_id": d.ID, "error": err.Error()})
			res.Failed++
		} else if !raced {
			res.Succeeded++
			if jctx.Metrics != nil {
				jctx.Metrics.RecordEnded(string(domain.EndReasonExpired))
			}
		}
		// raced (someone else already ended/cancelled it this tick) counts
		// toward neither succeeded nor failed — the desired end-state
		// already holds.
	}

	jctx.Logger.Info("delegation-expiry complete", map[string]interface{}{
		"attempted": res.Attempted, "succeeded": res.Succeeded,
		"failed": res.Failed, "deferred": res.Deferred,
	})
	return res, nil
}

// endExpired flips one delegation to ended and enqueues DelegationEnded
// {expired} inside a single tenant-GUC-bound transaction (RLS-6). raced=true
// means another actor (admin cancel, a concurrent tick) already ended it —
// not a failure.
func endExpired(ctx context.Context, jctx *Context, d domain.Delegation) (raced bool, err error) {
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
				EndedReason: domain.EndReasonExpired,
				ActorID:     domain.SystemActorID,
			}, "delegation-expiry")
	})
	return raced, err
}

// enqueueEvent mirrors internal/core/service's tiny enqueue helper (not
// exported there) — cron-origin events additionally carry the LLD §10.5
// note (1) system sentinels instead of a requestctx-derived IP/user-agent.
func enqueueEvent(ctx context.Context, eventType string, tenantID uuid.UUID, subject string, data any, cronName string) error {
	pub, ok := port.EventPublisherFromContext(ctx)
	if !ok || pub == nil {
		return nil
	}
	return pub.EnqueueCtx(ctx, &domain.DomainEvent{
		Type:       eventType,
		TenantID:   tenantID,
		Subject:    subject,
		Actor:      systemUserID,
		OccurredAt: time.Now().UTC(),
		IPAddress:  "system",
		UserAgent:  "iam-delegation/" + cronName + "-cron",
		Data:       data,
	})
}
