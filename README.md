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
(fed by Core's `MembershipRevoked` and `TenantOffboarded`, §11.5/§11.6). It publishes its own events
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

## Binaries

This repo ships **two** binaries (`cmd/server`, `cmd/reconciler` — LLD §16.1), each with its own
Dockerfile and Helm workload:

| Binary | Image | What it does |
|---|---|---|
| `cmd/server` | `iam-delegation-server` (`Dockerfile.server`) | HTTP API (DLG-1..7, DLG-I1..I4) **and** the `delegation-cascade-q` SQS consumer, run in-process via `errgroup` — one Deployment, ports 8080 (HTTP) / 9090 (metrics). |
| `cmd/reconciler` | `iam-delegation-reconciler` (`Dockerfile.reconciler`) | The three CronJob entry points under `cmd/reconciler/jobs/`, dispatched at invocation time by a `--job=<name>` flag: `delegation-expiry` (`*/5 * * * *`, DLG-I1), `delegation-review` (`0 * * * *`, DLG-I2), `delegation-cleanup` (`0 4 1 * *`, soft-delete purge). No HTTP/metrics server — three separate `CronJob` resources, no ports exposed. |

## API overview

| ID | Method & path | Auth | Notes |
|---|---|---|---|
| DLG-1 | `GET /api/v1/delegations` | self (gateway identity) | List active delegations for the caller. Cached (`del:list`, advisory — correctness never depends on it). |
| DLG-2 | `POST /api/v1/delegations` | self = delegator | Create. Requires `Idempotency-Key` (24h dedup, DLG-D8). Runs two concurrent grant-time membership-existence checks against `iam-org-membership` (DLG-D3) — fails `422`/`503` if either check fails or reports inactive. |
| DLG-3 | `DELETE /api/v1/delegations/{id}` | self or admin | Cancel. Pointer-clear against `iam-user-profile` is fail-open (DEL-6) — cancellation always succeeds even if the availability pointer-clear call fails. |
| DLG-4 | `POST /api/v1/delegations/{id}/extend` | self or admin | Extend the review window (`extend_days` in `[1,180]`). Resets the dual-warning bucket (DLG-D7). |
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
| `DATABASE_URL` | `delegation_app` (RLS-bound) connection string | *(required)* |
| `MIGRATION_DATABASE_URL` | `delegation_migrator` (BYPASSRLS) connection string for the startup migration run (`postgres.Migrate`, LLD §7.4/§16.4) | falls back to `DATABASE_URL` |
| `USER_PROFILE_BASE_URL` | `iam-user-profile`'s base URL, consulted for DEL-6 availability-first pointer set/clear | `http://iam-user-profile.iam.svc.cluster.local` |
| `ORG_MEMBERSHIP_BASE_URL` | `iam-org-membership`'s base URL, consulted for the two DLG-D3 grant-time membership checks | `http://iam-org-membership.iam.svc.cluster.local` |
| `VALKEY_ADDR` | `del:` keyspace — DLG-1 list cache + create-idempotency-key store (DLG-Q3) | `localhost:6379` |
| `SQS_QUEUE_URL` | `delegation-cascade-q` (inbound `MembershipRevoked`/`TenantOffboarded` cascade) | *(required)* |
| `SQS_CONCURRENCY` | `delegation-cascade-q`'s consumer concurrency | `4` |
| `EVENTS_TOPIC` / `EVENTS_SOURCE` | Outbound topic (`iam.delegation.events`) the outbox publishes `DelegationStarted`/`DelegationEnded`/`DelegationReviewRequested` to | `iam-delegation-events` / `iam-delegation` |
| `POLICY_DEFAULT_MAX_DURATION_DAYS` / `POLICY_DEFAULT_REVIEW_WINDOW_DAYS` | Fallback tenant policy (DLG-D2) when a tenant has no `delegation_tenant_settings` row | `90` / `90` |
| `REVIEW_WARN_EARLY_DAYS` / `REVIEW_WARN_LATE_DAYS` | Dual review-warning bucket (DLG-D7/DLG-Q6) | `7` / `3` |
| `DOCS_ENABLED` | Opt-in to serving `/swagger` and `/asyncapi`/`/asyncapi.yaml` in production | `false` |
| `DOCS_AUTH_TOKEN` | If set, requires `Authorization: Bearer <token>` on the docs routes in production | — |

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

## Further reading

- [ARCHITECTURE.md](ARCHITECTURE.md) — package layout and the LLD's DLG-D1..D12 decision register.
- [CHANGELOG.md](CHANGELOG.md) — notable changes, starting with the initial extraction.
- `docs/IAM_service/iam-lld-delegation-service.md` — the full Low-Level Design this service
  implements.
