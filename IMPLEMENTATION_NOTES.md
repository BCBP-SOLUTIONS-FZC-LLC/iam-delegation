# Implementation Notes

Maps `iam-lld-delegation-service.md` v2.1 and ADR-0008 v2 (`02-hld-delta-delegation.md`)
to where each is realized in this repo, and records every decision made during
this build that isn't already in the LLD's own §23 Decision Register (which
this file extends as DLG-D13…D17).

## LLD section → code map

| LLD section | Subject | Code |
|---|---|---|
| §6 (package layout) | Clean Architecture tree | Matches the LLD's tree exactly; see `ARCHITECTURE.md` |
| §6.1 (Option C) | This service never feeds I-8 | No I-8 client/consumer anywhere in this repo — confirmed by absence, not by a stub |
| §7.1 (enums) | `delegation_scope`, `delegation_status` | `internal/adapter/outbound/postgres/migrations/000001_schema.up.sql`; Go mirror in `internal/core/domain/delegation.go` |
| §7.2.1 (`delegations`) | Table, CHECKs, 5 partial indexes, `review_last_warned_bucket` | `migrations/000001_schema.up.sql`; repository in `internal/adapter/outbound/postgres/delegation_repository.go` |
| §7.2.2 (`delegation_tenant_settings`) | Per-tenant policy | Same migration; `internal/adapter/outbound/postgres/settings_repository.go`; service `internal/core/service/settings_service.go` |
| §7.2.3 (`processed_events`) | Cascade consumer dedup ledger | `internal/adapter/inbound/consumer/processed_events.go` (consumer names `cascade`/`offboarding` per the LLD's two-value enum) |
| §7.3 (RLS) | Three-function fail-closed design | `app_tenant_id()` / `rls_check_tenant()` / `log_rls_violation()` / `rls_violation_log` in `migrations/000001_schema.up.sql`, mirrored from `iam-user-profile`'s pattern (not `iam-tender-acl`'s deliberately-plain policy) |
| §7.5 (`touch_row()`) | TRG-1…3 | Same migration |
| §7.6 (lost composite FKs) | Two grant-time checks (DLG-D3) | `internal/core/port/membership_check_client.go` (port), `internal/adapter/outbound/orgmembership/http_client.go` (adapter), called concurrently in `DelegationService.checkBothMemberships` (`internal/core/service/delegation_service.go`) |
| §8 (API) | DLG-1…7, DLG-I1…I4 | `internal/adapter/inbound/http/{router,delegation_handler,settings_handler,internal_handler}.go` |
| §9 (caching) | `del:list`/`del:idem` | `internal/adapter/outbound/valkey/{cache,idempotency}.go` |
| §10 (events) | Envelope, publish, consume | `internal/adapter/outbound/eventbus/{codec,validator}.go` (enqueue-time validation / publish-time Glue codec split), `internal/adapter/outbound/postgres/db.go`'s `txBoundPublisher` (outbox enqueue, DLG-EVT-1), `internal/adapter/inbound/consumer/cascade_consumer.go` (inbound), `api/asyncapi.yaml` (contract, extended per §10.1 with the two consumed messages) |
| §11 (flows) | All sequence diagrams | `internal/core/service/delegation_service.go` (§11.1/§11.2/§11.7), `cmd/reconciler/jobs/{delegation_expiry,delegation_review}.go` (§11.3/§11.4), `internal/core/service/cascade_service.go` (§11.5/§11.6) |
| §12 (concurrency) | Optimistic lock, idempotency | `postgres.DelegationRepository.{End,ExtendReview,MarkReviewWarned}`'s 404-vs-409 probe; `DelegationService.Create`'s idempotency-key check |
| §13 (security) | RLS + GUC + gateway identity | `internal/adapter/inbound/http/router.go`'s `tenantGUCMiddleware`/`BindTenantGUC` seam (see DLG-D18 below — a build-time addition the LLD didn't spell out at the wiring-signature level) |
| §14 (observability) | Metrics | `internal/adapter/outbound/metrics/metrics.go`; registered in both `cmd/server/main.go` and available to `cmd/reconciler` (see DLG-D19 gap below) |
| §15 (config) | Env surface | `cmd/server/config.go`, `cmd/reconciler/config.go`, `deploy/helm/iam-delegation/values.yaml` |
| §16 (deploy) | Two binaries, three CronJobs | `cmd/server/`, `cmd/reconciler/`, `deploy/helm/iam-delegation/templates/{deployment,cronjob-*}.yaml` |
| §17 (testing) | Unit/integration/RLS matrix | See "Test coverage" below |
| §18 (GDPR) | Soft-delete → 90d purge | `cmd/reconciler/jobs/delegation_cleanup.go`, `DelegationRepository.HardPurgeSoftDeletedBefore` |
| §20 (errors) | Taxonomy | `internal/core/domain/errors.go` + `internal/adapter/inbound/http/errors.go` (mapper) |

## Decision register — DLG-D13…D19

Extends the LLD's own §23 (DLG-D1…D12), which this build implements as
specified. These are choices made *during* this build that the LLD didn't
(and, for D13/D14/D15, couldn't) specify:

| # | Decision |
|---|---|
| DLG-D13 | AuthZ Enrichment's `active_delegations[]`-driven department-level elevation (`policy.departmentLevel()`) is removed outright in PR4, not just the dead field — research found ADR-0008's "no policy reads it" claim was false (that method DID fold delegations into an authz decision). Full Option-C behavior was chosen over reintroducing a hot-path escape-hatch call. User-confirmed. |
| DLG-D14 | `MembershipRevoked{tenant_id,user_id,actor_id}` and `TenantOffboarded{tenant_id,actor_id}` are wholly new Core (`iam-org-membership`) event types (DLG-Q4) — Core had neither before this build (only `TenantMembershipRemoved`, tender-ACL's cascade signal). Built from scratch in PR3, not discovered as pre-existing wiring. |
| DLG-D15 | Event-schema governance uses in-house `santhosh-tekuri/jsonschema/v6` validation (mirroring `iam-user-profile`'s proven pattern) instead of `platform-schemagov`, which no sibling service actually depends on or exercises. `internal/adapter/outbound/eventbus/validator.go`. |
| DLG-D16 | Go toolchain pinned to `1.26.6` (matching every touched repo's actual toolchain) rather than the LLD's stated "Go 1.23", which is stale relative to the platform's real toolchain as of this build. |
| DLG-D17 | DLG-I1/I2 (`/internal/delegations/expire`, `/internal/delegations/review-sweep`) are real HTTP handlers on `cmd/server` **and** `cmd/reconciler`'s CronJobs call the identical implementation in-process (`cmd/reconciler/jobs`) rather than the CronJob shelling out over HTTP to the server pod. This matches `iam-org-membership`'s actual lift-source pattern and LLD §16.1's "a `reconciler` runs the CronJobs" — several other LLD passages phrase DLG-I1/I2 as if they're the *only* entry point, but §16.1 is authoritative on topology. `cmd/server/adapters.go`'s `reconcilerRunner` adapts `cmd/reconciler/jobs`' `Expiry`/`ReviewSweep` functions to the HTTP handler's injected `ExpiryRunner`/`ReviewRunner` interfaces so both paths share one code path. |
| DLG-D18 | The LLD describes RLS/GUC binding as happening "in the postgres outbound adapter" (§7.3/§13.1) without specifying the exact wiring signature. As built, `postgres.DelegationRepository`/`SettingsRepository` methods read whatever GUC is already bound on `ctx` (via `pgcommon.GUCSetFromContext`) — they do **not** derive it from their own `tenantID` parameter internally. This means the GUC must be bound *before* the call reaches the repository: `internal/adapter/inbound/http/router.go` adds a `BindTenantGUC`-typed seam (`tenantGUCMiddleware`, run right after `ContextMiddleware` on the public route group) so `cmd/server` can inject `postgres.WithTenantGUC` without the `http` package importing `pgcommon` directly (Clean Architecture); the mesh-only DLG-I3/I4 routes (no per-request middleware) instead use a small `gucBoundReader` wrapper (`cmd/server/adapters.go`) that binds per-call from the tenantID argument, which is safe there because those are single-statement reads outside any open transaction. The reconciler jobs and cascade consumer bind via their own injected `BindTenantGUC`/`GUCBinder` function parameters, per RLS-6. |
| DLG-D19 | **Known gap, not closed in this build:** the LLD §14.2 metric methods (`RecordCreated`, `RecordEnded`, `RecordReviewWarned`, `RecordMembershipCheckFailure`, `RecordUPAvailabilityFailure`, `RecordIdempotencyHit`, `RecordCascadeProcessed`, `RecordCascadeDLQ`, etc.) exist on `internal/adapter/outbound/metrics.Metrics` and the object is registered into `gincommon.MetricsRegisterer()` at both `cmd/server` startup (metrics registration only — `cmd/reconciler` does not register or call any `Metrics` method at all, since crons are short-lived processes without their own Prometheus scrape endpoint), but **no call site in `internal/core/service` or `cmd/reconciler/jobs` actually calls a `Metrics` method** — none of the service/service-layer constructors accept a `*metrics.Metrics` today. The counters will report zero. Wiring this fully requires adding a metrics-recorder parameter to `DelegationService`/`SettingsService`/`CascadeService`/`jobs.Context` and instrumenting each call site — scoped as immediate follow-up work, not done here to avoid a rushed, under-tested refactor of the already-tested service layer at the end of this build. |
| DLG-D20 | **Supersedes DLG-D15's CI-governance scope** (the in-house-validator half of DLG-D15 remains correct and unchanged — see below). `platform-schemagov` is not a Go dependency (it's a CI-only Docker image); it turns out `iam-user-profile` uses it exactly the same way this service now does, alongside the identical in-house `jsonschema/v6` `SchemaValidator` DLG-D15 describes. This service already shipped `eventbus.GlueCodec` (`codec.go`), the `aws-sdk-go-v2/service/glue` dependency, and a `GlueSchemaRegistryReadOnly` IAM SID in `deploy/iam/policy.json` from the initial build — only the CI-time governance pipeline and monitoring were missing. Closed by adding: `.github/workflows/schema-registry.yml` (rewritten to drive `platform-schemagov` validate/diff/register against a dedicated `iam-delegation-events` Glue registry, replacing the old Python-only structural check), `schema-prune.yml`, `schema-health-quarterly.yml`, `freeze-watchdog.yml`, `deploy/monitoring/schema-registry-alerts.yml`, `docs/runbook-schema-registry.md`, `Makefile`'s `schema-*` targets, `docker-compose.pro.yml`, and `GLUE_REGISTRY_NAME`/`GLUE_REGISTRY_ARN` in `deploy/helm/iam-delegation/values.yaml`. Also added the `x-lifecycle`/`x-owner`/`x-forward-compatibility`/`x-semantic-contract`/`x-version-governance`/`x-usage-override` annotations `schema-gov validate` requires to `api/asyncapi.yaml`, which had none before this change — **run `make schema-validate` on a branch to confirm these pass before relying on CI**, since the annotation content was adapted by hand from `iam-user-profile`'s, not generated by the tool. Requires the `staging` GitHub Environment to be created (this repo previously only had `production`) with `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`/`AWS_REGION` configured, and the `iam-delegation-events` Glue registry provisioned in each AWS account (Terraform shape already present in `deploy/iam/policy.tf.example`) — neither is done by this change. |

## Test coverage (as built, `go test ./... -race -cover`)

| Package | Coverage | Notes |
|---|---|---|
| `internal/core/service` | 84.7% | Full DEL-1…14 branch coverage, availability-first ordering, idempotency replay, reassign end-then-create, DEL-7 asymmetry |
| `internal/adapter/outbound/postgres` | 73.9% | Full §17.5 RLS matrix (Cases 1–5, both tables), review-sweep boundary predicates, trigger, `processed_events` dedup |
| `internal/adapter/inbound/http` | 90.4% | Every `domain.Err*` → HTTP status, role gates, DLG-5 raw-JSON presence detection |
| `internal/adapter/inbound/consumer` | 59.0% | Dispatch, idempotency skip/mark-after-success, malformed/unknown envelope handling |
| `cmd/reconciler/jobs` | 81.7% | Happy path, UP-failure defer, race-vs-failure distinction, dual-bucket warn+auto-end |
| `internal/adapter/outbound/{userprofile,orgmembership,valkey,metrics,eventbus}` | 85–100% | HTTP client shapes, cache/idempotency TTL behavior, metric registration, schema validation |

## Known deviations from a literal reading of the LLD

- **§8.4 DLG-2's `ooo_note`/`reason` fields**: the LLD's prose ("`ooo_note` rides the UP call (not stored); `reason` is stored and forwarded to UP as the display note") reads as two fields but the example request body only shows `ooo_note`. Resolved as one wire field (`ooo_note`, DTO) mapped to one domain field (`CreateInput.Reason`, stored in `delegations.reason`, forwarded to User Profile as the note) — not a second, separate field.
- **§10.5's "I-8 join" framing**: confirmed via the `iam-org-membership` survey that I-8's SQL is four separate queries in one transaction, not one literal multi-table `LEFT JOIN` statement — Option C's subtraction (PR3) removes the fourth query and the `ActiveDelegations` field, with an identical functional outcome to what the LLD describes.
- **PR2 (repoint Workflow/Notification/Audit)** is scoped to shipping a correct, validated `iam.delegation.events` AsyncAPI contract — the Workflow/Notification/Audit service repos are not present in this workspace, so their consumer-side subscription changes are out of scope for this engagement.
