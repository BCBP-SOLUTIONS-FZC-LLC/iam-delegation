package service

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
	"github.com/google/uuid"
)

// SettingsService owns DLG-6/7 — the per-tenant delegation policy relocated
// from Core's tenants row (LLD §7.2.2, DLG-D2).
type SettingsService struct {
	settings port.SettingsRepository
}

// NewSettingsService builds a SettingsService.
func NewSettingsService(settings port.SettingsRepository) *SettingsService {
	return &SettingsService{settings: settings}
}

// Get is DLG-6 — any tenant member. Returns the 90/90 default when the
// tenant has no persisted row.
func (s *SettingsService) Get(ctx context.Context, tenantID uuid.UUID) (domain.DelegationTenantSettings, error) {
	return s.settings.Get(ctx, tenantID)
}

// Set is DLG-7 — tenant_admin/owner only (enforced at the http layer).
// Both fields must be in [1,180] (LLD §8.4).
func (s *SettingsService) Set(ctx context.Context, tenantID uuid.UUID, maxDurationDays, reviewWindowDays int) (domain.DelegationTenantSettings, error) {
	if maxDurationDays < domain.PolicyDaysMin || maxDurationDays > domain.PolicyDaysMax {
		return domain.DelegationTenantSettings{}, domain.NewError(domain.ErrInvalidDelegationMaxDurationDays,
			"max_duration_days must be between 1 and 180")
	}
	if reviewWindowDays < domain.PolicyDaysMin || reviewWindowDays > domain.PolicyDaysMax {
		return domain.DelegationTenantSettings{}, domain.NewError(domain.ErrInvalidDelegationReviewWindow,
			"review_window_days must be between 1 and 180")
	}
	return s.settings.Upsert(ctx, tenantID, maxDurationDays, reviewWindowDays)
}
