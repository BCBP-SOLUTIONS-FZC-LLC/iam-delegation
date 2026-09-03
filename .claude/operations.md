# Security

## Tenant isolation

Three layers (LLD §13.1, summarized in `ARCHITECTURE.md` § Trust boundaries and isolation — full
detail there): Postgres RLS (`FORCE`, fail-closed), `SET LOCAL app.tenant_id` per transaction via
`platform-pgcommon` (never a session-scoped `SET` — CI greps the forbidden form,
`.github/scripts/check-forbidden-set-guc.sh`), and gateway-identity headers
(`x-tenant-id`/`x-user-id`/`x-tenant-roles`) — never a request-body-derived tenant/actor. Three
distinct GUC-binding paths exist for the three different call shapes (public HTTP, mesh-only
single-statement reads, cron/consumer jobs) — see `ARCHITECTURE.md` § Row-Level Security (RLS) and
GUC injection for the full diagram (`docs/architecture/mermaid/rls-guc-flow.mmd`).

## Network isolation (LLD §13.2)

Public routes behind Envoy; `/internal/*` (DLG-I1..I4) is mesh-only mTLS at the network layer — no
RBAC/JWT check, no `ContextMiddleware`, because there is no gateway identity to bridge on those
routes. Cross-service calls to `iam-org-membership`/`iam-user-profile` are intra-mesh mTLS.

## Input validation

Full field-level rules and error-code table live in `README.md` § Input validation (built this
session directly from `internal/core/domain/errors.go` and the service-layer validation functions) —
cross-reference rather than duplicate here.

# Observability

## Metrics — `internal/adapter/outbound/metrics/metrics.go`

**CLOSED for HTTP/cascade/cron paths (DLG-D30/D31, matching `iam-realm-provisioner` handler increments):**
`DelegationService.WithMetrics` / `CascadeService.WithMetrics` increment `created`/`ended`/
`idempotency_hits`/`up_availability_failures` after the matching outcome; the membership-check
client records duration + failures; the cascade consumer records processed/DLQ; the reconciler
jobs record deferred/warned/expired/ended. `iam_delegation_active_gauge{tenant}` is refreshed
every 5 minutes from a BYPASSRLS `COUNT(*) GROUP BY tenant_id` on `status='active' AND
deleted_at IS NULL` (`cmd/server/exporters.go`, matching `iam-realm-provisioner`'s DB-state
gauge exporters) — emit-once at start so the first scrape is populated.
`cmd/reconciler`'s CronJob binary still has no `/metrics` scrape endpoint — alert-visible
deferred counters reach Prometheus via `cmd/server`'s DLG-I1/I2 HTTP entry points.

| Metric | Type | Labels | Recorder method |
|---|---|---|---|
| `iam_delegation_created_total` | Counter | — | `RecordCreated(scope)` |
| `iam_delegation_ended_total` | Counter | — | `RecordEnded(reason)` |
| `iam_delegation_active_gauge` | Gauge | tenant | `ReplaceActiveGauges` (5-min sysPool snapshot) |
| `iam_delegation_expiry_deferred_total` | Counter | — | `RecordExpiryDeferred()` |
| `iam_delegation_review_deferred_total` | Counter | — | `RecordReviewDeferred()` |
| `iam_delegation_activation_deferred_total` | Counter | — | `RecordActivationDeferred()` |
| `iam_delegation_review_warned_total` | Counter | days_remaining | `RecordReviewWarned(daysRemaining)` |
| `iam_delegation_review_expired_total` | Counter | — | `RecordReviewExpired()` |
| `iam_delegation_membership_check_duration_seconds` | Histogram | — | `ObserveMembershipCheckDuration(seconds)` |
| `iam_delegation_membership_check_failures_total` | Counter | — | `RecordMembershipCheckFailure()` |
| `iam_delegation_up_availability_failures_total` | Counter | path | `RecordUPAvailabilityFailure(path)` |
| `iam_delegation_idempotency_hits_total` | Counter | — | `RecordIdempotencyHit()` |
| `iam_delegation_cascade_processed_total` | Counter | — | `RecordCascadeProcessed()` |
| `iam_delegation_cascade_dlq_total` | Counter | — | `RecordCascadeDLQ()` |

## Tracing and logging

OTel via `platform-gincommon` (same shared lib as sibling services); structured logging via Zap,
wired through `gincommon`. Both binaries call `gincommon.InitTracingFromEnv` then
`ObservabilityMiddlewares` at startup **before** creating tracers or registering collectors
(DLG-D31, matching `iam-realm-provisioner`), then share one `postgres.NewOTelTracer` across
the app and sys pools so `db.query` spans share the HTTP OTLP pipeline. Outbound User Profile /
Org Membership calls go through `internal/adapter/outbound/httpx` (`otelhttp`) so they emit
client spans and inject `traceparent`. The reconciler wraps each job in a
`reconciler.<job>` span. No custom TracerProvider/exporter. `/metrics` is served on
`METRICS_PORT` (default 9090), not on the API listener. `metrics.Register()` is the no-arg
gincommon API.

## Health checks — `internal/adapter/inbound/http/health.go`

- `GET /healthz` — `gincommon.HealthHandler()` verbatim (liveness only, no local logic).
- `GET /readyz` — Postgres (app pool) and sys Postgres (BYPASSRLS pool) are hard-blocking:
  - **Postgres** (`pgcommon.Pool.Health` on the app pool) — unhealthy → overall `503`. Also reports pool stats
    (total/idle/acquired/max conns, utilization) in the response body.
  - **Sys Postgres** (`pgcommon.Pool.Health` on sysPool) — unhealthy → overall `503` (matching `iam-realm-provisioner`).
  - **Valkey** (`redisPinger.Ping`, `cmd/server/adapters.go`) — degraded-only, never flips overall
    readiness.
  - **Outbox runner** (`outboxPinger.Ping`) — reports "not ready" until the runner's `Ready()`
    channel closes (first successful poll cycle); also degraded-only.

## Graceful shutdown

See `flows-and-concurrency.md` § Shutdown ordering for the exact sequence
(`httpServer.Shutdown` → `outboxRunner.Stop` → `sqsConsumer.Stop` → deferred `redisClient.Close`).

## Runbooks

`docs/runbook-schema-registry.md` — operator runbook for Glue registry drift, schema verification,
IAM permission failures, and the `schema-gov` CLI (written this session, DLG-D20).

# Configuration

**Env vars actually read by the Go binaries** (`cmd/server/config.go`, `pgcommon.ConfigFromEnv`,
`postgres.SystemDSNFromEnv`) — verified against current source, not copied from a sibling service:

| Variable | Required | Default | Notes |
|---|---|---|---|
| `ENVIRONMENT` | No | `development` | |
| `PORT` | No | `8080` | HTTP listen port — **not** `HTTP_PORT` (no dead config remains as of this pass, see below) |
| `DATABASE_URL` | Yes (via pgcommon) | — | App role (`delegation_app`, RLS-enforced, no BYPASSRLS) — or set `PG_HOST`/`PG_PORT`/`PG_USER`/`PG_PASSWORD`/`PG_DBNAME`/`PG_SSLMODE` individually; both forms go through `pgcommon.ConfigFromEnv` via `postgres.DSNFromEnv` |
| `MIGRATION_DATABASE_URL` | No | falls back to `postgres.DSNFromEnv()` | BYPASSRLS role for the server's self-migration at startup; must bypass PgBouncer |
| `SYSTEM_DATABASE_URL` | Helm: required. Go: required when `ENVIRONMENT=production` (both binaries fail fast at startup otherwise, DLG-D34); optional elsewhere | falls back to `postgres.DSNFromEnv()` with a startup warning outside production | BYPASSRLS pool for cross-tenant reconciler/cron sweeps, cascade `processed_events`, and the active-gauge exporter (`postgres.SystemDSNFromEnv`). Unset outside production → RLS-filtered zero rows, no alert |
| `PG_STATEMENT_TIMEOUT` | No | Helm `5s` | Applied to migration + sysPool DSNs (`postgres.ApplyStatementTimeout`); ignored on the app pool when `DATABASE_URL` is set verbatim |
| `PG_MAX_CONNS` / `PG_MIN_CONNS` / `PG_SLOW_QUERY_THRESHOLD` / `PG_BOUNCER_MODE` / `PG_SSLMODE` | No | Helm: `10` / `0` / `200ms` / `true` | Standard `pgcommon.ConfigFromEnv` vars. `PG_MIN_CONNS=0` with `PG_BOUNCER_MODE=true` is pgcommon's transaction-pooling recommendation |
| `VALKEY_ADDR` | No | `localhost:6379` | |
| `IDEMPOTENCY_TTL_SECONDS` | No | `86400` (24h) | Create-idempotency-key TTL (DLG-Q3) — **not** `CACHE_IDEMPOTENCY_TTL_SECONDS` |
| `LIST_CACHE_TTL_SECONDS` | No | `60` | DLG-1 list cache TTL — **not** `CACHE_LIST_TTL_SECONDS` |
| `USER_PROFILE_BASE_URL` | Yes | — | Client constructor fails fast at startup if empty (LLD §15) |
| `USER_PROFILE_TIMEOUT_MS` | No | `3000` | |
| `ORG_MEMBERSHIP_BASE_URL` | Yes | — | Client constructor fails fast at startup if empty |
| `ORG_MEMBERSHIP_MEMBERSHIP_CHECK_TIMEOUT_MS` | No | `3000` | |
| `SNS_TOPIC_ARN` | **Yes** | — | `loadConfig` returns an error if empty — the process never starts |
| `CASCADE_QUEUE_URL` | **Yes** | — | Same — fail-fast, not a soft default |
| `CASCADE_SQS_CONCURRENCY` | No | `4` | `events.WithConcurrency` on the cascade SQS consumer (iam-realm-provisioner) |
| `AWS_REGION` | No | `us-east-1` | |
| `AWS_ENDPOINT_URL` | No | — | LocalStack endpoint override, dev only |
| `GLUE_REGISTRY_NAME` | No | `""` → `NoopCodec` | Set to `iam-delegation-events` to activate `GlueCodec` (added this session, DLG-D20) |
| `OUTBOX_POLL_INTERVAL` / `OUTBOX_BATCH_SIZE` / `OUTBOX_MAX_ATTEMPTS` / `OUTBOX_DRAIN_TIMEOUT` / `OUTBOX_PUBLISH_CONCURRENCY` / `OUTBOX_PUBLISH_TIMEOUT` / `OUTBOX_STARTUP_JITTER` / `OUTBOX_CLAIM_LEASE_DURATION` | No | `500ms`/`50`/`5`/`30s`/`4`/`10s`/`2s`/`10m` | `outbox.Config` tunables — matches `iam-org-membership`'s identical env-var surface (DLG-D24) |
| `OUTBOX_PRUNE_INTERVAL` / `OUTBOX_PRUNE_RETENTION` / `OUTBOX_PRUNE_LIMIT` | No | `24h` / `168h` (7d) / `1000` | Daily sweep calling `outbox.Runner.PrunePublished` — matches `iam-user-profile`'s `runMaintenanceSweep`; without it `outbox_events` grows unbounded (DLG-D24) |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | No | — | |
| `DOCS_ENABLED` | No | `true` outside `production` | Gates `/swagger`, `/asyncapi` |
| `DOCS_AUTH_TOKEN` | No | — | Bearer-gates docs routes when `DOCS_ENABLED=true` in production |
| `POLICY_DEFAULT_MAX_DURATION_DAYS` / `POLICY_DEFAULT_REVIEW_WINDOW_DAYS` | No | `90` / `90` | Fallback tenant policy (DLG-D2) when no `delegation_tenant_settings` row exists |
| `SCHEMA_GOV_IMAGE` | CI-only | pinned per workflow | `platform-schemagov` CLI image — see Schema Governance below |

**No known dead config as of this pass** — `deploy/helm/iam-delegation/values.yaml`'s `env:` block
sets `SNS_TOPIC_ARN`/`CASCADE_QUEUE_URL` (the two `config.go` fail-fast-requires) and every other key
in it has a live Go reader; the previously-flagged dead `HTTP_PORT`/`METRICS_PORT`/`CACHE_KEYSPACE`/
`DATABASE_LOGICAL_NAME`/`REVIEW_WARN_EARLY_DAYS`/`REVIEW_WARN_LATE_DAYS`/`SQS_CONCURRENCY`/
`EVENTS_TOPIC`/`EVENTS_SOURCE`/`SQS_QUEUE_URL`/`SQS_DLQ_URL` keys this section used to list have all
been removed from `values.yaml`. Re-audit if `values.yaml` grows a new key — this note is a
point-in-time finding, not a standing guarantee.

# CI/CD

GitHub Actions (`.github/workflows/`), all names/steps verified against current YAML:

1. **`validate-test.yml`** (`Validate / Test`) — on push/PR: unit+integration+rls tests with `-race`
   and merged coverage → coverage-threshold gate (`.github/scripts/coverage-gate.sh`, **95%
   single global floor**, not a per-package gate — bumped from the original 70% during the
   DLG-D34 production-readiness sweep, global coverage sits at 95.2% as of that pass) → upload
   coverage artifact → architecture lint (`go-arch-lint`) → Swagger staleness check → end-to-end
   tests.
2. **`validate-quality.yml`** (`Validate / Quality`) — on push/PR: a repo-specific check rejecting
   HTML-escaped operators in workflow files, a repo-specific check rejecting non-transaction-local
   `SET app.tenant_id` (RLS-6 enforcement, `.github/scripts/check-forbidden-set-guc.sh`), gofmt
   check, `go mod tidy` drift check, `go vet`, `golangci-lint`, `govulncheck`, `go mod verify`,
   Dockerfile base-image digest check.
3. **`ci.yml`** (`CI`) — `build-image` job: Hadolint + `.dockerignore` check → Buildx build
   (**`linux/amd64` only**, not multi-platform) with GHCR cache → Trivy CVE scan (CRITICAL/HIGH/
   UNKNOWN, fails the build) + a second SARIF-upload scan to the GitHub Security tab → smoke tests
   against **both** the `server` and `reconciler` entrypoints from the same built image → push to
   GHCR with provenance+SBOM attached → Cosign keyless sign + self-verify. `pr-summary` job posts a
   status table on every PR push.
4. **`changelog-check.yml`** — fails if `CHANGELOG.md` isn't updated when source changes.
5. **`release.yml`** (`v*` tags) — same single-platform build with `provenance: mode=max` + CycloneDX
   SBOM, Cosign sign, SLSA provenance attestation, GitHub Release with checksums/SBOM/provenance as
   artifacts. `schema-registry.yml`'s `production` job also fires on `release: published`.
6. **Schema governance workflows** (all new this session, DLG-D20) — `schema-registry.yml`
   (validate/diff/register on push+PR+release), `schema-prune.yml` (monthly orphan cleanup, manual
   execute), `schema-health-quarterly.yml` (read-only quarterly report), `freeze-watchdog.yml`
   (daily `SCHEMA_FREEZE` staleness alert). See Schema Governance below and
   `docs/runbook-schema-registry.md`.

# Schema Governance (`platform-schemagov`)

Read this before touching anything in `scripts/`, `.github/workflows/schema-registry.yml`,
`schema-prune.yml`, `schema-health-quarterly.yml`, `freeze-watchdog.yml`, or any file related to
Glue or event-schema validation.

**Status: this repo is on the shared `platform-schemagov` tool as of this session (DLG-D20,
`ARCHITECTURE.md`'s "Session-specific decisions").** Before this session, `schema-registry.yml` ran a bespoke inline Python
structural check (JSON Schema syntax + AsyncAPI shape only, no live Glue registry) — that has been
fully replaced with the `platform-schemagov` CLI, invoked via `docker run schema-gov <command>`,
mirroring `iam-user-profile`'s pipeline. This repo's registry is **`iam-delegation-events`** — a
dedicated registry, distinct from any sibling service's.

All schema-governance logic lives in `github.com/BCBP-SOLUTIONS-FZC-LLC/platform-schemagov`:

```
platform-schemagov/
  schemagov/core/       — pure validation and classification logic (no I/O, no AWS)
  schemagov/adapters/   — AWS + filesystem I/O (boto3, git, filesystem)
  schemagov/cli/        — CLI commands exposed as `schema-gov`
```

**Rule: keep governance logic out of `scripts/` and out of workflow `run:` blocks. Extend
`platform-schemagov` instead.** When asked to add, extend, or fix schema-governance behavior, the
answer is always: implement it in `platform-schemagov`, expose it as a CLI command, call it from
the workflow via `docker run schema-gov <command>`.

## Local scripts — what's actually in `scripts/`

Just one file — this repo has no `init-db.sql`/`patch-swagger-extensions.py`/`merge_coverage.py`
equivalents (don't assume symmetry with `iam-user-profile`'s `scripts/` contents):

| File | Purpose |
|---|---|
| `scripts/init-localstack.sh` | Local dev: SNS topic + `delegation-cascade-q`/DLQ bootstrap, plus best-effort Glue registry + 3-schema registration (`docker-compose.pro.yml` only — Community LocalStack has no Glue) |

Do not add any new governance-related file to `scripts/`.

## Rules

**Do:**
- Implement new governance behavior in `platform-schemagov` and call it from the workflow.
- When a script has a bug, fix the `platform-schemagov` module — not the script.
- If `schema-gov` is missing a command you need, say so explicitly and implement the command in
  `platform-schemagov` before wiring it into the workflow.

**Do not:**
- Add new files to `scripts/` for governance logic.
- Add `pip install`, `python3 -c`, or inline Python governance logic to a workflow `run:` block —
  this repo already removed its one instance of that pattern (the pre-DLG-D20 structural check).
- Add new `aws` CLI commands or `jq` pipelines inside workflow `run:` blocks beyond what
  `schema-registry.yml`'s existing `diff_schema` helper already does (the raw `aws glue get-schema`/
  `get-schema-version` calls there are a deliberate, documented exception — see that file's header
  comment on `SCHEMA_NAME_MAP` — not a pattern to extend elsewhere).
- Copy validation logic from one place to another — if it exists in `platform-schemagov`, call it
  from there.

## Adding new governance behavior

1. **Core module** (`platform-schemagov/schemagov/core/your_module.py`) — pure logic, no
   `print()`/`sys.exit()`/`open()`/`boto3`; returns a result dataclass with `errors`/`warnings`.
2. **Adapter** (`platform-schemagov/schemagov/adapters/your_adapter.py`) — I/O and AWS, client
   injected via `__init__` for testability.
3. **CLI command** (`platform-schemagov/schemagov/cli/commands/your_command.py`) — owns output and
   exit codes; register in `platform-schemagov/schemagov/cli/main.py`.
4. **Workflow step** — call via `docker run ... "$SCHEMA_GOV_IMAGE" your-command [flags]`.

The core module must be testable without Docker, AWS credentials, or filesystem access.

## Docker Run Reference

```yaml
env:
  SCHEMA_GOV_IMAGE: ghcr.io/bcbp-solutions-fzc-llc/platform-schemagov:0.4
```

```bash
docker run --rm \
  -v "${{ github.workspace }}":/workspace \    # repo root → /workspace
  -v "${{ runner.temp }}":/tmp \               # metric JSON files shared between steps
  -e AWS_ACCESS_KEY_ID \
  -e AWS_SECRET_ACCESS_KEY \
  -e AWS_SESSION_TOKEN \
  -e AWS_REGION="${{ vars.AWS_REGION }}" \
  -e GITHUB_ACTIONS \                          # enables ::error:: / ::warning:: annotations
  "${{ env.SCHEMA_GOV_IMAGE }}" <command> [flags]
```

| Command | Fatal on failure | AWS required |
|---|---|---|
| `validate --asyncapi F --schema-dir D` | Yes | No |
| `diff --current F --proposed F --schema-name N` | Yes | No |
| `prune --registry R --env E --output F` | No (dry-run orphan detection) | Yes |
| `usage-check --source prometheus\|disabled ...` | No (always exits 0) | No |
| `enforce-lifecycle --usage-file F --asyncapi F` | Yes | No |
| `register --registry R --schema-dir D` | Yes | Yes |
| `changelog --asyncapi F ...` | Yes | No |
| `metrics --registry R --env E ...` | No (always exits 0) | Yes |

This repo does **not** use `extract` (`make extract-schemas`) — unlike `iam-user-profile`,
`api/asyncapi.yaml` and `internal/eventschema/*.json` are both hand-maintained independently here
(DLG-D20); there is no generative step deriving one from the other.

`usage-check` source selection: set `vars.PROMETHEUS_URL` to use Prometheus (no AWS credentials
needed); leave it unset to use `--source disabled` (lifecycle enforcement is skipped).

The image is built and published from `platform-schemagov` CI. Pin to a digest for production
stability; this repo currently pins the floating tag `:0.4` (see `SCHEMA_GOV_IMAGE` in
`.github/workflows/schema-registry.yml`'s `env:` block — same tag used in `Makefile`'s
`schema-*` targets and `docs/runbook-schema-registry.md`).
