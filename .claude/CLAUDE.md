# CLAUDE.md

This file provides guidance to Claude Code when working with the **Delegation Service** (`iam-delegation`), a Go microservice in the IAM subsystem.

## What This Repo Is

`iam-delegation` is a **private Go service** (module: `github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation`, Go 1.26.6 — pinned exactly, DLG-D16) that owns out-of-office delegation: who delegated their work to whom, over what scope (all work / one department / one tender), for how long, and why it ended (expired, cancelled, the delegate was removed, the delegate was disabled, or the review window lapsed). It is the fourth and last O&M extraction from `iam-org-membership` (ADR-0008, Option C) — not yet deployed to any live environment, and no Git tag has ever been pushed (see `VERSIONING.md`).

**Key responsibilities:**
- The `delegations` and `delegation_tenant_settings` tables (per-tenant policy defaults)
- The DLG-1..7 public API (list/create/cancel/extend/reassign/get-settings/set-settings) and DLG-I1..I4 mesh-only internal API
- Two synchronous outbound dependencies of its own: grant-time membership-existence checks against `iam-org-membership` (DLG-D3), and availability-first pointer set/clear against `iam-user-profile` (DEL-6)
- One inbound async subscription, `delegation-cascade-q` — two upstream SNS topics: Core's `MembershipRevoked`/`TenantMembershipsPurged` (`iam.membership.events`) and User Profile's `UserUpdated{status:disabled}` (`iam.user.events`, DLG-D26 — pre-deploy gap, subscription not yet provisioned in any environment)
- Its own outbound event topic, `iam.delegation.events` (`DelegationStarted`/`DelegationEnded`/`DelegationReviewRequested`/`DelegationEscalationRequested`, DLG-D27)

This service is **both a producer and a consumer** — the one structural difference from most of its O&M extraction siblings.

**Does NOT own:** tenant membership (Core/`iam-org-membership`), the synchronous user-removal gate (stays in Core, DLG-D9), availability state itself (`iam-user-profile` — this service only sets/clears a pointer), Core's I-8 endpoint (Option C dropped `active_delegations[]` from it entirely — nothing here feeds it back). **No longer true as of DLG-D25:** future-dated/scheduled delegation activation was originally out of v1 scope (DLG-D12); a `scheduled` status and the `delegation-activation` CronJob now implement it.

**Source of truth for:** the `delegations`/`delegation_tenant_settings` tables; `iam.delegation.events` (consumed by Workflow Service for reassignment, per LLD §10.5); `GET /internal/users/:id/active-delegations` (DLG-I4 — the escape-hatch replacement for Core's removed `active_delegations[]` embed; unused today).

## Common Commands

```bash
make setup            # copy .env.example to .env if missing, install git hooks (.githooks/pre-commit)
make install-hooks    # install .githooks/pre-commit into .git/hooks
make tidy             # go mod tidy
make fmt              # format source with gofmt
make fmt-check        # verify gofmt formatting (mirrors CI)
make vet              # go vet (default build + every test build tag)
make lint             # run golangci-lint (via go tool)
make mod-verify       # go mod verify
make vuln-check       # govulncheck on cmd/ + internal/ + pkg/
make test             # unit + integration + rls tests, in parallel (requires Docker)
make test-unit        # unit tests only (no Docker required)
make test-integration # integration tests: Postgres+Valkey+SQS-compatible via Testcontainers (requires Docker)
make test-rls         # Postgres Row-Level-Security tests via Testcontainers (requires Docker)
make test-e2e         # end-to-end tests: Postgres+Valkey+SQS-compatible via Testcontainers (requires Docker)
make race             # all four suites with -race, in parallel (requires Docker)
make test-ci          # race + coverage, merged into coverage.out (used in CI, requires Docker)
make cover            # coverage HTML report
make cover-func       # coverage summary by function
make run / run-server # run the server locally (go run cmd/server), sourcing .env if present
make run-reconciler   # run the reconciler locally; pass JOB=delegation-activation|delegation-expiry|delegation-review|delegation-cleanup
make build            # compile both binaries (iam-delegation-server, iam-delegation-reconciler) to bin/
make build-server     # compile only cmd/server
make build-reconciler # compile only cmd/reconciler
make arch-lint        # run go-arch-lint against .go-arch-lint.yml
make swag             # regenerate docs/swagger/ from handler annotations
make swag-check       # fail if Swagger regeneration would change docs/swagger/ (CI drift gate)
make ci               # tidy + fmt-check + vet + lint + arch-lint + test-ci + build
make docker-build     # build the container image (carries both binaries)
make docker-push      # push the container image
make docker-up        # start local Postgres + Valkey + floci (SNS/SQS/Glue) + floci-ui
make docker-down      # stop containers started by docker-up/compose-up
make compose-up       # start the full local dev stack, including the service itself (self-migrates at startup)
make compose-down     # stop and remove the local dev stack, including volumes
make migrate-up/-down/-create  # manual migration ops against DATABASE_MIGRATION_URL
make godoc            # serve local godoc/pkgsite at http://localhost:8080
make clean            # remove build artifacts and coverage output

# Schema governance (platform-schemagov 0.4, DLG-D20)
make schema-pull      # pull the schema-gov Docker image
make schema-validate  # validate AsyncAPI + event schemas — 8 passes (no AWS required)
make schema-diff      # diff two schema files: CURRENT=<path> PROPOSED=<path>
make schema-register  # register event schemas to Glue (requires AWS/floci)
make schema-verify    # pre-deploy check: fail if PascalCase schemas are missing (requires AWS)
make schema-prune     # dry-run: list orphaned Glue schemas (requires AWS)
```

To run a single test:
```bash
go test ./internal/core/service/... -run TestDelegationService_Create -v
go test -tags=rls ./internal/adapter/outbound/postgres/... -run TestRLS -v
```

**Test layout note:** unlike some sibling services, every test in this repo is colocated white-box (`*_test.go` next to the source it covers, package-internal) — there is no separate `test/` tree. `go test ./...` alone runs the complete unit/integration/RLS suite (unit + Postgres/testcontainer integration + the full RLS matrix all together); `-tags=integration|rls` still select no additional files (no test declares those two build tags — they're no-ops kept for future extensibility) — but `-tags=e2e` now does: `cmd/server/e2e_test.go` (DLG-D41, LLD §17.4) boots the real composition root against real Postgres/Valkey/floci containers, so `make test-e2e` genuinely exercises the full HTTP/SQS stack rather than silently re-running the unit suite a second time. That file is excluded from `test-unit`/`test-ci`/`race` (none of those pass `-tags=e2e`), so it never affects the coverage gate.

**Coverage note:** measure with `-coverpkg=$(go list ./internal/... ./pkg/... | tr '\n' ',')` — `make cover`/`make cover-func` already do this. CI enforces a single global statement-coverage gate of ≥95% on the merged `coverage.out` (`.github/scripts/coverage-gate.sh`, bumped from 70% during the DLG-D34 production-readiness sweep — global coverage sat at 95.2% as of that pass, 95.4% as of the DLG-D35 follow-up sweep) — see `CONTRIBUTING.md` § Coverage gate for current per-package numbers.

**Testcontainers note:** Postgres/Valkey/SQS-compatible integration and RLS tests spin up real containers via `testcontainers-go`. Docker must be running.

**Tool directive note:** `golangci-lint` and `swag` are declared as `tool` entries in `go.mod` and invoked via `go tool <name>`. They do not need to be installed separately.

## Architecture

Clean Architecture — dependencies point inward; outer layers never import inner layers.

```
iam-delegation/
├── api/
│   ├── asyncapi.yaml                  # AsyncAPI 3.0 spec — iam.delegation.events + delegation-cascade-q's two consumed types; x-lifecycle/x-owner/x-forward-compatibility/x-semantic-contract/x-version-governance/x-usage-override governance annotations (DLG-D20)
│   └── embed.go                       # `//go:embed asyncapi.yaml` → AsyncAPISpec []byte — GET /asyncapi(.yaml) serve this, not a disk read
├── cmd/
│   ├── server/
│   │   ├── main.go                    # composition root — errgroup runs six real background workers (see "Key Files to Know")
│   │   ├── adapters.go                # gucBoundReader (DLG-I3/I4 GUC binding) + reconcilerRunner (DLG-D17 dual entry point) + redisPinger
│   │   ├── config.go                  # loadConfig() — env var parsing, SNS_TOPIC_ARN/CASCADE_QUEUE_URL fail-fast checks; SYSTEM_DATABASE_URL required outside local/dev ENVIRONMENT (DLG-D34/D35); DOCS_AUTH_TOKEN required when DOCS_ENABLED=true in production (DLG-D40); ensureOutboxEnv backfills historical OUTBOX_* defaults (DLG-D46)
│   │   ├── wiring.go                  # buildSNSPublisher + cascadeSQSEnv — platform-events config.LoadSNS/LoadSQS/LoadOutbox composition (DLG-D44), with the pgx.Tx type assertion kept out of core/port (DLG-D46)
│   │   ├── exporters.go               # runActiveGaugeExporter — 5-min BYPASSRLS sysPool snapshot of iam_delegation_active_gauge
│   │   ├── e2e_test.go                # //go:build e2e — LLD §17.4 suite against real Postgres/Valkey/floci containers (DLG-D41)
│   │   └── swagger_info.go            # swaggo metadata
│   └── reconciler/
│       ├── main.go                    # --job=<name> dispatch (this chart's own convention, not shelling out over HTTP)
│       ├── config.go
│       └── jobs/
│           ├── context.go             # jobs.Context — shared deps for all four jobs
│           ├── delegation_activation.go # DLG-D25 (*/5 * * * *, promotes scheduled -> active)
│           ├── delegation_expiry.go   # DLG-I1 (*/5 * * * *, LLD §11.3)
│           ├── delegation_review.go   # DLG-I2 (0 * * * *, 3-day daily cascade warn, LLD §11.4)
│           └── delegation_cleanup.go  # soft-delete purge (0 4 1 * *, LLD §11.6/§18.4)
├── internal/
│   ├── core/
│   │   ├── domain/
│   │   │   ├── delegation.go          # Delegation, enums (Scope, Status, EndReason incl. review_expired)
│   │   │   ├── tenant_settings.go     # DelegationTenantSettings (policy)
│   │   │   ├── event.go               # DomainEvent + published/consumed event-type constants — see "Key Files to Know"
│   │   │   ├── event_payloads.go      # per-event payload structs
│   │   │   └── errors.go              # domain.Err* sentinels — the full LLD §20 taxonomy (see development-guide.md's Appendix)
│   │   ├── port/                      # logger.go (Zap-backed port.Logger via gincommon logger.NewLogger, DLG-D42) · delegation_repository.go · settings_repository.go · user_profile_client.go · membership_check_client.go · tender_scope_client.go (§7.6.7, DLG-D13 — unwired pending Tender's endpoint) · idempotency_store.go · event_publisher.go · cache.go · tx_runner.go (WithTx/TxFromContext carry the tx as `any` — core imports no pgx, DLG-D46) · errors.go · doc.go
│   │   └── service/
│   │       ├── delegation_service.go  # DLG-1..5 orchestration
│   │       ├── settings_service.go    # DLG-6/7
│   │       └── cascade_service.go     # delegation-cascade-q business logic (MembershipRevoked/TenantMembershipsPurged handling, + EndForDisabledDelegate for UserUpdated{status:disabled}, DLG-D26/D27)
│   └── adapter/
│       ├── inbound/
│       │   ├── http/                  # router.go · delegation_handler.go · settings_handler.go · internal_handler.go · asyncapi.go · health.go · authz.go · middleware.go (incl. tenantGUCMiddleware, requireIdempotencyKey) · dto.go · errors.go (errorStatusByCode)
│       │   └── consumer/              # cascade_consumer.go · wiring.go — delegation-cascade-q
│       └── outbound/
│           ├── postgres/              # db.go (TxRunner, WithTenantGUC) · delegation_repository.go · settings_repository.go · processed_events.go · gauge_repository.go · otel_tracer.go · migrate.go · migrations_fs.go · migrations/ (000001_schema only, as of this build)
│           ├── userprofile/           # http_client.go (UserProfileClient impl, DEL-6) + propagate.go
│           ├── orgmembership/         # http_client.go (MembershipCheckClient impl, DLG-D3) + propagate.go
│           ├── tender/                # http_client.go (TenderScopeClient impl, §7.6.7, DLG-D13) + propagate.go — built/tested, not constructed anywhere in cmd/server
│           ├── eventbus/              # publisher.go + validating_codec.go (enqueue) · codec.go (GlueCodec encode + GlueDecodeCodec consume-side decode, DLG-D21) · validator.go (SchemaValidator, tests)
│           ├── valkey/                # cache.go · client.go · idempotency.go — del: cache + idempotency store
│           └── metrics/               # metrics.go — iam_delegation_* Prometheus instruments; DLG-D19 is closed — every instrument has a real call site (internal/core/service/metrics.go's injected Metrics port, cmd/reconciler/jobs.Context.Metrics, or cmd/server/exporters.go's active-gauge exporter)
├── internal/eventschema/              # delegation_{started,ended,review_requested,escalation_requested}.json + schemas.go (//go:embed) — hand-maintained, no extract-schemas step (DLG-D20)
├── pkg/requestctx/                    # gateway-identity / tenant-actor extraction helpers
├── docs/
│   ├── lld/iam-lld-delegation-service.md  # the full LLD, current rev 2.18 (design-time source of truth)
│   ├── architecture/                  # README.md (index) + mermaid/*.mmd — 13 diagrams (layer model, package deps, ER, RLS/GUC flow, 9 request/cron/cascade flows)
│   ├── runbook-schema-registry.md     # operator runbook for the Glue registry (DLG-D20)
│   └── swagger/                       # generated by `make swag` — checked in
├── deploy/
│   ├── helm/iam-delegation/           # this service's independent Helm chart
│   ├── iam/                           # policy.json (incl. GlueSchemaRegistryReadOnly SID) + policy.tf.example
│   └── monitoring/                    # app-alerts.yml (threshold alerts) + slo-rules.yml (SLO-1..4 burn-rate, DLG-D39) — both also rendered by templates/prometheusrule.yaml — + prometheus-adapter-rule.yaml + schema-registry-alerts.yml (CI schema pipeline)
├── .github/workflows/                 # ci.yml · validate-quality.yml · validate-test.yml · release.yml · changelog-check.yml · schema-registry.yml · schema-prune.yml · schema-health-quarterly.yml · freeze-watchdog.yml
├── .githooks/pre-commit               # tidy + fmt-check + lint + swag-check
├── Dockerfile  docker-compose.yml  Makefile  go.mod  .golangci.yml  .go-arch-lint.yml
├── ARCHITECTURE.md                    # detailed architecture narrative with Mermaid diagrams; includes the DLG-D13+ as-built decision register
├── CONTRIBUTING.md                    # dev setup, extension playbooks, PR checklist
├── VERSIONING.md                      # SemVer policy, runtime-contract scope, release process, compatibility matrix
└── README.md                          # onboarding + quick-start (~450 lines)
```

### Shared library dependencies

```go
require (
    github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events     v1.4.0
    github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon  v1.3.0
    github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon   v1.3.0
)
```

- `platform-gincommon` — HTTP middleware, logging, tracing (OTel OTLP), Prometheus registerer
- `platform-pgcommon` — PostgreSQL pool (`pgx/v5`), RLS GUC injection (`GUCSetFromContext`/`WithGUCSet`), migrations
- `platform-events` — transactional outbox, SNS publisher (`events.WithCodec`), SQS consumer

Also notable: `github.com/aws/aws-sdk-go-v2/service/glue` (GlueCodec's schema-version lookups) and `github.com/santhosh-tekuri/jsonschema/v6` (SchemaValidator).

### Dependency rules (enforced in CI via `go-arch-lint`, `.go-arch-lint.yml`)

- `internal/core/*` imports no adapter, Gin, pgx, or AWS SDK.
- `core/port` depends only on `core/domain`; `core/service` depends on `domain`+`port`+`requestctx`.
- Inbound adapters depend on `service`+`port`+`domain` — never an outbound adapter directly.
- Outbound adapters depend on `port`+`domain`+`eventschema` — never `service`, never inbound.
- `cmd/*` is the only place concretes get wired together.
- No session-scoped `SET app.tenant_id` — only `SET LOCAL` via `pgcommon.GUCSetFromContext` (CI greps the forbidden form, `.github/scripts/check-forbidden-set-guc.sh`, RLS-6).
- Events/outbox pass through `platform-events` only — no direct AWS SDK SNS/SQS client calls or hand-built `events.Envelope` struct literals outside it (CI greps for both, `.github/scripts/check-forbidden-events-bypass.sh`). Consumer-side dedup (`processed_events`) is the one deliberate exception — `platform-events` has no consumer-side idempotency mechanism of its own, only the publish-side, SNS-FIFO-only `WithMessageDeduplicationID`.

## Key Files to Know

- **`cmd/server/main.go`** — composition root. **Six** real background goroutines run under one `errgroup`: the outbox runner, the `delegation-cascade-q` SQS consumer, the `iam_delegation_active_gauge` exporter (`runActiveGaugeExporter` in `cmd/server/exporters.go` — a 5-minute BYPASSRLS `sysPool` snapshot via `pgadapter.NewGaugeRepository`, since no request or reconciler path can otherwise keep a point-in-time gauge current), a daily outbox-prune sweep (`outboxRunner.PrunePublished`, DLG-D24 — matching `iam-user-profile`'s `runMaintenanceSweep`; without it `outbox_events` grows unbounded, since published rows are never deleted automatically), the HTTP API server, and a dedicated `:METRICS_PORT` metrics server (split from the API listener so a NetworkPolicy can grant scrape access without also granting API access) — plus a graceful-shutdown goroutine and `GlueCodec.StartRefresher`'s internal ticker. DLG-D19's observability gap is closed: every registered `iam_delegation_*` instrument now has a real call site — the deferred/warned/expired reconciler counters (GAP-27/DLG-D25) via `cmd/reconciler/jobs.Context.Metrics`, the service-layer counters via `internal/core/service/metrics.go`'s injected `Metrics` port, and the previously-dead `active_gauge` via the exporter above.
- **`cmd/server/adapters.go`** — two adapters unique to this service's topology: `gucBoundReader` binds `app.tenant_id` per-call for the mesh-only DLG-I3/I4 reads (no per-request middleware on that route group); `reconcilerRunner` adapts `cmd/reconciler/jobs`' `Expiry`/`ReviewSweep` functions to the HTTP handler's injected runner interfaces so DLG-I1/I2's on-demand HTTP endpoints and the CronJob binary share one implementation (DLG-D17).
- **`cmd/server/wiring.go`** (DLG-D44/D46) — `buildSNSPublisher` and `cascadeSQSEnv`, the two small helpers that adapt this service's own env-var names (`SNS_TOPIC_ARN`, `CASCADE_QUEUE_URL`, `CASCADE_SQS_CONCURRENCY`) onto `platform-events/pkg/config`'s `LoadSNS`/`LoadSQS` composition contract (the library's own env-var names, `SNS_TOPIC_ARN`/`SQS_QUEUE_URL`/`SQS_CONCURRENCY`, don't match this service's history, so `main.go` can't call `LoadSQS()` unmodified for the cascade queue). `main.go`'s outbox wiring additionally goes through `ensureOutboxEnv` (`config.go`) before `config.LoadOutbox()`, since that library's own defaults (`5s` poll, concurrency 1) differ from this service's historical ones.
- **`cmd/server/e2e_test.go`** (`//go:build e2e`, DLG-D41) — the LLD §17.4 end-to-end suite. Boots the real `run(ctx, logger)` composition root against real Postgres/Valkey/floci (SNS/SQS-compatible) containers, with User Profile/Org Membership faked via local `httptest.Server`s. Excluded from `test-unit`/`test-ci`/`race` (none pass `-tags=e2e`) — see the Test layout note above.
- **`internal/core/domain/event.go`** — `EventDelegationStarted`/`EventDelegationEnded`/`EventDelegationReviewRequested`/`EventDelegationEscalationRequested` (DLG-D27 — fired by `CascadeService.EndForDisabledDelegate` alongside `DelegationEnded`, notify-only, escalates to `tenant_admin`/`tenant_owner`) constants. Unlike `iam-user-profile`'s `domain.GlueSchemaName` translation switch, **these constants ARE the PascalCase Glue schema names directly** — no dot-notation-to-PascalCase mapping exists or is needed here.
- **`internal/core/service/delegation_service.go`** — DLG-1..5 orchestration: the availability-first create ordering (membership checks → User Profile → `RunInTx`), fail-open cancel, self-retrying expiry, 3-day daily-cascade review warnings.
- **`internal/core/service/cascade_service.go`** — `EndForUser` (MembershipRevoked → end every delegation where the user is delegator or delegate; delegate-side only emits an event, delegator-side is silent per DLG-EVT-4) and `ScrubTenant` (TenantMembershipsPurged → soft-delete the tenant's rows).
- **`internal/adapter/inbound/http/router.go`** — route registration; `tenantGUCMiddleware` on the public group (right after `ContextMiddleware`); `requireIdempotencyKey()` gates `POST /delegations` with a 400, not a service-layer check.
- **`internal/adapter/inbound/http/errors.go`** — `errorStatusByCode`, the map from every `domain.Err*` sentinel to its HTTP status (LLD §20 verbatim). A code missing from this map falls back to 500.
- **`internal/adapter/outbound/eventbus/publisher.go`** — `Publisher` implements `port.EventPublisher` and takes `events.Codec`; `ValidatingCodec` wraps `events.NoopCodec` and validates at enqueue (missing schema = pass-through). Glue stays on the SNS `events.WithCodec` path. There is no local enqueue `Codec`/`NoopCodec`.
- **`internal/adapter/outbound/postgres/db.go`** — `TxRunner` injects `port.EventPublisher` into ctx; `WithTenantGUC` binds the GUC for reconciler jobs/cascade consumer.
- **`api/asyncapi.yaml`** — hand-maintained (no `extract-schemas` step, DLG-D20); carries the `x-lifecycle`/`x-owner`/`x-forward-compatibility`/`x-semantic-contract`/`x-version-governance`/`x-usage-override` governance annotations `schema-gov validate` requires.
- **`api/embed.go`** — `//go:embed asyncapi.yaml` → `AsyncAPISpec []byte`, served by `GET /asyncapi`/`GET /asyncapi.yaml` without a disk read.

## See Also

Detailed reference docs in `.claude/`:
- [Database Schema](database.md) — tables, RLS, pool config, migrations, triggers
- [API, Caching & Events](api-and-events.md) — endpoints, cache keys, event types, Glue wire format
- [Request Flows & Concurrency](flows-and-concurrency.md) — create/cancel/cron/cascade flows, optimistic locking, shutdown ordering
- [Operations](operations.md) — security, observability (DLG-D19's metrics gap is closed — see the `iam_delegation_*` table there for each instrument's call site), configuration, CI/CD, schema governance (`platform-schemagov`/`schema-gov` CLI rules — read before touching `.github/workflows/schema-registry.yml` or any Glue/event-schema-validation file)
- [Development Guide](development-guide.md) — design decisions, extending, workflow, troubleshooting, error codes

Supplementary docs in the repo root and `docs/`:
- **`README.md`** — onboarding, quick-start, local dev setup, integrating with other services
- **`ARCHITECTURE.md`** — detailed architecture narrative with Mermaid diagrams; its "Session-specific decisions" section is the DLG-D13+ as-built decision register
- **`CONTRIBUTING.md`** — dev setup, extension playbooks, PR checklist
- **`VERSIONING.md`** — SemVer policy, runtime-contract scope, release process, compatibility matrix
- **`docs/lld/iam-lld-delegation-service.md`** — the full Low-Level Design (v2.13) this service implements
