# Database Schema

**Database:** `delegation` on shared RDS PostgreSQL Multi-AZ (LLD §7). Connection,
pool sizing, PgBouncer mode, transactions, and health all go through
`platform-pgcommon` (DLG-D23/D32, matching `iam-realm-provisioner`):
`pgcommon.ConfigFromEnv` / `pgcommon.NewPool` / `pgcommon.RunInTxWithRetryOpts` /
`pgcommon.Pool.Health` / `DrainAndClose`. `PG_BOUNCER_MODE` is a standard
`ConfigFromEnv` var (app pool); `SystemPoolConfig` forces `PGBouncerMode: true`
unconditionally so the BYPASSRLS pool still works under transaction pooling.

**Pool configuration** (`cmd/server/main.go` and `cmd/reconciler/main.go`):
```go
pgCfg, warnings := pgcommon.ConfigFromEnv()   // DATABASE_URL / PG_MAX_CONNS / PG_MIN_CONNS / PG_SLOW_QUERY_THRESHOLD / PG_SSLMODE / PG_BOUNCER_MODE
pgCfg.DSN = postgres.DSNFromEnv()             // pgcommon-built DSN + PG_STATEMENT_TIMEOUT
pgCfg.GUCProvider = pgcommon.GUCSetFromContext
pgCfg.Tracer = postgres.NewOTelTracer(serviceName)
pool, err := pgcommon.NewPool(ctx, pgCfg)
```
Defaults (`deploy/helm/iam-delegation/values.yaml`): `PG_MAX_CONNS=10`, `PG_MIN_CONNS=2`,
`PG_SLOW_QUERY_THRESHOLD=200ms`.

**Core tables** (exactly three — LLD §7.2, ER diagram in `docs/architecture/mermaid/data-model.mmd`):

| Table | Purpose | Tenant-scoped | RLS |
|-------|---------|----------------|-----|
| `delegations` | Delegation grants: who → whom, scope, window, status, review tracking | Yes | Yes (FORCE) |
| `delegation_tenant_settings` | Per-tenant policy (`max_duration_days`, `review_window_days`), one row per tenant, 90/90 default when absent | Yes | Yes (FORCE) |
| `processed_events` | Cascade-consumer idempotency ledger, `(event_id, consumer)` composite PK | No | No (operational, RLS-exempt) |

Plus `rls_violation_log` — not a business table, RLS-disabled audit sink (see RLS section below).

**`delegations` columns of interest** (LLD §7.2.1):
- `delegator_membership_id` / `delegate_membership_id` — logical refs to Core's `tenant_memberships(id)`, no local FK (the composite-FK loss this service exists to replace, DLG-D3 — see `flows-and-concurrency.md`).
- `ends_at IS NULL` = open-ended → tracked by `review_due_at`/`review_last_warned_bucket`; `ends_at` set = fixed-window, never review-tracked.
- `review_last_warned_bucket int` — `7`, `3`, or `NULL`; which review notice fired this cycle (`CHECK IN (7,3)`); reset to `NULL` on extend/reassign to re-arm both warnings.
- `record_version bigint` — optimistic lock, `CHECK (record_version > 0)`, maintained by `touch_row()` trigger (not application code).
- `CHECK` constraints enforce structurally what the service layer also validates: `chk_scope_id` (scope=all ⟺ scope_id NULL), `chk_no_self_delegate`, `chk_ends_after_starts`, `chk_review_window_days BETWEEN 1 AND 180`.
- Five partial indexes, all `WHERE deleted_at IS NULL AND status = 'active'` (plus `idx_delegations_review_due` additionally scoped to `ends_at IS NULL`) — every hot-path query is served by one of these.

**`delegation_tenant_settings`** — PK is `tenant_id` itself (one row per tenant); `CHECK` both `max_duration_days`/`review_window_days BETWEEN 1 AND 180`.

**`processed_events`** — PK `(event_id, consumer)`; `consumer` distinguishes `cascade` from a future second inbound subscription if one is ever added. Monthly-pruned (LLD §18.4, `delegation-cleanup` CronJob).

## Row-Level Security (LLD §7.3)

Three-function fail-closed design, explicitly mirroring `iam-user-profile`'s pattern (migration comment: "mirroring iam-user-profile's app_tenant_id()/rls_check_tenant()/log_rls_violation()/rls_violation_log pattern, NOT iam-tender-acl's simpler plain-policy pattern"):

- **`app_tenant_id()`** — `STABLE SECURITY DEFINER`, reads `current_setting('app.tenant_id', true)`, returns `NULL` on any parse error (fail-closed, not fail-open).
- **`rls_check_tenant(tenant_id, table_name)`** — `STABLE STRICT SECURITY DEFINER`, used in policy `USING` clauses. Returns `false` (blocking the read) and logs one of two violation types via `log_rls_violation`: `missing_or_invalid_guc` (no GUC set) or `cross_tenant_access` (GUC present, wrong tenant).
- **`log_rls_violation(...)`** — `SECURITY DEFINER`, 1%-sampled INSERT into `rls_violation_log`, swallows its own errors (a logging failure must never abort the caller's transaction).

Policies: `USING (rls_check_tenant(tenant_id, '<table>'))` (reads — gated + logged) + `WITH CHECK (tenant_id = app_tenant_id())` (writes — a row can never be written into another tenant, no logging needed since the write itself fails). Both `delegations` and `delegation_tenant_settings` have `ENABLE` + `FORCE ROW LEVEL SECURITY` — even the table owner can't bypass without an explicit `BYPASSRLS` role. `rls_violation_log` itself has RLS **disabled** (`DISABLE ROW LEVEL SECURITY` + `NO FORCE`) so `log_rls_violation` (invoked from inside an RLS check) cannot recurse into itself.

**Three GUC-binding paths converge on the same RLS** — richer than a single gateway-bridge path, per DLG-D18. Full detail (with diagram) in `ARCHITECTURE.md § Row-Level Security (RLS) and GUC injection` / `docs/architecture/mermaid/rls-guc-flow.mmd`:
1. Public routes — `tenantGUCMiddleware`, once per request.
2. Mesh-only internal reads (DLG-I3/I4) — `gucBoundReader`, per single-statement call.
3. Reconciler jobs + cascade consumer — injected `BindTenantGUC`/`GUCBinder` function params, per row before each `RunInTx`.

All three call `postgres.WithTenantGUC(ctx, tenantID, userID)` → `pgcommon.GUCSet` on `ctx` → next pool checkout issues `SET LOCAL app.tenant_id = '...'` (transaction-scoped only — CI greps for the forbidden session-scoped form via `.github/scripts/check-forbidden-set-guc.sh`, RLS-6).

## BYPASSRLS pool (`sysPool`)

Both `cmd/server/main.go` and `cmd/reconciler/main.go` build a second pool from `SYSTEM_DATABASE_URL` (`postgres.SystemDSNFromEnv()` + `postgres.SystemPoolConfig()`, `internal/adapter/outbound/postgres/db.go`):
```go
sysDSN := pgadapter.SystemDSNFromEnv()
if sysDSN == dsn {
    logger.Warn("SYSTEM_DATABASE_URL not set — sysPool reuses app DSN; cross-tenant cron/internal sweeps will be RLS-filtered")
}
sysCfg := pgadapter.SystemPoolConfig(sysDSN, logger) // forces PGBouncerMode, no GUCProvider
sysCfg.Tracer = queryTracer
sysPool, err := pgcommon.NewPool(ctx, sysCfg)
```
Falls back to `DSNFromEnv()` in dev with a startup warning — cross-tenant reads then return zero rows under RLS (fail-quiet, not fail-loud). `cmd/server/config.go`'s `loadConfig()` fails fast instead of falling through to this warning whenever `ENVIRONMENT` isn't one of `isDevLikeEnvironment`'s recognized local/dev aliases (`development`/`dev`/`local`) — originally gated on a literal `"production"` compare (DLG-D34), widened to every other environment name in DLG-D35.

**What actually uses `sysPool` in this repo:**
- `cmd/server/main.go`: `delegationRepoSys := pgadapter.NewDelegationRepository(sysPool)` — backs the mesh-only DLG-I3/I4 internal reads (`gucBoundReader`, cross-tenant lookups by design).
- `cmd/server/main.go`: `processedEvents := pgadapter.NewProcessedEventsRepository(sysPool)` — the cascade consumer's idempotency ledger, RLS-exempt by table design but routed through the BYPASSRLS pool and `withPool` (joins an ambient TxRunner tx).
- `cmd/server/main.go`: `pgadapter.NewGaugeRepository(sysPool)` — 5-minute `iam_delegation_active_gauge{tenant}` snapshot exporter.
- `cmd/reconciler/main.go`: `pgadapter.NewDelegationRepository(sysPool)` — backs all four CronJobs' cross-tenant sweep queries (expiry, activation, review, cleanup — LLD §11.1a/§11.3/§11.4/§18.4).
- `cmd/reconciler/main.go`: `pgadapter.NewProcessedEventsRepository(sysPool)` — monthly `processed_events` prune (LLD §18.4).

## PostgreSQL roles (LLD §7.3/§7.4)

- **`delegation_app`** — runtime role, `NOBYPASSRLS` explicitly (RLS-4). Dev password is checked into the migration (`delegation_app_dev_password`) — production rotates this out of band, never by editing the migration.
- **`delegation_migrator`** — DDL + BYPASSRLS, backs `MIGRATION_DATABASE_URL` and (in practice) `SYSTEM_DATABASE_URL` too.
- **`admin_readonly`** — the LLD (§7.3 prose) describes this as an *optional* future compliance-reader role, `SELECT`-only, still RLS-bound. **Not implemented** — grep of the one migration file confirms no such role is created. Don't assume it exists; if compliance tooling needs it, it requires a new migration.

## Migrations

**Startup order (DLG-D32, matching `iam-realm-provisioner` / `postgres.Migrate`):** `outbox.ApplySchema` first (creates `outbox_events`), then the domain migration (which GRANTs `delegation_app` on that table). The reverse order — domain then outbox, which `iam-user-profile` uses — fails every fresh-database bring-up here.

This service has never been deployed (no Git tag has ever been pushed — see `../VERSIONING.md`), so there is no rolling-upgrade history across incremental migration files worth preserving — one file (`000001_schema`) is the whole schema, verified round-trip (`up` then `down`) against a real Postgres container. Every schema fix so far (the `scheduled` status/`idx_delegations_starts_at` for DLG-D25, the `delegate_disabled` end reason, etc.) has been folded back into that single file rather than layered as a new migration. Once a real deployment exists, future schema changes should go back to the normal forward-only, additive-where-possible discipline (column drops split across two releases to stay compatible with rolling deploys) — see `../VERSIONING.md`.

**Trigger:** `touch_row()` — identical pattern to user-profile's: maintains `updated_at` and increments `record_version` on every genuine `UPDATE`, guarded by `WHEN (OLD.* IS DISTINCT FROM NEW.*)` from day one (not retrofitted in a later migration the way user-profile's was) so a no-op `UPDATE` never bumps the version. Attached to both `delegations` and `delegation_tenant_settings` (`trg_touch_delegations`, `trg_touch_delegation_tenant_settings`).
