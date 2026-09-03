package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The canonical RLS matrix, LLD §17.5, run against both tenant-scoped
// tables (delegations, delegation_tenant_settings).

// ── Case 1: missing GUC → 0 rows ────────────────────────────────────────────

func TestRLS_Case1_MissingGUC_ZeroRows_Delegations(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	tenantA := uuid.New()
	seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantA, Status: "active"})

	var count int
	err := pgcommon.RunInTx(ctx, db.App, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM delegations`).Scan(&count)
	})
	require.NoError(t, err)
	assert.Equal(t, 0, count, "RLS-2: missing GUC must return 0 rows")
}

func TestRLS_Case1_MissingGUC_ZeroRows_Settings(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	tenantA := uuid.New()
	seedSettings(t, ctx, db.Raw, tenantA, 60, 60)

	var count int
	err := pgcommon.RunInTx(ctx, db.App, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM delegation_tenant_settings`).Scan(&count)
	})
	require.NoError(t, err)
	assert.Equal(t, 0, count, "RLS-2: missing GUC must return 0 rows")
}

// ── Case 2: cross-tenant write rejected ─────────────────────────────────────

func TestRLS_Case2_CrossTenantInsertRejected_Delegations(t *testing.T) {
	db := setupTestDB(t)
	tenantA := uuid.New()
	tenantB := uuid.New()
	ctxA := withTenant(context.Background(), tenantA)

	err := pgcommon.RunInTx(ctxA, db.App, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO delegations (tenant_id, delegator_id, delegate_id, delegator_membership_id, delegate_membership_id, scope)
			VALUES ($1, gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'all')`,
			tenantB)
		return err
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "row-level security")
}

func TestRLS_Case2_CrossTenantInsertRejected_Settings(t *testing.T) {
	db := setupTestDB(t)
	tenantA := uuid.New()
	tenantB := uuid.New()
	ctxA := withTenant(context.Background(), tenantA)

	err := pgcommon.RunInTx(ctxA, db.App, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO delegation_tenant_settings (tenant_id) VALUES ($1)`, tenantB)
		return err
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "row-level security")
}

// ── Case 3: cross-tenant read isolation ─────────────────────────────────────

func TestRLS_Case3_CrossTenantReadIsolation_Delegations(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	tenantA := uuid.New()
	tenantB := uuid.New()
	seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantB, Status: "active"})

	ctxA := withTenant(ctx, tenantA)
	var count int
	err := pgcommon.RunInTx(ctxA, db.App, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM delegations WHERE tenant_id = $1`, tenantB).Scan(&count)
	})
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

func TestRLS_Case3_CrossTenantReadIsolation_Settings(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	tenantA := uuid.New()
	tenantB := uuid.New()
	seedSettings(t, ctx, db.Raw, tenantB, 60, 60)

	ctxA := withTenant(ctx, tenantA)
	var count int
	err := pgcommon.RunInTx(ctxA, db.App, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM delegation_tenant_settings WHERE tenant_id = $1`, tenantB).Scan(&count)
	})
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

// ── Case 4: cross-tenant soft-delete / write rejected (0 rows affected) ────

func TestRLS_Case4_CrossTenantSoftDeleteRejected_Delegations(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	tenantA := uuid.New()
	tenantB := uuid.New()
	id := seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantB, Status: "active"})

	ctxA := withTenant(ctx, tenantA)
	var rowsAffected int64
	err := pgcommon.RunInTx(ctxA, db.App, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		cmd, err := tx.Exec(ctx, `UPDATE delegations SET deleted_at = now() WHERE tenant_id = $1`, tenantB)
		if err != nil {
			return err
		}
		rowsAffected = cmd.RowsAffected()
		return nil
	})
	require.NoError(t, err)
	assert.Zero(t, rowsAffected, "RLS: cross-tenant UPDATE must affect 0 rows")

	var deletedAt *time.Time
	require.NoError(t, db.Raw.QueryRow(ctx, `SELECT deleted_at FROM delegations WHERE id = $1`, id).Scan(&deletedAt))
	assert.Nil(t, deletedAt, "tenant B's row must be untouched")
}

func TestRLS_Case4_CrossTenantUpdateRejected_Settings(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	tenantA := uuid.New()
	tenantB := uuid.New()
	seedSettings(t, ctx, db.Raw, tenantB, 60, 60)

	ctxA := withTenant(ctx, tenantA)
	var rowsAffected int64
	err := pgcommon.RunInTx(ctxA, db.App, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		cmd, err := tx.Exec(ctx, `UPDATE delegation_tenant_settings SET max_duration_days = 1 WHERE tenant_id = $1`, tenantB)
		if err != nil {
			return err
		}
		rowsAffected = cmd.RowsAffected()
		return nil
	})
	require.NoError(t, err)
	assert.Zero(t, rowsAffected)

	var maxDays int
	require.NoError(t, db.Raw.QueryRow(ctx, `SELECT max_duration_days FROM delegation_tenant_settings WHERE tenant_id = $1`, tenantB).Scan(&maxDays))
	assert.Equal(t, 60, maxDays, "tenant B's row must be untouched")
}

// ── Case 5: no GUC leakage across a pooled connection (RLS-6) ───────────────
// Pin MaxConns=1 so tenant A and tenant B's transactions are forced onto the
// same backend connection.

// newSingleConnAppPool builds a MaxConns=1 delegation_app pool against the
// same container setupTestDB already started, forcing tenant A's and tenant
// B's transactions onto the same backend connection (RLS-6).
func newSingleConnAppPool(t *testing.T, db *testDB) *pgcommon.Pool {
	t.Helper()
	pool, err := pgcommon.NewPool(context.Background(), pgcommon.Config{
		DSN:         db.AppDSN,
		MaxConns:    1,
		MinConns:    1,
		GUCProvider: pgcommon.GUCSetFromContext,
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func TestRLS_Case5_NoGUCLeakageAcrossPooledConnection_Delegations(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	singleConnPool := newSingleConnAppPool(t, db)

	tenantA := uuid.New()
	tenantB := uuid.New()

	// Tenant A inserts a row under SET LOCAL app.tenant_id = A.
	ctxA := withTenant(ctx, tenantA)
	var idA uuid.UUID
	err := pgcommon.RunInTx(ctxA, singleConnPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO delegations (tenant_id, delegator_id, delegate_id, delegator_membership_id, delegate_membership_id, scope)
			VALUES ($1, gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'all')
			RETURNING id`, tenantA).Scan(&idA)
	})
	require.NoError(t, err)

	// Tenant B, same pooled backend, sees only its own rows (none yet).
	ctxB := withTenant(ctx, tenantB)
	var countB int
	err = pgcommon.RunInTx(ctxB, singleConnPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM delegations WHERE tenant_id = $1`, tenantA).Scan(&countB)
	})
	require.NoError(t, err)
	assert.Equal(t, 0, countB, "tenant B must not see tenant A's row on the shared backend")

	// Same backend, no GUC bound at all -> 0 rows, not tenant A's rows.
	var countNoGUC int
	err = pgcommon.RunInTx(ctx, singleConnPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM delegations`).Scan(&countNoGUC)
	})
	require.NoError(t, err)
	assert.Equal(t, 0, countNoGUC, "no GUC bound must fail closed, never leak tenant A's row")

	// Tenant B cannot update tenant A's row by id.
	var rowsAffected int64
	err = pgcommon.RunInTx(ctxB, singleConnPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		cmd, err := tx.Exec(ctx, `UPDATE delegations SET deleted_at = now() WHERE id = $1`, idA)
		if err != nil {
			return err
		}
		rowsAffected = cmd.RowsAffected()
		return nil
	})
	require.NoError(t, err)
	assert.Zero(t, rowsAffected, "tenant B must not be able to update tenant A's row by id")
}

func TestRLS_Case5_NoGUCLeakageAcrossPooledConnection_Settings(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	singleConnPool := newSingleConnAppPool(t, db)

	tenantA := uuid.New()
	tenantB := uuid.New()

	ctxA := withTenant(ctx, tenantA)
	err := pgcommon.RunInTx(ctxA, singleConnPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO delegation_tenant_settings (tenant_id) VALUES ($1)`, tenantA)
		return err
	})
	require.NoError(t, err)

	ctxB := withTenant(ctx, tenantB)
	var countB int
	err = pgcommon.RunInTx(ctxB, singleConnPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM delegation_tenant_settings WHERE tenant_id = $1`, tenantA).Scan(&countB)
	})
	require.NoError(t, err)
	assert.Equal(t, 0, countB)

	var countNoGUC int
	err = pgcommon.RunInTx(ctx, singleConnPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM delegation_tenant_settings`).Scan(&countNoGUC)
	})
	require.NoError(t, err)
	assert.Equal(t, 0, countNoGUC)

	var rowsAffected int64
	err = pgcommon.RunInTx(ctxB, singleConnPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		cmd, err := tx.Exec(ctx, `UPDATE delegation_tenant_settings SET max_duration_days = 1 WHERE tenant_id = $1`, tenantA)
		if err != nil {
			return err
		}
		rowsAffected = cmd.RowsAffected()
		return nil
	})
	require.NoError(t, err)
	assert.Zero(t, rowsAffected)
}

// ── touch_row trigger on delegations ────────────────────────────────────────

func TestTouchRow_Delegations_RealUpdateBumpsVersion_NoOpDoesNot(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	tenantA := uuid.New()
	id := seedDelegation(t, ctx, db.Raw, seedDelegationOpts{TenantID: tenantA, Status: "active", Scope: "all"})
	ctxA := withTenant(ctx, tenantA)

	var before int64
	require.NoError(t, db.Raw.QueryRow(ctx, `SELECT record_version FROM delegations WHERE id = $1`, id).Scan(&before))
	require.Equal(t, int64(1), before)

	// No-op update: same value.
	err := pgcommon.RunInTx(ctxA, db.App, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE delegations SET status = 'active' WHERE id = $1`, id)
		return err
	})
	require.NoError(t, err)
	var afterNoOp int64
	require.NoError(t, db.Raw.QueryRow(ctx, `SELECT record_version FROM delegations WHERE id = $1`, id).Scan(&afterNoOp))
	assert.Equal(t, before, afterNoOp, "TRG-3: no-op UPDATE must not bump record_version")

	// Real change.
	err = pgcommon.RunInTx(ctxA, db.App, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE delegations SET status = 'cancelled' WHERE id = $1`, id)
		return err
	})
	require.NoError(t, err)
	var afterReal int64
	var updatedAt time.Time
	require.NoError(t, db.Raw.QueryRow(ctx, `SELECT record_version, updated_at FROM delegations WHERE id = $1`, id).Scan(&afterReal, &updatedAt))
	assert.Equal(t, before+1, afterReal, "a real change must bump record_version")
	assert.WithinDuration(t, time.Now().UTC(), updatedAt, 10*time.Second)
}

// ── RLS-4: delegation_app must never hold BYPASSRLS ─────────────────────────

func TestRLS_AppRoleHasNoBypassRLS(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	var bypass bool
	require.NoError(t, db.Raw.QueryRow(ctx, `SELECT rolbypassrls FROM pg_roles WHERE rolname = 'delegation_app'`).Scan(&bypass))
	assert.False(t, bypass, "delegation_app must NOT hold BYPASSRLS (RLS-4)")
}

func TestRLS_MigratorRoleHasBypassRLS(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	var bypass bool
	require.NoError(t, db.Raw.QueryRow(ctx, `SELECT rolbypassrls FROM pg_roles WHERE rolname = 'delegation_migrator'`).Scan(&bypass))
	assert.True(t, bypass, "delegation_migrator must hold BYPASSRLS")
}

func TestRLS_EnableAndForceOnBothTenantTables(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	got := map[string]bool{}
	err := pgcommon.RunInTx(ctx, db.Bypass, pgx.TxOptions{}, func(_ context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
		SELECT c.relname FROM pg_class c JOIN pg_namespace n ON c.relnamespace = n.oid
		WHERE n.nspname = 'public' AND c.relkind = 'r' AND c.relrowsecurity AND c.relforcerowsecurity`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			got[name] = true
		}
		return rows.Err()
	})
	require.NoError(t, err)
	assert.True(t, got["delegations"])
	assert.True(t, got["delegation_tenant_settings"])
	assert.False(t, got["rls_violation_log"], "rls_violation_log must have RLS disabled to avoid recursion")
	assert.False(t, got["processed_events"], "processed_events is exempt from RLS (operational)")
}

// ── processed_events dedup ───────────────────────────────────────────────

func TestProcessedEvents_CompositePKDedup(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	_, err := db.Raw.Exec(ctx, `INSERT INTO processed_events (event_id, consumer) VALUES ('evt-1', 'cascade')`)
	require.NoError(t, err)

	// Same (event_id, consumer) pair -> unique violation without ON CONFLICT.
	_, err = db.Raw.Exec(ctx, `INSERT INTO processed_events (event_id, consumer) VALUES ('evt-1', 'cascade')`)
	require.Error(t, err)
	assert.True(t, pgcommon.IsUniqueViolation(err))

	// ON CONFLICT DO NOTHING is the idempotent consumer pattern.
	cmd, err := db.Raw.Exec(ctx, `INSERT INTO processed_events (event_id, consumer) VALUES ('evt-1', 'cascade') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	assert.Zero(t, cmd.RowsAffected())

	// Same event_id, different consumer -> distinct row, allowed.
	_, err = db.Raw.Exec(ctx, `INSERT INTO processed_events (event_id, consumer) VALUES ('evt-1', 'offboarding')`)
	require.NoError(t, err)

	var count int
	require.NoError(t, db.Raw.QueryRow(ctx, `SELECT count(*) FROM processed_events WHERE event_id = 'evt-1'`).Scan(&count))
	assert.Equal(t, 2, count)
}
