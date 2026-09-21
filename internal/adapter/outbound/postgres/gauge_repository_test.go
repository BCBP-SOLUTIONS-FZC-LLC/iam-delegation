package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGaugeRepository_CountActiveByTenant_CountsActiveOnly(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	gauges := NewGaugeRepository(db.Bypass)

	tenantA := uuid.New()
	tenantB := uuid.New()

	seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantA, Status: "active"})
	seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantA, Status: "active"})
	seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantA, Status: "scheduled"})
	seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantA, Status: "ended"})
	seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantA, Status: "cancelled"})
	deletedAt := time.Now().UTC()
	seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantA, Status: "active", DeletedAt: &deletedAt})
	seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantB, Status: "active"})

	counts, err := gauges.CountActiveByTenant(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(2), counts[tenantA.String()], "scheduled/ended/cancelled/soft-deleted must not count")
	assert.Equal(t, int64(1), counts[tenantB.String()])
	assert.Len(t, counts, 2, "tenants with zero active rows must be absent, not present as 0")
}

func TestGaugeRepository_CountActiveByTenant_Empty(t *testing.T) {
	db := setupTestDB(t)
	gauges := NewGaugeRepository(db.Bypass)

	counts, err := gauges.CountActiveByTenant(context.Background())
	require.NoError(t, err)
	assert.Empty(t, counts)
}

func TestGaugeRepository_CountActiveByTenant_AppPoolWithoutGUCSeesNothing(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: uuid.New(), Status: "active"})

	// RLS-scoped app pool with no tenant GUC bound — the reason this
	// repository must sit on sysPool. Zero rows is fail-closed, not a
	// cross-tenant leak.
	appGauges := NewGaugeRepository(db.App)
	counts, err := appGauges.CountActiveByTenant(ctx)
	require.NoError(t, err)
	assert.Empty(t, counts)
}

// TestGaugeRepository_CountActiveByTenant_CancelledContext covers the
// WithConn query-error return: a pre-cancelled context causes the acquire
// or query to fail immediately.
func TestGaugeRepository_CountActiveByTenant_CancelledContext(t *testing.T) {
	db := setupTestDB(t)
	gauges := NewGaugeRepository(db.Bypass)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := gauges.CountActiveByTenant(ctx)
	require.Error(t, err)
}
