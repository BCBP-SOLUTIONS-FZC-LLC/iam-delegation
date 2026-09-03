package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// testAppPassword matches the dev-only password baked into
// migrations/000001_schema.up.sql for the delegation_app role.
const testAppPassword = "delegation_app_dev_password"

// testDB bundles the two pools every integration test needs: App (bound to
// the RLS-enforced delegation_app role, exactly as production code sees it)
// and Raw (the postgres superuser, used only to seed/inspect rows out of
// band — bypasses RLS naturally, like a real BYPASSRLS admin connection).
type testDB struct {
	App *pgcommon.Pool
	Raw *pgxpool.Pool
	// AppDSN is the delegation_app connection string for this container —
	// exposed so RLS Case 5 can build its own MaxConns=1 pool pinned to a
	// single backend, distinct from App's default multi-conn pool.
	AppDSN string
	// Bypass is a pgcommon.Pool authenticated as the postgres superuser —
	// RLS is bypassed for any superuser regardless of policy, matching how
	// the delegation_migrator role (also BYPASSRLS) sees rows in production.
	// Used only for the cross-tenant sweep queries (FindDueForDailyWarn,
	// FindDueForAutoEnd, HardPurgeSoftDeletedBefore) that have no tenant_id
	// predicate at all and therefore return zero rows under a tenant-scoped
	// GUC no matter which tenant it names.
	Bypass *pgcommon.Pool
}

// setupTestDB spins up a fresh postgres:16-alpine container, applies every
// embedded migration (which also creates the delegation_app/delegation_migrator
// roles) via the same Migrate entry point cmd/server calls, and returns pools
// bound to both roles. Every test gets its own container so tests never leak
// state into each other.
func setupTestDB(t *testing.T) *testDB {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping postgres integration test in short mode")
	}
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		Image:        "postgres:16-alpine",
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_DB":       "delegation",
			"POSTGRES_USER":     "postgres",
			"POSTGRES_PASSWORD": "postgres",
		},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).
			WithStartupTimeout(60 * time.Second),
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	host, err := container.Host(ctx)
	require.NoError(t, err)
	port, err := container.MappedPort(ctx, "5432")
	require.NoError(t, err)

	superDSN := fmt.Sprintf("postgres://postgres:postgres@%s:%s/delegation?sslmode=disable", host, port.Port())

	// Applies the platform-events outbox schema and then 000001_schema
	// (enums, tables, indexes, RLS, roles) — the same entry point cmd/server
	// calls, so the test fixture can never drift from what actually ships.
	require.NoError(t, Migrate(ctx, superDSN))

	rawPool, err := pgxpool.New(ctx, superDSN)
	require.NoError(t, err)
	t.Cleanup(rawPool.Close)

	appDSN := fmt.Sprintf("postgres://delegation_app:%s@%s:%s/delegation?sslmode=disable", testAppPassword, host, port.Port())
	appPool, err := pgcommon.NewPool(ctx, pgcommon.Config{
		DSN:         appDSN,
		GUCProvider: pgcommon.GUCSetFromContext,
	})
	require.NoError(t, err)
	t.Cleanup(appPool.Close)

	bypassPool, err := pgcommon.NewPool(ctx, pgcommon.Config{DSN: superDSN})
	require.NoError(t, err)
	t.Cleanup(bypassPool.Close)

	return &testDB{App: appPool, Raw: rawPool, AppDSN: appDSN, Bypass: bypassPool}
}

// withTenant returns a context carrying a pgcommon GUCSet so the app pool's
// GUCProvider emits `SET LOCAL app.tenant_id = <uuid>` on every checkout —
// exactly what WithTenantGUC does, reimplemented here so RLS tests can bind
// arbitrary/malformed values without going through the production helper.
func withTenant(ctx context.Context, tenantID uuid.UUID) context.Context {
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.TenantID = tenantID.String()
	return pgcommon.WithGUCSet(ctx, g)
}

// seedDelegationOpts are the fields a test cares about; everything else gets
// a sane default. Seeding goes through the Raw (superuser) pool so it always
// succeeds regardless of the RLS policy under test.
type seedDelegationOpts struct {
	ID                     uuid.UUID
	TenantID               uuid.UUID
	DelegatorID            uuid.UUID
	DelegateID             uuid.UUID
	Scope                  string
	ScopeID                *uuid.UUID
	Status                 string
	StartsAt               *time.Time // DLG-D25: nil uses the column's DEFAULT now()
	EndsAt                 *time.Time
	ReviewDueAt            *time.Time
	ReviewLastWarnedBucket *int
	DeletedAt              *time.Time
}

func seedDelegation(t *testing.T, ctx context.Context, raw *pgxpool.Pool, o seedDelegationOpts) uuid.UUID {
	t.Helper()
	if o.ID == uuid.Nil {
		o.ID = uuid.New()
	}
	if o.DelegatorID == uuid.Nil {
		o.DelegatorID = uuid.New()
	}
	if o.DelegateID == uuid.Nil {
		o.DelegateID = uuid.New()
	}
	if o.Scope == "" {
		o.Scope = "all"
	}
	if o.Status == "" {
		o.Status = "active"
	}
	if o.StartsAt != nil {
		_, err := raw.Exec(ctx, `
			INSERT INTO delegations (id, tenant_id, delegator_id, delegate_id,
				delegator_membership_id, delegate_membership_id, scope, scope_id,
				status, starts_at, ends_at, review_due_at, review_last_warned_bucket, deleted_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			o.ID, o.TenantID, o.DelegatorID, o.DelegateID,
			uuid.New(), uuid.New(), o.Scope, o.ScopeID,
			o.Status, *o.StartsAt, o.EndsAt, o.ReviewDueAt, o.ReviewLastWarnedBucket, o.DeletedAt)
		require.NoError(t, err)
		return o.ID
	}
	_, err := raw.Exec(ctx, `
		INSERT INTO delegations (id, tenant_id, delegator_id, delegate_id,
			delegator_membership_id, delegate_membership_id, scope, scope_id,
			status, ends_at, review_due_at, review_last_warned_bucket, deleted_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		o.ID, o.TenantID, o.DelegatorID, o.DelegateID,
		uuid.New(), uuid.New(), o.Scope, o.ScopeID,
		o.Status, o.EndsAt, o.ReviewDueAt, o.ReviewLastWarnedBucket, o.DeletedAt)
	require.NoError(t, err)
	return o.ID
}

func seedSettings(t *testing.T, ctx context.Context, raw *pgxpool.Pool, tenantID uuid.UUID, maxDays, reviewDays int) {
	t.Helper()
	_, err := raw.Exec(ctx, `
		INSERT INTO delegation_tenant_settings (tenant_id, max_duration_days, review_window_days)
		VALUES ($1, $2, $3)`, tenantID, maxDays, reviewDays)
	require.NoError(t, err)
}

func timePtr(t time.Time) *time.Time { return &t }
func intPtr(n int) *int              { return &n }
