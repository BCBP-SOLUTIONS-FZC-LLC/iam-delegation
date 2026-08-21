package service

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
	"github.com/google/uuid"
)

// CascadeService handles the two inbound signals from Core on
// delegation-cascade-q (LLD §10.1, §11.5/§11.6): MembershipRevoked (a
// user removed from a tenant) and TenantOffboarded (a whole tenant
// offboarded). Both are asynchronous — the stranding-hazard gate itself
// stays synchronous in Core (LLD §11.5, DLG-D9); this service only ends the
// now-inert rows.
type CascadeService struct {
	delegations port.DelegationRepository
	settings    port.SettingsRepository
	userProfile port.UserProfileClient
	txRunner    port.TxRunner
}

// NewCascadeService builds a CascadeService.
func NewCascadeService(d port.DelegationRepository, settings port.SettingsRepository, up port.UserProfileClient, txRunner port.TxRunner) *CascadeService {
	return &CascadeService{delegations: d, settings: settings, userProfile: up, txRunner: txRunner}
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
		//nolint:errcheck // best-effort: the row is already ended/inert
		// (§7.6.5) — a UP failure here is not retried by this cascade, only
		// logged upstream by the consumer; the delegation-expiry cron only
		// revisits still-active rows, so there's nothing to retry against.
		_ = s.userProfile.SetAvailability(ctx, port.SetAvailabilityRequest{
			TenantID: tenantID, UserID: d.DelegatorID, ClearDelegate: true,
		})
	}
	return nil
}

// ScrubTenant handles TenantOffboarded (LLD §11.6) — soft-deletes every
// delegation and the tenant's delegation_tenant_settings row. No event
// emission (stale rows are inert, §7.6.5); the monthly delegation-cleanup
// job hard-purges after the 90-day retention window (§18.4).
func (s *CascadeService) ScrubTenant(ctx context.Context, tenantID uuid.UUID) error {
	if err := s.delegations.SoftDeleteTenant(ctx, tenantID); err != nil {
		return err
	}
	return s.settings.SoftDeleteTenant(ctx, tenantID)
}
