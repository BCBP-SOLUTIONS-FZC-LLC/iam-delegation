package postgres

import (
	"context"
	"errors"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SettingsRepository implements port.SettingsRepository against
// delegation_tenant_settings (LLD §7.2.2, DLG-D2).
type SettingsRepository struct {
	pool *pgcommon.Pool
}

var _ port.SettingsRepository = (*SettingsRepository)(nil)

// NewSettingsRepository builds a SettingsRepository over pool.
func NewSettingsRepository(pool *pgcommon.Pool) *SettingsRepository {
	return &SettingsRepository{pool: pool}
}

const settingsCols = `tenant_id, max_duration_days, review_window_days, record_version, created_at, updated_at`

func scanSettings(row pgx.Row) (domain.DelegationTenantSettings, error) {
	var s domain.DelegationTenantSettings
	err := row.Scan(&s.TenantID, &s.MaxDurationDays, &s.ReviewWindowDays, &s.RecordVersion, &s.CreatedAt, &s.UpdatedAt)
	return s, err
}

// Get returns the tenant's policy row, or the 90/90 default (never an
// error) when no row exists yet.
func (r *SettingsRepository) Get(ctx context.Context, tenantID uuid.UUID) (domain.DelegationTenantSettings, error) {
	var out domain.DelegationTenantSettings
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+settingsCols+` FROM delegation_tenant_settings WHERE tenant_id = $1`, tenantID)
		s, err := scanSettings(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				out = domain.DefaultDelegationTenantSettings(tenantID)
				return nil
			}
			return err
		}
		out = s
		return nil
	})
	return out, err
}

// Upsert creates or updates the tenant's policy row (DLG-7). The touch_row
// trigger manages record_version/updated_at on the ON CONFLICT DO UPDATE
// path (BEFORE UPDATE triggers fire for the update arm of an
// INSERT ... ON CONFLICT DO UPDATE); the INSERT path defaults
// record_version=1 via the column default.
func (r *SettingsRepository) Upsert(ctx context.Context, tenantID uuid.UUID, maxDurationDays, reviewWindowDays int) (domain.DelegationTenantSettings, error) {
	var out domain.DelegationTenantSettings
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO delegation_tenant_settings (tenant_id, max_duration_days, review_window_days)
			VALUES ($1, $2, $3)
			ON CONFLICT (tenant_id) DO UPDATE
				SET max_duration_days = $2, review_window_days = $3
			RETURNING `+settingsCols,
			tenantID, maxDurationDays, reviewWindowDays)
		s, err := scanSettings(row)
		if err != nil {
			return err
		}
		out = s
		return nil
	})
	return out, err
}

// SoftDeleteTenant removes the tenant's policy row on TenantOffboarded
// (LLD §11.6). delegation_tenant_settings has no deleted_at column — it is
// pure policy, not audit-relevant data — so "soft-delete" for this table
// means a real DELETE: there is nowhere to mark the row deleted, and a
// stale policy row for an offboarded tenant is meaningless anyway (a
// subsequent DLG-7 write would just recreate the 90/90 default lazily).
func (r *SettingsRepository) SoftDeleteTenant(ctx context.Context, tenantID uuid.UUID) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM delegation_tenant_settings WHERE tenant_id = $1`, tenantID)
		return err
	})
}
