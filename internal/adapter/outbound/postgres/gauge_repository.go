package postgres

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/jackc/pgx/v5"
)

// GaugeRepository serves the read-only, cross-tenant aggregate query that
// backs iam_delegation_active_gauge{tenant} (LLD §14.2). That gauge is a
// count of current table state rather than an event tally, so nothing on a
// request or reconciler path can maintain it; cmd/server polls this
// repository on an interval instead.
//
// It MUST be constructed with the BYPASSRLS sysPool (LLD §7.2.3) — the
// query spans tenants and would return zero rows through the RLS-scoped
// app pool with no tenant GUC bound.
//
// Unlike DelegationRepository / SettingsRepository it implements no core
// port: its only consumer is the composition root's exporter goroutine
// (iam-realm-provisioner's GaugeRepository convention).
type GaugeRepository struct {
	pool *pgcommon.Pool
}

// NewGaugeRepository constructs a GaugeRepository over the BYPASSRLS pool.
func NewGaugeRepository(sysPool *pgcommon.Pool) *GaugeRepository {
	return &GaugeRepository{pool: sysPool}
}

// countActiveByTenantSQL counts currently-active, non-deleted delegations
// per tenant. scheduled / ended / cancelled / soft-deleted rows are
// excluded so the gauge matches idx_delegations_tenant (status='active'
// AND deleted_at IS NULL). Tenants with zero matching rows are absent
// from the result; the exporter Reset()s the GaugeVec before publishing
// so those labels disappear instead of lingering at a stale count.
const countActiveByTenantSQL = `
SELECT tenant_id::text, count(*)
  FROM public.delegations
 WHERE status = 'active'
   AND deleted_at IS NULL
 GROUP BY tenant_id`

// CountActiveByTenant returns the count of active, non-deleted
// delegations keyed by tenant_id. Tenants with none are absent from the
// map rather than present as 0.
func (r *GaugeRepository) CountActiveByTenant(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, countActiveByTenantSQL)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				tenant string
				count  int64
			)
			if err := rows.Scan(&tenant, &count); err != nil {
				return err
			}
			out[tenant] = count
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
