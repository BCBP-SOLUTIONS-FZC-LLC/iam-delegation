# iam-delegation

Delegation Service (out-of-office delegations) for the Tender Management SaaS Platform's IAM
subsystem — the fourth and last O&M extraction from `iam-org-membership` (ADR-0008, Option C). Owns
`delegations` + `delegation_tenant_settings`, the DLG-1..7 / DLG-I1..I4 API, the
delegation-expiry/-review/-cleanup CronJobs, and the `iam.delegation.events` topic.

## Mental model

A user going out-of-office delegates their pending work to a colleague for a bounded window. This
service is the system of record for that delegation: who delegated to whom, over what scope (all
work / one department / one tender), for how long, and why it ended (expired, cancelled, the
delegate was removed, or the review window lapsed unconfirmed).

It has **two** synchronous outbound dependencies of its own — grant-time membership-existence
checks against `iam-org-membership` (DLG-D3, replacing two composite foreign keys Core no longer
enforces) and availability-first pointer-set/clear calls against `iam-user-profile` (DEL-6,
`user_availability.delegate_id`) — and **one** inbound async subscription, `delegation-cascade-q`
(fed by Core's `MembershipRevoked` and `TenantMembershipsPurged`, §11.5/§11.6). It publishes its own events
(`DelegationStarted`/`DelegationEnded`/`DelegationReviewRequested`) to a dedicated topic,
`iam.delegation.events` — this service is both a producer and a consumer, unlike most of its O&M
extraction siblings.

Under Option C (ADR-0008 §13.1/§14), Core's I-8 endpoint no longer embeds `active_delegations[]` at
all — Core dropped the `delegations` table entirely. Nothing in this service feeds I-8; see
[ARCHITECTURE.md](ARCHITECTURE.md) and LLD §6.1 for where those consumers were re-sourced instead.

If you're familiar with `iam-org-membership`, `iam-catalog-admin`, `iam-group-mapping`, or
`iam-tender-acl`, the package layout will look immediately familiar: Clean Architecture /
Ports-and-Adapters, `internal/core/{domain,port,service}` + `internal/adapter/{inbound,outbound}`.
See [ARCHITECTURE.md](ARCHITECTURE.md) for the full package tree and the LLD's DLG-D1..D12 decision
register.

## Why this service exists

Out-of-office delegation started as one join in `iam-org-membership`'s I-8 response
(`active_delegations[]`) and a `delegations` table Core owned alongside its actual
membership data. ADR-0008's decomposition of `iam-org-membership` treats that as a
fourth, separable concern — one with its own lifecycle (create/extend/reassign/cancel,
a review-window sweep, an expiry sweep) that has nothing to do with tenant membership
itself, and its own consumers (Workflow, Notification, Audit) that never needed to go
through Core to get it.

Under **Option C** (DLG-D1, resolved in this build after the backward-compatibility
constraint that produced the original Option A/B split was lifted — see the LLD's
revision history), Core drops `delegations` and `active_delegations[]` entirely rather
than keeping a synced projection: I-8 gets a faster four-table join, and every
would-be consumer of that field is re-sourced to this service's own events or
endpoints instead (§6.1). The alternative — leaving delegation logic embedded in
Core — would have kept coupling Core's hot-path membership endpoint to a feature
with a completely different read/write pattern (bursty writes on create/cancel,
two background sweeps, its own outbound event topic) for no benefit once nothing
else needs the embed.

Centralizing it here also means the two lost composite foreign keys (delegator/delegate
membership) and the lost tenant-cascade FK aren't quietly dropped — they become explicit,
documented compensations (DLG-D3's synchronous membership checks, DLG-D4's async
cascade consumer) instead of implicit guarantees a schema constraint used to provide for
free.

## Binaries

This repo ships **two** binaries (`cmd/server`, `cmd/reconciler` — LLD §16.1) built into
**one** image (`Dockerfile`) — the Deployment runs it unmodified; each CronJob overrides
`command` to invoke the reconciler binary instead (mirrors `iam-user-profile`'s single-image
pattern):

| Binary | Entrypoint | What it does |
|---|---|---|
| `cmd/server` | `/iam-delegation-server` (the image's default `ENTRYPOINT`) | HTTP API (DLG-1..7, DLG-I1..I4) **and** the `delegation-cascade-q` SQS consumer, run in-process via `errgroup` — one Deployment, ports 8080 (HTTP) / 9090 (metrics). |
| `cmd/reconciler` | `/iam-delegation-reconciler` (`command` override) | The three CronJob entry points under `cmd/reconciler/jobs/`, dispatched at invocation time by a `--job=<name>` flag: `delegation-expiry` (`*/5 * * * *`, DLG-I1), `delegation-review` (`0 * * * *`, DLG-I2), `delegation-cleanup` (`0 4 1 * *`, soft-delete purge). No HTTP/metrics server — three separate `CronJob` resources, no ports exposed. |

## API overview

| ID | Method & path | Auth | Notes |
|---|---|---|---|
| DLG-1 | `GET /api/v1/delegations` | self (gateway identity) | List active delegations for the caller. Cached (`del:list`, advisory — correctness never depends on it). |
| DLG-2 | `POST /api/v1/delegations` | self = delegator | Create. Requires `Idempotency-Key` (24h dedup, DLG-D8). Runs two concurrent grant-time membership-existence checks against `iam-org-membership` (DLG-D3) — fails `422`/`503` if either check fails or reports inactive. |
| DLG-3 | `DELETE /api/v1/delegations/{id}` | self or admin | Cancel. Pointer-clear against `iam-user-profile` is fail-open (DEL-6) — cancellation always succeeds even if the availability pointer-clear call fails. |
| DLG-4 | `POST /api/v1/delegations/{id}/extend` | self or admin | Extend the review window (`extend_days` in `[1,180]`). Resets the daily-cascade warn bucket (DLG-D7). |
| DLG-5 | `POST /api/v1/delegations/{id}/reassign` | self or admin | End the current delegation and create a new one to a different delegate in one call (fuller body — DLG-D11). |
| DLG-6 | `GET /api/v1/delegations/settings` | any authenticated tenant member | Read the tenant's delegation policy (`delegation_tenant_settings`, falling back to `POLICY_DEFAULT_*` env defaults, DLG-D2). |
| DLG-7 | `PUT /api/v1/delegations/settings` | `tenant_admin`/`tenant_owner` | Set the tenant's delegation policy. |
| DLG-I1 | `POST /internal/delegations/expire` | mesh-only (mTLS) | Run the expiry sweep on demand — normally driven by the `delegation-expiry` CronJob. |
| DLG-I2 | `POST /internal/delegations/review-sweep` | mesh-only (mTLS) | Run the review-window sweep on demand — normally driven by the `delegation-review` CronJob. |
| DLG-I3 | `GET /internal/delegations/dept-delegate` | mesh-only (mTLS) | Find the active department-scope delegate for a user — consulted by Core's user-removal flow (DLG-D9, §11.5). |
| DLG-I4 | `GET /internal/users/{id}/active-delegations` | mesh-only (mTLS) | List a user's active outbound delegations — the escape hatch that replaces Core's old `active_delegations[]` embed (§6.1). Nothing calls it today. |

`/internal/*` (DLG-I1..I4) shares the same HTTP port as the public API but is a mesh-only mTLS trust
boundary at the network layer (LLD §13.2) — no RBAC/JWT check, no `ContextMiddleware` (there is no
gateway identity to bridge on these routes).

Full request/response schemas: Swagger UI at `/swagger` (generated from handler annotations via
`make swag`). Event contract: [`api/asyncapi.yaml`](api/asyncapi.yaml), also browsable as a
server-rendered HTML catalog at `/asyncapi` — `GET /asyncapi.yaml` serves the spec itself, embedded
into the binary at compile time (`api/embed.go`).

Both docs routes are dev-only by default; see `DOCS_ENABLED`/`DOCS_AUTH_TOKEN` below to opt either
into production.

## Input validation

Errors are returned via `gincommon.ErrorResponse` — a flat `{error, status, trace_id, request_id}`
body, `error`/`status` matching the table below (`internal/adapter/inbound/http/errors.go`'s
`errorStatusByCode` map, sourced from LLD §20's error taxonomy). There is no per-field error array
like some sibling services return for `422`s — each rule below maps to one sentinel code for the
whole request.

| Rule | Error code | Status |
|---|---|---|
| `Idempotency-Key` header required on `POST /delegations` | `validation_error` | 400 |
| `scope` must be one of `all` / `department` / `tender` | `invalid_delegation_scope` | 400 |
| `scope_id` required when `scope` is `department`/`tender` | `scope_id_required` | 422 |
| `scope_id` must be omitted when `scope` is `all` | `invalid_scope_id` | 422 |
| `delegate_id` must not equal the caller (no self-delegation) | `self_delegation` | 422 |
| `reason` (the audit-only note) ≤ 500 characters | `reason_too_long` | 422 |
| `starts_at` must not be in the past | `delegation_start_in_past` | 422 |
| `starts_at` must be within 1 year from now | `delegation_start_too_far_future` | 422 |
| `ends_at` must be after `starts_at` | `delegation_window_inverted` | 422 |
| Span (`ends_at - starts_at`) must not exceed the tenant's `max_duration_days` | `delegation_window_too_long` | 422 |
| `extend_days` must be in `[1, 180]` | `extend_days_out_of_range` | 422 |
| Extend only applies to open-ended (review-tracked) delegations, not a fixed `ends_at` | `not_review_tracked` | 422 |
| Tenant policy `max_duration_days` must be in `[1, 180]` | `invalid_delegation_max_duration_days` | 400 |
| Tenant policy `review_window_days` must be in `[1, 180]` | `invalid_delegation_review_window_days` | 400 |
| Delegate must be an active member of the tenant (both parties checked against `iam-org-membership`) | `invalid_delegate` | 422 |
| Optimistic lock — `record_version` mismatch on cancel/extend/settings update | `optimistic_lock_conflict` | 409 |
| Target delegation not found (or already terminal, for cancel) | `delegation_not_found` | 404 |

Validation runs in `internal/core/service/{delegation_service,settings_service}.go`, not a
separate validator package — see those files for the exact ordering (e.g. scope/self-delegation
checks run before the two membership-existence calls, so a malformed request never reaches
`iam-org-membership`).

## Local development

```sh
make setup           # install tools, git hooks
make docker-up        # postgres + valkey + localstack (SQS/SNS-compatible), see docker-compose.yml
make run              # runs cmd/server against docker-up's dependencies
```

Or bring up the whole stack, including the service itself, in one shot:

```sh
make compose-up       # postgres + valkey + localstack + iam-delegation (self-migrates at startup)
make compose-down     # stop and remove everything, including volumes
```

The two synchronous outbound dependencies — `iam-user-profile` (DEL-6) and `iam-org-membership`
(DLG-D3 membership checks) — are **not** part of this compose stack; run them separately (each has
its own `docker-compose.yml`) and point `USER_PROFILE_BASE_URL`/`ORG_MEMBERSHIP_BASE_URL` at them,
or accept the degraded/fail paths (DEL-6 defers-and-retries; a membership-check failure rejects
DLG-2 create with `503 org_membership_unavailable`).

Docs, once running (port from `HTTP_PORT`, default `8080`):

| Tool | URL | Notes |
|---|---|---|
| Swagger UI | `http://localhost:8080/swagger/index.html` | REST contract (DLG-1..7, DLG-I1..I4). Regenerate after changing a handler's `// @…` annotations with `make swag`; `make swag-check` is the CI drift gate. |
| AsyncAPI viewer | `http://localhost:8080/asyncapi` | Event contract browser for `api/asyncapi.yaml`. `GET /asyncapi.yaml` serves the raw spec. |

### Configuration

The full set of non-secret environment variables (with production defaults) lives in
[`deploy/helm/iam-delegation/values.yaml`](deploy/helm/iam-delegation/values.yaml)'s `env:` block —
that file is the single source of truth for config keys, kept in sync with the LLD §15
`values-prod.yaml` excerpt. Secrets (`DATABASE_URL`, `MIGRATION_DATABASE_URL`, `VALKEY_PASSWORD`,
`DOCS_AUTH_TOKEN`) are supplied separately — see `envFromSecret`/`secretValues`/`existingSecret` in
the same file, and `docker-compose.yml` for their dev-only values.

Notable variables:

| Variable | Purpose | Default |
|---|---|---|
| `DATABASE_URL` | `delegation_app` (RLS-bound) connection string — or set `PG_HOST`/`PG_PORT`/`PG_USER`/`PG_PASSWORD`/`PG_DBNAME`/`PG_SSLMODE` individually; both are read by `pgcommon.ConfigFromEnv` via `postgres.DSNFromEnv` | *(required, one form or the other)* |
| `MIGRATION_DATABASE_URL` | `delegation_migrator` (BYPASSRLS) connection string for the startup migration run (`postgres.Migrate`, LLD §7.4/§16.4); must bypass PgBouncer | falls back to `postgres.DSNFromEnv()` |
| `SYSTEM_DATABASE_URL` | BYPASSRLS connection string for the reconciler/cascade consumer's cross-tenant sweep pool (`postgres.SystemDSNFromEnv`) | falls back to `postgres.DSNFromEnv()`, with a startup warning that cross-tenant sweeps will be RLS-filtered |
| `PG_STATEMENT_TIMEOUT` | Server-side `statement_timeout` appended to the app DSN (Go duration string, e.g. `5s`) so a hung query releases its pool connection instead of holding it for the full request deadline; ignored when `DATABASE_URL` is set verbatim | — |
| `PG_MAX_CONNS` / `PG_MIN_CONNS` / `PG_SLOW_QUERY_THRESHOLD` / `PG_BOUNCER_MODE` | Standard `pgcommon.ConfigFromEnv` pool-sizing vars | pgcommon defaults |
| `USER_PROFILE_BASE_URL` | `iam-user-profile`'s base URL, consulted for DEL-6 availability-first pointer set/clear | *(required)* |
| `ORG_MEMBERSHIP_BASE_URL` | `iam-org-membership`'s base URL, consulted for the two DLG-D3 grant-time membership checks | *(required)* |
| `VALKEY_ADDR` | `del:` keyspace — DLG-1 list cache + create-idempotency-key store (DLG-Q3) | `localhost:6379` |
| `SNS_TOPIC_ARN` | Outbound topic (`iam.delegation.events`) the outbox publishes `DelegationStarted`/`DelegationEnded`/`DelegationReviewRequested` to | *(required)* |
| `CASCADE_QUEUE_URL` | `delegation-cascade-q` (inbound `MembershipRevoked`/`TenantMembershipsPurged` cascade) | *(required)* |
| `GLUE_REGISTRY_NAME` | Set to `iam-delegation-events` to activate `GlueCodec` on the outbound publish path; empty uses `NoopCodec` | `""` |
| `POLICY_DEFAULT_MAX_DURATION_DAYS` / `POLICY_DEFAULT_REVIEW_WINDOW_DAYS` | Fallback tenant policy (DLG-D2) when a tenant has no `delegation_tenant_settings` row | `90` / `90` |
| `DOCS_ENABLED` | Opt-in to serving `/swagger` and `/asyncapi`/`/asyncapi.yaml` in production | `true` outside `production` |
| `DOCS_AUTH_TOKEN` | If set, requires `Authorization: Bearer <token>` on the docs routes in production | — |

See `.claude/operations.md`'s Configuration section for the complete, verified env-var table, including `cmd/reconciler`-only vars (`CRON_BATCH_LIMIT`/`DELEGATION_RETENTION_DAYS`).

## Testing

```sh
make test              # unit + integration + rls, in parallel (requires Docker)
make test-unit          # unit tests only (no Docker required)
make test-integration   # Postgres/Valkey/SQS-compatible via Testcontainers (requires Docker)
make test-rls           # canonical RLS suite (missing-GUC, cross-tenant read/write, FORCE RLS)
make test-e2e           # full HTTP-stack request flows (requires Docker)
make race               # all four suites with -race, in parallel
make test-ci            # race + merged coverage report (used in CI)
```

See `Makefile`'s `help` target (`make help`) for the complete command list, including
`build`/`build-server`/`build-reconciler`, `lint`, `arch-lint` (`.go-arch-lint.yml`, LLD §6.2),
`migrate-up`/`migrate-down`/`migrate-create`, and `docker-build`/`docker-push`.

## CI

Three workflows run on every push/PR to `main`/`master` (`.github/workflows/`):

| Workflow | What it does |
|---|---|
| `validate-test.yml` | Unit + integration + RLS suites with `-race` and coverage, merged into one `coverage.out`; gated on a **70%** statement-coverage floor (`.github/scripts/coverage-gate.sh`) — lower than some sibling services' gates, matching `iam-tender-acl`'s starting baseline. Also runs `go-arch-lint` against `.go-arch-lint.yml`. |
| `validate-quality.yml` | `gofmt`/`go mod tidy` check, `go vet`, `golangci-lint`, `govulncheck`, a Dockerfile base-image digest check, and two repo-specific greps: no HTML-escaped operators in workflow YAML, and no non-transaction-local `SET app.tenant_id` (RLS-6, enforced by `.github/scripts/check-forbidden-set-guc.sh`). |
| `ci.yml` | Orchestrates the two above in parallel with `build-image` — Hadolint, a multi-stage Docker build (`linux/amd64`), a Trivy CRITICAL/HIGH/UNKNOWN CVE scan (SARIF uploaded to the GitHub Security tab), smoke tests against both binaries, then push to GHCR and Cosign keyless-sign the image. |

`schema-registry.yml`, `schema-prune.yml`, `schema-health-quarterly.yml`, and
`freeze-watchdog.yml` separately govern the AWS Glue Schema Registry side of
`api/asyncapi.yaml`/`internal/eventschema/` — see
[`docs/runbook-schema-registry.md`](docs/runbook-schema-registry.md). `changelog-check.yml`
fails a PR that doesn't touch `CHANGELOG.md`'s `[Unreleased]` section.

**On a `v*` release tag:** `release.yml` builds standalone binaries for multiple
platforms, builds and re-scans the Docker image (CVE scan, CycloneDX SBOM, SLSA
provenance, Cosign signature), and publishes a GitHub Release with checksums attached.

## Docker

One multi-stage `Dockerfile` builds **both** binaries into a single distroless,
non-root image (`gcr.io/distroless/static-debian12:nonroot`) — `iam-delegation-server`
is the default `ENTRYPOINT`; each CronJob template
(`deploy/helm/iam-delegation/templates/cronjob-*.yaml`) overrides `command` to run
`/iam-delegation-reconciler --job=<name>` against that same image instead, so there's
one image reference to build, tag, scan, and sign — not two.

```sh
make docker-build   # docker build -t $(IMAGE) .   (IMAGE defaults to iam-delegation:local)
make docker-push    # docker-build, then docker push
```

Building requires a `go_private_token` build secret (a token with read access to the
`BCBP-SOLUTIONS-FZC-LLC` private Go modules) — see the Dockerfile's builder stage;
`docker build` alone without `--secret id=go_private_token,src=<token-file>` will fail
fast with a clear error rather than silently produce a broken image.

## Security

- **Tenant isolation** — three layers (LLD §13.1): Postgres RLS (`FORCE`, fail-closed `NULLIF`
  policy on both tables), `SET LOCAL app.tenant_id` per transaction via `platform-pgcommon`
  (never a session-scoped `SET` — CI greps for the forbidden form, `.github/scripts/
  check-forbidden-set-guc.sh`), and gateway-identity headers (`x-tenant-id`/`x-user-id`/
  `x-tenant-roles`) — never a request-body-derived tenant/actor.
- **Network isolation** (LLD §13.2) — public behind Envoy; `/internal/*` mesh-only mTLS via the
  internal-route guard; cross-service calls to `iam-org-membership`/`iam-user-profile` are
  intra-mesh mTLS.
- Every outbound client sits behind a swappable port interface (`MembershipCheckClient`,
  `UserProfileClient`); every event publish goes through the transactional outbox, atomic with the
  triggering DB write.

## Integrating with other services

This section is for **platform service authors** who need to call this service's public/internal
API or react to the events it publishes.

### 1. Prerequisites

Every caller must:

1. Run on the **internal service mesh** (mTLS) for `/internal/*` — public internet cannot reach
   those routes; they have no RBAC/JWT check of their own, only the network-layer trust boundary
   (LLD §13.2).
2. For the public `/api/v1/delegations*` routes, forward `x-user-id`, `x-tenant-id`, and
   `x-tenant-roles` headers — these are gateway-injected in production (Envoy `ext_authz` →
   AuthZ Enrichment, LLD §11.0) and read directly in local dev/tests.

### 2. HTTP client setup

This repo's own outbound clients (`internal/adapter/outbound/{userprofile,orgmembership}`) use
`gincommon.PropagateHeaders` to forward trace context and identity headers on every call —
mirror this pattern if you're calling `iam-delegation` from another Gin-based service:

```go
import (
    "encoding/json"
    "fmt"
    "net/http"

    "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
    "github.com/gin-gonic/gin"
)

func callDelegations(c *gin.Context, baseURL string) ([]DelegationResponse, error) {
    req, err := http.NewRequestWithContext(
        c.Request.Context(), http.MethodGet, baseURL+"/api/v1/delegations", nil)
    if err != nil {
        return nil, err
    }

    gincommon.PropagateHeaders(c, req) // traceparent, x-request-id, x-user-id, x-tenant-id, x-tenant-roles

    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        var errResp gincommon.ErrorResponse
        _ = json.NewDecoder(resp.Body).Decode(&errResp)
        return nil, fmt.Errorf("iam-delegation: %s (trace_id=%s)", errResp.Error, errResp.TraceID)
    }

    var out []DelegationResponse
    return out, json.NewDecoder(resp.Body).Decode(&out)
}
```

### 3. Creating a delegation (DLG-2)

```bash
POST /api/v1/delegations
x-user-id: <delegator's UUID>
x-tenant-id: <tenant UUID>
Idempotency-Key: <opaque, required — see Input validation above>
Content-Type: application/json

{
  "delegate_id": "9ac3...",
  "scope": "department",
  "scope_id": "engr-uuid",
  "starts_at": "2026-07-01T00:00:00Z",
  "ends_at": "2026-07-14T23:59:59Z",
  "ooo_note": "On annual leave — contact Carol for Engineering queries."
}
```

```json
// 201 Created
{
  "delegation_id": "del-uuid",
  "delegator_id": "2b1f...",
  "delegate_id": "9ac3...",
  "scope": "department",
  "scope_id": "engr-uuid",
  "starts_at": "2026-07-01T00:00:00Z",
  "ends_at": "2026-07-14T23:59:59Z",
  "status": "active",
  "record_version": 1
}
```

Ordering matters: both parties' tenant-membership is checked against `iam-org-membership`
concurrently, then `iam-user-profile`'s availability pointer is set, and only then is the row
written — see [`docs/architecture/mermaid/create-flow.mmd`](docs/architecture/mermaid/create-flow.mmd)
for the full sequence and every failure branch. A repeated `Idempotency-Key` within 24h returns
the original `201` rather than creating a duplicate.

### 4. Extend, reassign, and settings

```bash
POST /api/v1/delegations/:id/extend
{ "extend_days": 90 }   # optional; default = tenant review_window_days; [1,180]
# 200 OK
{ "delegation_id": "del-uuid", "review_due_at": "2026-11-08T00:00:00Z", "review_last_warned_bucket": null }
```

```bash
POST /api/v1/delegations/:id/reassign
{ "new_delegate_id": "7cd1...", "scope": "department", "scope_id": "engr-uuid", "ends_at": null, "reason": "..." }
# 201 Created — a NEW delegation row; the old one is ended (DelegationEnded{cancelled}), never mutated
```

```bash
PUT /api/v1/delegations/settings   # tenant_admin/tenant_owner only
{ "max_duration_days": 60, "review_window_days": 45 }
# 200 OK  { "max_duration_days": 60, "review_window_days": 45, "record_version": 2 }
```

### 5. The DLG-I3/I4 internal endpoints

Both are mesh-only and exist for **Core** (`iam-org-membership`), not for general use:

- `GET /internal/delegations/dept-delegate?tenant_id=&user_id=&dept_id=` — Core's user-removal
  flow calls this to find the active department-scope delegate before deciding whether a removal
  strands any in-flight workflow (LLD §11.5).
- `GET /internal/users/:id/active-delegations` — the escape hatch that replaces Core's old
  `active_delegations[]` I-8 embed (§6.1). **Nothing calls this today** — it exists so a future
  synchronous consumer doesn't have to reintroduce the embed.

### 6. Subscribing to `iam.delegation.events`

Three event types, all on one SNS topic (`iam.delegation.events`, wire-format details in
[`api/asyncapi.yaml`](api/asyncapi.yaml)). Per LLD §10.5, the real consumers today are:

| Event type | Emitted when | Consumers |
|---|---|---|
| `DelegationStarted` | DLG-2 create, and the create leg of DLG-5 reassign | **Workflow Service** (reroute), Notification, Audit |
| `DelegationEnded` | Cancel, `ends_at` expiry, review auto-end, the end leg of reassign, delegate-removed cascade | **Workflow Service** (restore), Notification, Audit |
| `DelegationReviewRequested` | The review sweep's 3-day daily cascade (once per calendar day at days_remaining ∈ {3,2,1}) | Notification, Audit |

`DelegationStarted`/`DelegationEnded` are the *authoritative* routing signal the Workflow Service
acts on — this is a different, stronger contract than a presentation-only availability change.
`ended_reason` on `DelegationEnded` is one of `expired` / `cancelled` / `delegate_removed` /
`review_expired` — react to it if your service's behavior differs by reason (e.g. Notification
probably wants a different message for `delegate_removed` than for `expired`).

**Consumers must be forward-compatible** (lenient JSON deserialization — do not
`DisallowUnknownFields`/use strict decoding) — see `api/asyncapi.yaml § x-forward-compatibility`
for the full per-language settings and the open-schema (`additionalProperties: true`) contract
this service's CI enforces on every schema change.

One asymmetry worth knowing: on a delegate-removed cascade, only the **delegate-side** row emits
`DelegationEnded` — the delegator-side row (if the removed user was the delegator) ends silently,
no event (DEL-7/DLG-EVT-4). Don't expect a 1:1 event per ended row on that path.

### 7. Error handling and optimistic locking

See [Input validation](#input-validation) above for the full error-code table and response shape.
All mutable responses include `record_version` — pass it back to detect concurrent writes; a
mismatch returns `409 optimistic_lock_conflict`. Cancel takes it as a query param
(`DELETE /delegations/:id?record_version=N`); extend and reassign take it in the JSON body
(`record_version` field).

### 8. Rate limits

None today — no per-endpoint rate limiter exists in this service (unlike some sibling IAM
services). Requests are subject only to whatever the API gateway/mesh enforces at that layer.

### 9. Background jobs you may observe

The three CronJobs described under [Binaries](#binaries) run independently of the HTTP path and
have externally-visible side effects worth knowing about: the `delegation-review` sweep emits
`DelegationReviewRequested` once per calendar day for each of the 3 days *before* an open-ended
delegation lapses (days_remaining ∈ {3,2,1}), then auto-ends it with `ended_reason: review_expired`
if nobody extends it in time; the
`delegation-expiry` sweep ends fixed-`ends_at` delegations the moment they lapse. Both defer and
retry (rather than fail) if `iam-user-profile` is unreachable when they try to clear the
availability pointer (DEL-6) — so a transient User Profile outage delays, but never loses, an
expected `DelegationEnded`.

## Out of scope

Per LLD §3 (Non-goals), this service does **not** handle:

| Concern | Where it lives |
|---|---|
| Workflow rerouting/restoration on delegation start/end | **Workflow Service** — this service only emits the event (DEL-4) |
| OOO presentation state (`user_availability`) | **User Profile** — this service only calls it (DEL-6); it's also the dashboard's source for a user's current delegate pointer |
| The delegate-impact *decision* on user removal (whether it strands a workflow) | **Core**'s `MembershipService.RemoveUser`, synchronously, against the Workflow Service — this service only ends rows afterward, asynchronously (§11.5) |
| Tenant membership management | **Core** (`iam-org-membership`) |
| Core's `GET /internal/users/:id/memberships` (I-8) | **Core** — under Option C it no longer references delegations at all; this service never serves I-8 (§6.1) |
| Future-dated delegation activation | Deferred to a v2 start-scheduler feature — v1 is active-at-create only (DLG-D12) |
| Rate limiting | API gateway / service mesh (this service has none of its own — see above) |

## Further reading

- [ARCHITECTURE.md](ARCHITECTURE.md) — package layout, sequence diagrams, and the LLD's
  DLG-D1..D20 decision register.
- [CHANGELOG.md](CHANGELOG.md) — notable changes, starting with the initial extraction.
- [`docs/lld/iam-lld-delegation-service.md`](docs/lld/iam-lld-delegation-service.md) — the full
  Low-Level Design this service implements.
- [`docs/architecture/README.md`](docs/architecture/README.md) — index of the standalone Mermaid
  diagram sources `ARCHITECTURE.md` embeds.
- [`docs/runbook-schema-registry.md`](docs/runbook-schema-registry.md) — operational runbook for
  the AWS Glue Schema Registry integration (startup contract, failure modes, adding a new event
  type).
- [`ARCHITECTURE.md`](ARCHITECTURE.md)'s "Session-specific decisions" section — every as-built
  deviation (DLG-D13 onward) not in the original LLD.
- Sibling IAM services with the same Clean Architecture layout: `iam-org-membership` (Core —
  the service this was extracted from), `iam-user-profile`, `iam-catalog-admin`,
  `iam-group-mapping`, `iam-tender-acl`.

## License / ownership

BCBP Solutions FZC LLC — internal platform service. Not for external distribution.
