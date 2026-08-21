package port

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/google/uuid"
)

// SettingsRepository owns delegation_tenant_settings (LLD §7.2.2, DLG-D2).
type SettingsRepository interface {
	// Get returns the tenant's policy row, or the 90/90 default (never an
	// error) when no row exists yet.
	Get(ctx context.Context, tenantID uuid.UUID) (domain.DelegationTenantSettings, error)

	// Upsert creates or updates the tenant's policy row (DLG-7).
	Upsert(ctx context.Context, tenantID uuid.UUID, maxDurationDays, reviewWindowDays int) (domain.DelegationTenantSettings, error)

	// SoftDeleteTenant removes the tenant's policy row on TenantOffboarded
	// (§11.6).
	SoftDeleteTenant(ctx context.Context, tenantID uuid.UUID) error
}
