package service

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
	"github.com/google/uuid"
)

// CascadeService handles the inbound signals landing on
// delegation-cascade-q (LLD §10.1, §11.5/§11.6): MembershipRevoked (a
// user removed from a tenant) and TenantMembershipsPurged (a whole tenant
// offboarded) from Core, plus User Profile's UserUpdated{status: disabled}
// (Bug 2 — a second SNS subscription onto the same queue). All three are
// asynchronous — the stranding-hazard gate for membership removal stays
// synchronous in Core (LLD §11.5, DLG-D9); this service only ends the
// now-inert/no-longer-servable rows.
type CascadeService struct {
	delegations port.DelegationRepository
	settings    port.SettingsRepository
	userProfile port.UserProfileClient
	txRunner    port.TxRunner
	metrics     Metrics
}

// NewCascadeService builds a CascadeService.
func NewCascadeService(d port.DelegationRepository, settings port.SettingsRepository, up port.UserProfileClient, txRunner port.TxRunner) *CascadeService {
	return &CascadeService{delegations: d, settings: settings, userProfile: up, txRunner: txRunner}
}

// WithMetrics attaches an optional recorder. Nil is valid (no-op).
func (s *CascadeService) WithMetrics(m Metrics) *CascadeService {
	s.metrics = m
	return s
}

// EndForUser handles MembershipRevoked (LLD §11.5). It ends every active
// delegation where userID is delegator or delegate, pointer-clears the UP
// availability for each affected delegator (DEL-6, best-effort — a UP
// failure here does not block the row-end since the row is already inert
// once the user has no membership, §7.6.5), and emits DelegationEnded only
// for delegate-side rows (DEL-7/DLG-EVT-4 — delegator-side is silent).
func (s *CascadeService) EndForUser(ctx context.Context, tenantID, userID uuid.UUID) error {
	var ended []domain.Delegation
	err := s.txRunner.RunInTx(ctx, func(txCtx context.Context) error {
		rows, err := s.delegations.EndForUser(txCtx, tenantID, userID)
		if err != nil {
			return err
		}
		ended = rows
		for _, d := range rows {
			if d.DelegateID != userID {
				// Delegator-side removal is silent (DEL-7/DLG-EVT-4).
				continue
			}
			if err := enqueue(txCtx, domain.EventDelegationEnded, tenantID, d.ID.String(), "iam-system",
				domain.DelegationEndedPayload{
					DelegationID: d.ID, TenantID: tenantID,
					DelegatorID: d.DelegatorID, DelegateID: d.DelegateID,
					Scope: d.Scope, ScopeID: d.ScopeID,
					EndedReason: domain.EndReasonDelegateRemoved,
					ActorID:     domain.SystemActorID,
				}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Pointer-clear per affected delegator, best-effort (the row is already
	// ended and inert — a UP failure here is logged by the caller, not
	// retried by this cascade; the delegation-expiry cron only revisits
	// still-active rows, so any consumer wrapping this call should log on
	// error rather than fail the whole cascade).
	for _, d := range ended {
		if d.DelegatorID == userID {
			continue // the removed user was the delegator — no pointer to clear for them
		}
		// (§7.6.5) — a UP failure here is not retried by this cascade, only
		// logged upstream by the consumer; the delegation-expiry cron only
		// revisits still-active rows, so there's nothing to retry against.
		if err := s.userProfile.ClearDelegatePointer(ctx, tenantID, d.DelegatorID); err != nil && s.metrics != nil {
			s.metrics.RecordUPAvailabilityFailure("cascade")
		}
	}
	if s.metrics != nil {
		for _, d := range ended {
			if d.DelegateID == userID {
				s.metrics.RecordEnded(string(domain.EndReasonDelegateRemoved))
			}
		}
	}
	return nil
}

// EndForDisabledDelegate handles User Profile's UserUpdated{status:
// disabled} (Bug 2). It ends every active/scheduled delegation where
// delegateID is the delegate and emits, per row in the same transaction:
// DelegationEnded{ended_reason: delegate_disabled} (distinct from
// EndForUser's delegate_removed — that fires on tenant-membership removal,
// a different signal; "disabled" is not "removed"), immediately followed
// by DelegationEscalationRequested (Bug 2a) — the generic "this grant
// ended" notice alone doesn't tell anyone the delegator may now have NO
// valid handler for their work while still OOO. DEL-7/DLG-EVT-4's
// delegator-side silence rule does not apply here: every row this query
// matches is, by definition, delegate-side.
//
// No User Profile pointer-clear call here (unlike EndForUser): the
// delegate pointer on the delegator's own user_availability row was
// already cleared by User Profile itself, atomically within the same
// transaction that flipped the delegate's status and published this very
// UserUpdated event (LLD §8.8.16 K1 in iam-user-profile) — by the time
// this consumer sees the event, User Profile's side is guaranteed done.
func (s *CascadeService) EndForDisabledDelegate(ctx context.Context, tenantID, delegateID uuid.UUID) error {
	var n int
	err := s.txRunner.RunInTx(ctx, func(txCtx context.Context) error {
		rows, err := s.delegations.EndForDisabledDelegate(txCtx, tenantID, delegateID)
		if err != nil {
			return err
		}
		n = len(rows)
		for _, d := range rows {
			if err := enqueue(txCtx, domain.EventDelegationEnded, tenantID, d.ID.String(), "iam-system",
				domain.DelegationEndedPayload{
					DelegationID: d.ID, TenantID: tenantID,
					DelegatorID: d.DelegatorID, DelegateID: d.DelegateID,
					Scope: d.Scope, ScopeID: d.ScopeID,
					EndedReason: domain.EndReasonDelegateDisabled,
					ActorID:     domain.SystemActorID,
				}); err != nil {
				return err
			}
			if err := enqueue(txCtx, domain.EventDelegationEscalationRequested, tenantID, d.ID.String(), "iam-system",
				domain.DelegationEscalationRequestedPayload{
					DelegationID: d.ID, TenantID: tenantID,
					DelegatorID: d.DelegatorID, DelegateID: d.DelegateID,
					Scope: d.Scope, ScopeID: d.ScopeID,
					Reason:  domain.EndReasonDelegateDisabled,
					ActorID: domain.SystemActorID,
				}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if s.metrics != nil {
		for i := 0; i < n; i++ {
			s.metrics.RecordEnded(string(domain.EndReasonDelegateDisabled))
		}
	}
	return nil
}

// ScrubTenant handles TenantMembershipsPurged (LLD §11.6) — soft-deletes every
// delegation and the tenant's delegation_tenant_settings row. No event
// emission (stale rows are inert, §7.6.5); the monthly delegation-cleanup
// job hard-purges after the 90-day retention window (§18.4).
func (s *CascadeService) ScrubTenant(ctx context.Context, tenantID uuid.UUID) error {
	return s.txRunner.RunInTx(ctx, func(txCtx context.Context) error {
		if err := s.delegations.SoftDeleteTenant(txCtx, tenantID); err != nil {
			return err
		}
		return s.settings.SoftDeleteTenant(txCtx, tenantID)
	})
}
