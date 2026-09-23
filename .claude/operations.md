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

## Idempotency, docs auth, and outbound response bounds (DLG-D35 hardening pass)

- **Create idempotency is an atomic `SETNX` reservation, not check-then-act.**
  `port.IdempotencyStore.Reserve`/`Release` (`internal/adapter/outbound/valkey/idempotency.go`)
  close a real race the original `Get`-then-`Save` implementation left open — two concurrent
  same-key `POST /delegations` could both miss the `Get` and both insert. A losing concurrent
  caller now gets `409 idempotency_key_in_flight`, not a duplicate delegation.
- **Swagger/AsyncAPI docs bearer-token check is constant-time.** `router.go`'s
  `docsAuthMiddleware` compares via `crypto/subtle.ConstantTimeCompare`, not `!=` — closes a
  timing side-channel that could otherwise leak the token byte-by-byte. Gates the docs routes
  only, not the API itself.
- **Outbound response bodies are capped at 1 MiB.** `internal/adapter/outbound/httpx.LimitBody`
  wraps `resp.Body` before `json.NewDecoder(...).Decode` in both the User Profile and Org
  Membership HTTP clients, bounding worst-case memory use if a mesh peer returns an oversized or
  malformed body.

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

Three-tier taxonomy per Enterprise Platform Observability Standard. `platform_*` registered on `prometheus.WrapRegistererWith({domain,service,environment})` via `gincommon.MetricsRegisterer()`. Tier 3 `iam_delegation_*` carry `{service,version,environment}` ConstLabels. Tier 2 (`iam_*`) not currently emitted — all signals are either cross-domain (Tier 1) or delegation-specific (Tier 3).

**Tier 1 — `platform_*` (cross-domain, `{domain,service,environment}` injected centrally)**

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `platform_messages_received_total` | Counter | queue | Every SQS delivery entering cascade Handle(), regardless of outcome |
| `platform_messages_processed_total` | Counter | queue | Successfully handled messages; dual-emits with `iam_delegation_cascade_processed_total` (compat) |
| `platform_messages_failed_total` | Counter | queue | Handler-returned errors; dual-emits with `iam_delegation_cascade_dlq_total` (compat) |
| `platform_duplicate_messages_total` | Counter | queue | SQS redeliveries skipped via processed_events (IDEMP-4); Registry-Proposed |
| `platform_dependency_request_seconds` | Histogram | target_service, endpoint | Latency of outbound HTTP calls to org_membership, user_profile, catalog_admin; dual-emits with Tier 3 legacy; Registry-Proposed |
| `platform_dependency_errors_total` | Counter | target_service, endpoint, outcome | Outbound call failures (timeout\|5xx); dual-emits with Tier 3 legacy; Registry-Proposed |

**Tier 3 — `iam_delegation_*` (service-specific, `{service,version,environment}` ConstLabels)**

| Metric | Type | Labels | Recorder method |
|---|---|---|---|
| `iam_delegation_created_total` | Counter | scope | `RecordCreated(scope)` |
| `iam_delegation_ended_total` | Counter | ended_reason | `RecordEnded(reason)` |
| `iam_delegation_active_gauge` | Gauge | tenant | `ReplaceActiveGauges` (5-min sysPool snapshot) |
| `iam_delegation_expiry_deferred_total` | Counter | — | `RecordExpiryDeferred()` |
| `iam_delegation_review_deferred_total` | Counter | — | `RecordReviewDeferred()` |
| `iam_delegation_activation_deferred_total` | Counter | — | `RecordActivationDeferred()` |
| `iam_delegation_review_warned_total` | Counter | days_remaining | `RecordReviewWarned(daysRemaining)` |
| `iam_delegation_review_expired_total` | Counter | — | `RecordReviewExpired()` |
| `iam_delegation_membership_check_duration_seconds` | Histogram | — | `ObserveMembershipCheckDuration(seconds)` — legacy Tier 3; superseded by `platform_dependency_request_seconds{target_service="org_membership"}` |
| `iam_delegation_membership_check_failures_total` | Counter | — | `RecordMembershipCheckFailure()` — legacy Tier 3 |
| `iam_delegation_up_availability_failures_total` | Counter | path | `RecordUPAvailabilityFailure(path)` — legacy Tier 3 |
| `iam_delegation_dependency_call_duration_seconds` | Histogram | target_service, endpoint | Legacy Tier 3 predecessor of `platform_dependency_request_seconds`; dual-emitted during compat period |
| `iam_delegation_dependency_call_failures_total` | Counter | target_service, endpoint, outcome | Legacy Tier 3 predecessor of `platform_dependency_errors_total`; dual-emitted during compat period |
| `iam_delegation_idempotency_hits_total` | Counter | — | `RecordIdempotencyHit()` |
| `iam_delegation_cascade_processed_total` | Counter | — | Legacy; use `platform_messages_processed_total{queue="delegation-cascade-q"}` |
| `iam_delegation_cascade_dlq_total` | Counter | — | Legacy; use `platform_messages_failed_total{queue="delegation-cascade-q"}` |
| `iam_delegation_processed_events_duplicates_total` | Counter | consumer | Legacy; use `platform_duplicate_messages_total` |
| `iam_delegation_unknown_event_acknowledged_total` | Counter | consumer, event_type | Forward-compat acks |

## Alerting (LLD §14.5, DLG-D39)

Two layers, both sourced from the metrics table above and kept in sync across three files:

- **Threshold alerts** — `deploy/monitoring/app-alerts.yml` and `templates/prometheusrule.yaml`
  render the same flat alerts: availability (`up`, replica count), HTTP 5xx rate (warning/critical),
  per-cron deferral (`*_deferred_total` increase over 30m, one per CronJob), cascade DLQ growth,
  outbox dead-letters (`platform-events`' own metric), and membership-check failure rate > 5%.
- **SLO burn-rate alerts** — `deploy/monitoring/slo-rules.yml` and the same
  `templates/prometheusrule.yaml`'s `iam_delegation_slo_records`/`iam_delegation_slo_burn` groups
  add a multi-window multi-burn-rate layer (SRE book Ch. 5) on top: four SLOs (write-path error
  rate 99.9%, DLG-D3 membership-check success 99%, reconciler-defer convergence, cascade
  convergence), each a recording rule plus a fast-burn/page and slow-burn/ticket alert pair. Unlike
  the sibling services that keep `slo-rules.yml` standalone, this service also wires the same
  groups into the Helm-rendered `PrometheusRule` — the standalone file is kept only as the
  `--rule-files` copy for environments not installing via the chart.

## Tracing and logging

OTel via `platform-gincommon` (same shared lib as sibling services); structured logging via Zap,
wired through `logger.NewLogger` as a single `internal/core/port.Logger` (DLG-D42, matching
`iam-user-profile` / `iam-org-membership`). Both binaries call `gincommon.InitTracingFromEnv` then
`ObservabilityMiddlewares` at startup **before** creating tracers or registering collectors
(DLG-D31, matching `iam-realm-provisioner`), then share one `postgres.NewOTelTracer` across
the app and sys pools so `db.query` spans share the HTTP OTLP pipeline. Outbound User Profile /
Org Membership calls go through `internal/adapter/outbound/httpx` (`otelhttp`) so they emit
client spans and inject `traceparent`. The reconciler wraps each job in a
`reconciler.<job>` span and also calls `events`/`pgmetrics` `InitWithRegisterer`. No custom
TracerProvider/exporter. `/metrics` is served on `METRICS_PORT` (default 9090), not on the API
listener. `metrics.Register()` is the no-arg gincommon API.

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
| `ENVIRONMENT` | No | `development` | Read by `resolveAppEnv()` only when `APP_ENV` is unset (DLG-D37) |
| `APP_ENV` | No | — | Takes precedence over `ENVIRONMENT` in `resolveAppEnv()` — so a deploy that sets only `APP_ENV=production` cannot accidentally skip the `SYSTEM_DATABASE_URL` guard below the way a bare `ENVIRONMENT` default would (DLG-D37, matching `iam-org-membership`'s `isDevLikeEnv`) |
| `PORT` | No | `8080` | HTTP listen port — **not** `HTTP_PORT` (no dead config remains as of this pass, see below) |
| `DATABASE_URL` | Yes | — | App role (`delegation_app`, RLS-enforced, no BYPASSRLS) — or set `PG_HOST`/`PG_PORT`/`PG_USER`/`PG_PASSWORD`/`PG_DBNAME`/`PG_SSLMODE` individually; both forms go through `pgcommon.ConfigFromEnv` via `postgres.DSNFromEnv`. `loadConfig` now fails fast with an explicit error if neither form is set (DLG-D37, matching `iam-realm-provisioner`'s/`iam-org-membership`'s `validateRequiredEnv`) rather than relying on pgcommon's own error |
| `MIGRATION_DATABASE_URL` | Required when `PG_BOUNCER_MODE=true` (DLG-D37) | falls back to `postgres.DSNFromEnv()` | BYPASSRLS role for the server's self-migration at startup; must bypass PgBouncer — migrations take a session-scoped `pg_advisory_lock`, which PgBouncer's transaction pooling cannot hold across statements |
| `SYSTEM_DATABASE_URL` | Helm: required. Go: required outside `isDevLikeEnvironment(resolveAppEnv())` (`development`/`dev`/`local`/`test` — both binaries fail fast at startup otherwise, DLG-D34, widened from a literal `production` check in DLG-D35, then keyed off `resolveAppEnv()` with `test` added as a fourth alias in DLG-D37); optional in local/dev | falls back to `postgres.DSNFromEnv()` with a startup warning in local/dev | BYPASSRLS pool for cross-tenant reconciler/cron sweeps, cascade `processed_events`, and the active-gauge exporter (`postgres.SystemDSNFromEnv`). Unset outside local/dev → RLS-filtered zero rows, no alert |
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
| `CASCADE_SQS_CONCURRENCY` | No | `4` | Overlaid onto `config.LoadSQS` (which reads `SQS_CONCURRENCY`) then applied via `SQSConsumerOptions` / `WithConcurrency` (iam-user-profile / iam-org-membership) |
| `AWS_REGION` | No | `ap-south-1` | |
| `AWS_ENDPOINT_URL` | No | — | floci endpoint override, dev only |
| `GLUE_REGISTRY_NAME` | No | `iam-delegation-events` (dev default) → `""` forces `NoopCodec` | floci provisions this registry for free, so `GlueCodec` runs by default in local dev (DLG-D20) |
| `OUTBOX_POLL_INTERVAL` / `OUTBOX_BATCH_SIZE` / `OUTBOX_MAX_ATTEMPTS` / `OUTBOX_DRAIN_TIMEOUT` / `OUTBOX_PUBLISH_CONCURRENCY` / `OUTBOX_PUBLISH_TIMEOUT` / `OUTBOX_STARTUP_JITTER` / `OUTBOX_CLAIM_LEASE_DURATION` | No | `500ms`/`50`/`5`/`30s`/`4`/`10s`/`2s`/`10m` when set in Helm / `.env.example`; otherwise platform-events `LoadOutbox` library defaults (`5s`/`50`/`5`/`30s`/`1`/`10s`/`0`/`0`) | Passed through `config.LoadOutbox` → `RunnerConfigFromEnv` (iam-user-profile / iam-org-membership, DLG-D44). Helm keeps the historical values so production does not silently change poll/concurrency. |
| `OUTBOX_PRUNE_INTERVAL` / `OUTBOX_PRUNE_RETENTION` / `OUTBOX_PRUNE_LIMIT` | No | `24h` / `168h` (7d) / `1000` | Daily sweep calling `outbox.Runner.PrunePublished` — matches `iam-user-profile`'s `runMaintenanceSweep`; `LoadOutbox` does not cover prune. |
| `PROCESSED_EVENTS_TTL_DAYS` | No | `30` | Monthly `delegation-cleanup` prune window for `processed_events` (LLD §18.4, DLG-D38). Realm-provisioner defaults to 8 with a dedicated CronJob; this service keeps the LLD's 30-day window bundled into cleanup. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | No | — | |
| `DOCS_ENABLED` | No | `true` outside `production` | Gates `/swagger`, `/asyncapi` |
| `DOCS_AUTH_TOKEN` | No | — | Bearer-gates docs routes when `DOCS_ENABLED=true` in production — compared via `crypto/subtle.ConstantTimeCompare`, not `!=` (DLG-D35) |
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
   DLG-D34 production-readiness sweep, global coverage sat at 95.2% as of that pass, 95.4% as of
   the DLG-D35 follow-up sweep) → upload coverage artifact → architecture lint (`go-arch-lint`) →
   Swagger staleness check → end-to-end tests (`make test-e2e`, `-tags=e2e` — genuinely exercises
   `cmd/server/e2e_test.go` against real Postgres/Valkey/floci containers as of DLG-D41; this step
   pre-dates that file and was previously a silent no-op re-run of the unit suite).
2. **`validate-quality.yml`** (`Validate / Quality`) — on push/PR: HTML-escaped operators check,
   then **seven repo-specific enforcement scripts** (each exits non-zero on violation):
   - `check-forbidden-set-guc.sh` — no session-scoped `SET app.tenant_id` (RLS-6)
   - `check-forbidden-events-bypass.sh` — no direct SNS/SQS SDK calls, no hand-built `events.Envelope{}` literals, scans `cmd/ internal/ pkg/` (DLG-D45/D46)
   - `check-outbox-access.sh` — no hand-rolled SQL against `outbox_events` (DLG-D47)
   - `check-metric-namespacing.sh` — platform_* suffix rules, iam_delegation_* suffix rules, forbidden high-cardinality labels, required `platform_dependency_*` metrics
   - `check-observability-compliance.sh` — logs/metrics/traces via platform-gincommon only (9 rules L-1..L-4, M-1..M-3, T-1..T-2)
   - `check-platform-events-compliance.sh` — events/outbox/dedup via platform-events only (11 rules P-1..P-7, C-1..C-2, D-1..D-2)
   - `check-pgcommon-compliance.sh` — DB connections/config/operations via platform-pgcommon only (11 rules PC-1..PC-3, CF-1..CF-3, TX-1..TX-3, OQ-1..OQ-2)

   Then: gofmt check, `go mod tidy` drift, `go vet`, `golangci-lint`, `govulncheck`, `go mod verify`, Dockerfile base-image digest check.
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
| `scripts/init-floci.sh` | Local dev: Glue registry + 4-schema registration, SNS topic, `delegation-cascade-q`/DLQ bootstrap, and the three downstream fan-out queues (Workflow/Notification/Audit, see LLD §10.4) — always runs (floci includes Glue Schema Registry free, unlike LocalStack Community which gated it behind Pro) |

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
  comment on schema files and names — not a pattern to extend elsewhere).
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

`extract` is the generative step (DLG-D51, reversing DLG-D20's hand-maintained model): `make
extract-schemas` derives `internal/eventschema/*.json` (4 produced + 3 consumed) from
`api/asyncapi.yaml`'s `<Name>Payload` schemas, and every `schema-registry.yml` job runs
`extract --check` ("Event schema sync check"; `make schema-sync-check` locally), failing on drift.
`usage-check`, `diff` and `register` read the PascalCase produced-only copy in `.tmp/glue-schemas`
(`stage-produced-event-schemas.sh`) because schema-gov names schemas after their file stems.

`usage-check` source selection: set `vars.PROMETHEUS_URL` to use Prometheus (no AWS credentials
needed); leave it unset to use `--source disabled` (lifecycle enforcement is skipped).

The image is built and published from `platform-schemagov` CI. Pin to a digest for production
stability; this repo currently pins the floating tag `:0.4` (see `SCHEMA_GOV_IMAGE` in
`.github/workflows/schema-registry.yml`'s `env:` block — same tag used in `Makefile`'s
`schema-*` targets and `docs/runbook-schema-registry.md`).
