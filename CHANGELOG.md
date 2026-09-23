# Changelog

All notable changes to this project are documented in this file. Format based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

Not yet deployed anywhere — this section describes the service as it currently stands, not a
release history. Schema fixes are folded back into the single `000001_schema` migration rather
than layered as new ones; that stops once this service is actually deployed. Full rationale for
every fix below lives in `ARCHITECTURE.md`'s "Session-specific decisions" register (DLG-D13
onward) and `docs/lld/iam-lld-delegation-service.md`'s revision history — this file stays a short
index into those, not a duplicate of them.

### Added

- **GAP-020 / cross-service compatibility** — `scope=department` delegations now validate `scope_id`
  against the Catalog Admin global department catalog at create time (GAP-020, DLG-D13). Added:
  `port.CatalogAdminClient` interface; `internal/adapter/outbound/catalogadmin/` HTTP adapter
  calling `GET /api/v1/departments/:id` (CAT-7), fail-closed on 5xx (`503 catalog_admin_unavailable`)
  and `422 invalid_scope_id` on 404 or `is_active=false`; `DelegationService.WithCatalogAdmin()`
  optional setter (nil = presence-only fallback, no startup failure); `CATALOG_ADMIN_BASE_URL` /
  `CATALOG_ADMIN_TIMEOUT_MS` config + `.env.example` + Helm `values.yaml` entries; `catalogadmin`
  package added to `.go-arch-lint.yml` `adapters_outbound` allow-list.
- **Observability — platform_dependency_* metrics** — `platform_dependency_request_seconds` and
  `platform_dependency_errors_total` (Registry-Proposed, same pattern as `iam-org-membership`) added
  to `internal/adapter/outbound/metrics/metrics.go`. All three outbound HTTP clients
  (`orgmembership`, `userprofile`, `catalogadmin`) now dual-emit Tier-1 latency + error counters
  alongside the existing Tier-3 legacy metrics during the compatibility period. Package-level
  `ObserveDependencyLatency` / `IncDependencyError` / `DependencyOutcome` helpers added.
- **CI enforcement gates** — four new CI scripts in `.github/scripts/` covering platform library
  compliance: `check-observability-compliance.sh` (logs/metrics/traces via platform-gincommon,
  9 rules), `check-platform-events-compliance.sh` (events/outbox/dedup via platform-events,
  11 rules), `check-pgcommon-compliance.sh` (DB connections/config/operations via platform-pgcommon,
  11 rules), and `check-metric-namespacing.sh` (pre-existing, now wired into `validate-quality.yml`).
  All four run on every PR via `validate-quality.yml`.
- **Cross-service dependencies section** — `README.md` now has a dedicated `## Cross-service
  dependencies` section documenting all synchronous outbound calls (including the new
  `iam-catalog-admin` entry), inbound callers, async event table, and infrastructure dependencies
  in one place, mirroring the `iam-org-membership` README convention.
- **OM-GAP-9 fix** — `x-caller-service: iam-delegation` header added to
  `internal/adapter/outbound/orgmembership/propagate.go` so `iam-org-membership`'s I-15
  `iam_org_membership_membership_exists_check_total{caller}` metric correctly labels delegation's
  grant-time membership checks (previously showed `caller="unknown"`).

- Initial extraction from `iam-org-membership` as the fourth and last O&M extraction service
  (ADR-0008 Option C) — owns `delegations`/`delegation_tenant_settings`/`processed_events`, ships
  the DLG-1..7 public API and DLG-I1..I4 mesh-only internal API, two binaries (`cmd/server`,
  `cmd/reconciler`), the `iam.delegation.events` topic (this service is both producer and
  consumer), and an independent Helm chart/CI pipeline.
- CI schema governance (`platform-schemagov`) and documentation parity with `iam-user-profile`
  (DLG-D20).
- `ended_reason=reassigned` (`EndReasonReassigned`) — DLG-5 Reassign now ends the replaced
  delegation with this reason instead of `cancelled`, so consumers can tell a reassignment from
  a user cancel.
- SLO burn-rate alerting (DLG-D39, LLD §14.5): four multi-window multi-burn-rate SLOs (write-path
  error rate, DLG-D3 membership-check dependency success, reconciler-defer convergence, cascade
  convergence) as recording rules + fast-burn/slow-burn alert pairs, in `deploy/monitoring/slo-rules.yml`
  and `templates/prometheusrule.yaml`'s new `iam_delegation_slo_records`/`iam_delegation_slo_burn`
  groups — a formalization of targets §14.1/§14.5 already stated, complementing (not replacing)
  the existing threshold alerts in `app-alerts.yml`.
- LLD §17.4 end-to-end test suite (DLG-D41, `cmd/server/e2e_test.go`, `//go:build e2e`) — boots
  the real composition root against real Postgres/Valkey/floci containers and drives it over real
  HTTP/SQS, closing the gap where `make test-e2e` silently re-ran the untagged unit suite (no file
  anywhere declared the `e2e` build tag). Covers create/cancel fan-out, expiry, the review daily
  cascade, and the user-removal cascade (including DLG-EVT-4's silent delegator-side end).

### Changed

- **`api/asyncapi.yaml` is the single source of the event schemas (DLG-D51):** payloads now use
  the `<Name>Envelope` → `data: <Name>Payload` form, `make extract-schemas` generates
  `internal/eventschema/*.json`, and CI's new "Event schema sync check" (`extract --check`,
  `make schema-sync-check` locally) fails on drift. The produced schemas lose their top-level
  descriptions (no field change), so they register as new Glue versions.
- **Consumed payloads are validated before dispatch (DLG-D51):** `delegation-cascade-q` checks
  `MembershipRevoked`/`TenantMembershipsPurged`/`UserUpdated` against embedded consumed schemas
  (`eventbus.ConsumedValidator`); a violation is logged, counted on
  `iam_delegation_cascade_dlq_total` and left for redrive to the DLQ, never acked.

- **Glue schema-version resolution by definition (DLG-D50):** `GlueCodec` resolves each
  schema's version once at startup with `GetSchemaByDefinition` over the binary's own embedded
  schema (must be `AVAILABLE`) instead of `GetSchemaVersion(LatestVersion)` plus a 5-minute
  refresher, so events carry the version the binary actually produces. `StartRefresher` and
  `GlueCodec.WithLogger` are removed. An unregistered definition now fails startup;
  `make schema-verify` checks all four schemas by definition + `AVAILABLE` pre-deploy.

- Default AWS region is now `ap-south-1` (`cmd/server` fallback, Helm `values.yaml`,
  docker-compose, LocalStack, `.env.example`).
- Dependabot disabled — `.github/dependabot.yml` renamed to `.github/dependabot.yml.disabled`.

### Fixed

- **Architecture diagrams re-synced:** 8 of 10 LLD-derived diagrams had drifted between the LLD,
  `docs/architecture/mermaid/*.mmd` and ARCHITECTURE.md. All were corrected against the code
  (`ooo_until`, the `delegate_disable` bucket, cascade commit order, Extend's UP re-sync,
  Reassign's `reassigned` reason) and made identical in all three places.

- **Schema governance (DLG-D51):** `schema-gov validate` Pass 7 failed on every run — the three
  consumed messages in `api/asyncapi.yaml` had no schema files (now extracted). asyncapi's
  `actor_id` was wrongly *required* on `MembershipRevoked`/`TenantMembershipsPurged` (Core omits
  it on system-driven removals). `usage-check` used snake_case file stems as event names, so
  usage→lifecycle enforcement never matched a message; it now reads the PascalCase staged copy
  shared with `diff`/`register`.

- **Glue registration names (DLG-D50):** `schema-registry.yml`'s register steps and
  `make schema-register` registered `internal/eventschema/*.json` under their snake_case file
  stems (`delegation_started`), but the service looks up `DelegationStarted` — every pod would
  have failed startup in a CI-provisioned environment. Both now register a PascalCase copy
  staged by `.github/scripts/stage-produced-event-schemas.sh`. `make schema-verify` also no
  longer skips `DelegationEscalationRequested`.

- **New CI gate ported from `iam-org-membership` (DLG-D47):**
  `.github/scripts/check-outbox-access.sh` rejects any hand-rolled SQL against
  `outbox_events` outside `internal/adapter/outbound/eventbus` — every
  mutation must go through `outbox.Enqueue`/`outbox.Runner.PrunePublished`.
  The sibling repo hit this exact bypass once (a hand-rolled batched
  `DELETE` duplicating `PrunePublished`); this service has no history of
  it, but the same gap applies equally here. Wired into
  `validate-quality.yml` next to DLG-D46's `check-forbidden-events-bypass.sh`.
- **Correctness pass over the DLG-D40..D45 sweep, plus a new CI gate (DLG-D46):**
  DLG-D44's tx-context move left `internal/core/port/tx_runner.go` importing
  `github.com/jackc/pgx/v5` directly, violating this repo's own "core imports no
  pgx" rule — `port.WithTx`/`port.TxFromContext` now carry the tx as `any`, with
  the `pgx.Tx` assertion only on the two adapter call sites that need it.
  DLG-D42's `logger` import rename collided with a `logger` parameter in both
  `run()` functions (`golangci-lint` `importShadow`) — restored the `gclogger`
  alias. DLG-D44's `config.LoadOutbox()` has different library defaults (`5s`
  poll / concurrency 1) than this service's historical ones (`500ms` / 4); Helm
  and `.env.example` were already pinned, but any other invocation path wasn't —
  added `ensureOutboxEnv()` (mirrors `ensureGincommonEnv`) so the historical
  defaults apply everywhere, not just those two files. New CI gate,
  `.github/scripts/check-forbidden-events-bypass.sh`, rejects any AWS SDK
  SNS/SQS import/call outside `cmd/server/main.go`'s one legitimate
  client-construction site, and any hand-built `events.Envelope{}` literal.
  `api/asyncapi.yaml`'s four published-event schemas were missing the
  `tenant_id` property `internal/eventschema/*.json` already declares —
  added to all four.
- **Production-readiness sweep (DLG-D40):** `registerDocsRoutes` only gates `/swagger` and
  `/asyncapi` behind `docsAuthMiddleware` when `Environment=="production"` AND `AuthToken!=""` —
  `loadConfig` had no matching fail-fast, so `DOCS_ENABLED=true` with `DOCS_AUTH_TOKEN` unset in
  production would have served both docs surfaces with no auth check at all. `loadConfig` now
  rejects that combination at startup. Also removed `Metrics.SetActiveGauge` (dead code — no call
  site outside its own test; `ReplaceActiveGauges` already sets the same gauge inline).
- Database connection/configuration/operations re-checked against
  `iam-user-profile` / `iam-org-membership` (DLG-D43): the active-gauge
  snapshot now uses `pgcommon.Pool.WithConn` (O&M's exporter pattern) instead
  of opening a `RunInTx` for a read-only `COUNT`; `ApplyStatementTimeout`
  uses `?` when the DSN has no query string so `SYSTEM_DATABASE_URL` /
  `MIGRATION_DATABASE_URL` without `?sslmode=` stay valid URLs. Connection,
  pool, GUC, migrate, health, and `TxRunner` already went through pgcommon
  (DLG-D23/D32/D37); `wrapConnErr` keeps the D32 pass-through for business
  errors rather than O&M's catch-all 503.
- Events/outbox/dedup pass through `platform-events` only (DLG-D45):
  deleted the local enqueue `Codec`/`NoopCodec` duplicate; `Publisher` and
  `ValidatingCodec` now implement/wrap `events.Codec` and use
  `events.NoopCodec` at enqueue time (the library ships no schema
  validator or Glue codec — those remain service-side `events.Codec`
  implementations). SNS/SQS/outbox runner/dedup composition from DLG-D44
  is unchanged.
- Events/outbox/dedup re-checked against `iam-user-profile` /
  `iam-org-membership` (DLG-D44): SNS/SQS/outbox runner now go through
  `platform-events` `config.LoadSNS` / `LoadSQS` / `LoadOutbox` +
  `SNSConfigFromEnv` / `SQSConfigFromEnv` / `SQSConsumerOptions` /
  `RunnerConfigFromEnv` (the siblings' composition contract). `CASCADE_*`
  overlays `SQS_QUEUE_URL`/`SQS_CONCURRENCY` the same way O&M overlays
  per-queue names. `eventbus.Publisher` reads the `RunInTx` tx via
  `port.TxFromContext` so it no longer imports the postgres adapter. Helm /
  `.env.example` keep the historical `OUTBOX_*` values so library defaults
  (`5s` poll / concurrency 1) do not silently change production. Consume
  still uses `GlueDecodeCodec` (O&M catalog-only consume is LLD A68);
  RoutingPublisher is not copied.
- Events/outbox/dedup re-checked against `iam-realm-provisioner` /
  `iam-org-membership` (DLG-D38): `outbox_events.payload` is `TEXT` with an
  `outbox_normalize_payload` trigger (PgBouncer SimpleProtocol); cascade
  PG writes and `processed_events` commit in one `TxRunner.RunInTx`; filtered
  `UserUpdated` acks record a dedup row; `idx_processed_events_processed_at`
  backs prune; `PROCESSED_EVENTS_TTL_DAYS` (default 30) drives monthly cleanup.
- Logs/metrics/traces re-checked against `iam-user-profile` / `iam-org-membership`
  (DLG-D42): duplicated per-package `Logger` interfaces collapsed onto one
  `internal/core/port.Logger` matching gincommon's Zap shape; both binaries
  construct the sink via `logger.NewLogger` and thread that value through
  gincommon / platform-events / pgcommon (`postgres.NewLoggerAdapter` remains
  the only adapter, for pgcommon's Field-based `domain.Logger`); the reconciler
  now calls `events`/`pgmetrics` `InitWithRegisterer` after
  `ObservabilityMiddlewares`; unhandled HTTP 500s include gincommon
  `trace_id`/`request_id` plus path/method.
- **`cmd/server` ran the outbox/domain migrations *after* opening the RLS-scoped `delegation_app`
  pool**, not just in the wrong order relative to each other (that ordering was already fixed —
  see the migration-startup-order entry below). On a genuinely fresh database the domain migration
  is what *creates* the `delegation_app` role, so opening that pool before migrations ran failed
  bring-up outright rather than merely risking a `GRANT`-ordering race. Migrations now run before
  any pool is constructed. Also: `make test-ci`'s `sed -i ''` (BSD-only) is replaced with a
  portable temp-file redirect, since GNU sed on Linux CI runners treated the empty string argument
  as a filename and failed; and test coverage was broadened across the adapter and service layers
  (including a new `errortx_test.go`) as part of the same pass.
- **Cross-service (LLD rev 2.11):** `iam-user-profile` migrated its published event type names
  from dot-notation to PascalCase while still undeployed — `domain.EventUserUpdated` here changed
  from `"user.updated"` to `"UserUpdated"` to match. `CascadeConsumer.Handle`'s dispatch already
  compared against the symbolic constant, not a literal, so only the constant's value and doc/spec
  references to the literal SNS filter value needed updating (§10.5, §11.5a, `api/asyncapi.yaml`,
  `ARCHITECTURE.md`'s DLG-D26 entry). Caught before the still-unprovisioned `delegation-cascade-q`
  subscription (DLG-D26) went live with the wrong filter value.
- DLG-5 Reassign didn't preserve the old delegation's `ends_at` when omitted.
- `Idempotency-Key` accepted whitespace-only keys and unbounded values; blank/whitespace keys
  and keys over 1 KiB are now rejected as `ErrValidation`.
- `make test-ci` ran `go test ./...` three times in parallel (the unit/integration/rls
  targets are the same colocated suite) and the Glue refresher tests raced on the mock
  server's request log — both failed the CI `-race` gate. The suite now runs once, and
  the mock is synchronized. Postgres integration tests now share one Testcontainers
  instance (truncate between tests) so the ~50-container suite fits the runner
  timeout; the Glue refresher call counter is atomic.
- Docker image build excluded `docs/swagger` via `.dockerignore`, so
  `go build ./cmd/server` failed in buildx (`docs/swagger` is a checked-in
  Go package blank-imported for the Swagger UI). The swagger tree is now
  kept in the build context.
- The monthly `delegation-cleanup` CronJob never purged `processed_events` — `cmd/reconciler`
  never wired a `ProcessedEvents` store.
- The two reconciler deferred-counter metrics were registered but never incremented (DLG-D19).
- Migration startup order broke every fresh database — outbox schema must apply before the
  domain migration's GRANT on `outbox_events`.
- The cascade consumer couldn't decode Core's Glue-encoded events — missing
  `events.WithConsumerCodec` (DLG-D21).
- Logging/metrics/traces audited to pass through `platform-gincommon` only — both binaries now
  call `logger.NewLogger` directly instead of hand-rolling `slog` (DLG-D22). A second pass
  against `iam-user-profile`'s live wiring (DLG-D30) closed init-order and scrape-port gaps.
  A third pass against `iam-realm-provisioner` (DLG-D31) closed the remaining hop-level
  gaps: outbound User Profile / Org Membership clients wrap `otelhttp` via a shared
  `internal/adapter/outbound/httpx` transport so those calls emit client spans and inject
  `traceparent`; `metrics.Register()` is the no-arg gincommon API (`sync.Once`); both
  binaries register collectors immediately after `ObservabilityMiddlewares`; the
  reconciler primes metrics labels, starts a `reconciler.<job>` span, and shares one
  `NewOTelTracer` across both pools; `cmd/server` listens on `METRICS_PORT` (Helm already
  scraped `:9090/metrics`). `iam_delegation_active_gauge{tenant}` is now populated by a 5-minute
  BYPASSRLS snapshot exporter in `cmd/server` (was registered but never set).
- Database connection/configuration/operations re-audited against `iam-realm-provisioner`
  (DLG-D32): bumped `platform-pgcommon` to v1.3.0; `TxRunner` retries deadlocks via
  `RunInTxWithRetryOpts`; `wrapConnErr` uses pgcommon SQLSTATE helpers (08/53/57/58) and
  `puddle.ErrClosedPool` → `db_unavailable` 503, and no longer masks business errors as
  503; `processed_events` moved to the postgres adapter and goes through `withPool`;
  `MIGRATION_DATABASE_URL` gets `PG_STATEMENT_TIMEOUT`; `/readyz` checks sysPool;
  `cmd/server` applies outbox schema before the domain GRANT (matching
  `postgres.Migrate`); both binaries `DrainAndClose` with a timeout. Tests
  seed through `pgcommon.NewPool` (no `pgxpool.New`). Helm/compose pass
  `SYSTEM_DATABASE_URL`, `PG_BOUNCER_MODE`, and `PG_STATEMENT_TIMEOUT`.
- Events/outbox/dedup re-audited against `iam-realm-provisioner` (DLG-D33): enqueue
  moved from `postgres.txBoundPublisher` to `eventbus.Publisher` + `ValidatingCodec`
  (missing schema = pass-through); both binaries inject that publisher into
  `TxRunner` so cron events are schema-validated; cascade consumer marks unknown
  types in `processed_events`, counts duplicates, and `MarkProcessed` goes through
  `TxRunner.RunInTx`; SQS wiring sets `Region`/`EndpointURL`/`WithConcurrency`;
  monthly cleanup uses bounded `Prune` instead of unbounded `CleanupExpired`.
- Database connection/configuration/pool operations audited to pass through `pgcommon` only —
  fixed `SystemDSNFromEnv`'s empty-DSN fallback and added `PG_STATEMENT_TIMEOUT` support (DLG-D23).
- Events/outbox/dedup audited to pass through `platform-events` only — added the missing
  `specversion` envelope field, made `outbox.Config` env-tunable, added the `PrunePublished` prune
  sweep, and fixed a stale `specversion` const in `api/asyncapi.yaml` (DLG-D24).
- Cross-service bug: a delegation created with a future `starts_at` immediately called User
  Profile's `SetAvailability` and emitted `DelegationStarted`, showing the delegator as OOO and
  routing work to the delegate before the leave actually began. `Create` now defers both when
  `starts_at` is genuinely in the future — the delegation is inserted as a new `scheduled` status
  instead of `active`, with no User Profile call and no event yet. A new `delegation-activation`
  CronJob (`*/5 * * * *`, mirrors `delegation-expiry`) calls User Profile and flips the row to
  `active` (emitting `DelegationStarted` then) once `starts_at` is reached. `EndForUser` (the
  `MembershipRevoked` cascade) and `Cancel`/`End` now also accept `scheduled` rows, so a scheduled
  delegation for a member who leaves the tenant is cancelled rather than stranded (DLG-D25). See
  `iam-user-profile`'s matching CHANGELOG entry for the server-side counterpart fix.
- Cross-service bug (Bug 2): disabling a delegate never ended their active delegations — this
  service only ever cascaded on `MembershipRevoked` (full tenant removal), and "disabled" is not
  "removed". A delegate's disabled account kept receiving routed work indefinitely. `delegation-
  cascade-q` now carries a second SNS subscription onto User Profile's `iam.user.events`, filtered
  to `EventType = "UserUpdated"`; on a decoded payload with `status: "disabled"`,
  `CascadeService.EndForDisabledDelegate` ends every active/scheduled delegation where the
  disabled user is the delegate, emitting `DelegationEnded{ended_reason: delegate_disabled}` — a
  new value distinct from `delegate_removed`, and (unlike the `MembershipRevoked` cascade)
  `deleted_at` is deliberately left unset, since the user is still a tenant member. No User Profile
  pointer-clear call is needed here: User Profile already clears the delegate pointer atomically
  within the same transaction that publishes this event (see `iam-user-profile`'s matching
  CHANGELOG entry, which also documents the new `UserUpdatedPayload.status` field this fix
  depends on) (DLG-D26).
- Cross-service bug (Bug 2a): a generic `DelegationEnded` alone gave nobody a signal that the
  delegator might now have no valid handler for their work while still OOO after their delegate
  was disabled. `EndForDisabledDelegate` (DLG-D26) now also enqueues a new event,
  `DelegationEscalationRequested`, immediately after `DelegationEnded` in the same transaction —
  self-contained (carries the same `delegation_id`/`delegator_id`/`delegate_id`/`scope`/`scope_id`
  plus a `reason` field). Escalates to `tenant_admin`/`tenant_owner` specifically, not a new
  "supervisor"/"team lead" concept — this service owns no org-structure data, and admin/owner is
  the only role it already recognizes as escalation-capable and the only one that can call DLG-5
  Reassign. Notify-only: never auto-creates a replacement delegation. New JSON Schema
  (`internal/eventschema/delegation_escalation_requested.json`), registered in `SchemaValidator`
  and the Glue codec's pre-fetch list. Also fixed two related, pre-existing schema-governance gaps
  found while implementing this (both silent-failure bugs from DLG-D26 that every unit test missed,
  since unit tests use a fake `EventPublisher` that skips schema validation): `delegate_disabled`
  was never added to `internal/eventschema/delegation_ended.json`'s or `api/asyncapi.yaml`'s
  `ended_reason` enum, and the Glue codec's schema pre-fetch list in `cmd/server/main.go` was never
  updated for the new event type (DLG-D27). See `iam-user-profile`'s matching CHANGELOG entry for
  Bug 2's `UserAvailabilityChanged` fix this depends on.
- Production-readiness sweep (DLG-D28): CI's `SCHEMA_NAME_MAP` (all three job blocks in
  `.github/workflows/schema-registry.yml`) had no entry for `DelegationEscalationRequested` — it
  would have registered the new Glue schema under the wrong (snake_case) name, and `cmd/server`
  would have failed to start in any environment with `GLUE_REGISTRY_NAME` set. Also added the
  missing `IamDelegationActivationDeferred` Prometheus alert (the DLG-D25 metric was wired but
  never alerted on — a stuck `iam-user-profile` during the activation cron's window would silently
  leave delegations un-activated forever with no page) and fixed `scripts/init-localstack.sh`'s
  missing schema registration/queue filters for local dev.
- **Cross-service bug (DLG-D29, pre-existing, unrelated to DLG-D25/26/27): open-ended delegations
  could never actually be created.** `Create` sent `OOOUntil: nil` to User Profile for any
  delegation with no `ends_at` — a first-class case, not an edge case (DLG-4 Extend and DLG-I2's
  review-cron exist only to serve open-ended delegations). User Profile's `ValidateOOOWindow`
  unconditionally requires `ooo_until` whenever `status="ooo"`, so every open-ended create was
  rejected with 422, mapped to a confusing `invalid_delegate`. Confirmed live (not just by
  inspection) against User Profile's real HTTP handler before fixing. `Create` now sends the
  already-computed `review_due_at` as `OOOUntil` for open-ended delegations instead — always
  within User Profile's 180-day cap, since `review_window_days` is itself bounded to 1..180.
  `Extend` (which previously never called User Profile at all) now re-syncs the new
  `review_due_at` on every successful extend, fail-open, matching Cancel's existing pattern — 
  without this, an extended open-ended delegation's `ooo_until` in User Profile would go stale and
  eventually trigger a premature reset to `available`. `Reassign` needed no separate fix (its
  create leg already calls `Create`). **A third audit pass found the identical bug at a second,
  independent call site**: `cmd/reconciler/jobs/delegation_activation.go`'s `Activation` job (which
  finishes creating a *scheduled* open-ended delegation once `starts_at` is reached) had its own
  `OOOUntil: d.EndsAt` — also fixed, falling back to `d.ReviewDueAt`. Unfixed, this would have
  deferred forever: the same rejected payload resent unchanged on every 5-minute tick, continuously
  triggering the DLG-D28 alert rather than resolving. Grepped every `SetAvailability` call site in
  the repo afterward (10 total) to confirm these were the only two that set `Status: "ooo"` — the
  other 8 only clear the delegate pointer, unaffected. See `iam-user-profile`'s matching CHANGELOG
  entry for the permanent contract-guard test this depends on.
- **Production-readiness sweep (DLG-D34):** `make lint` failed outright (17 issues — unchecked
  `os.Setenv`/`gincommon.Shutdown` errors in both binaries' config/main files, a second
  `context.WithTimeout(context.Background(), ...)` call needing its own `//nolint:contextcheck`
  in `cmd/server/main.go`'s shutdown goroutine, missing package comments on the `consumer` package's
  new files, `otel_tracer.go`'s `NewOTelTracer` returning an unexported type, and three stale
  `//nolint:errcheck` directives in `cascade_service.go`/`delegation_service.go` left over from
  before those `SetAvailability` calls were changed to actually check their error — all fixed).
  `go.mod`/`go.sum` had `make tidy` drift (`otelhttp`/`puddle` mis-declared as indirect) — also
  fixed. Both `cmd/server` and `cmd/reconciler` now fail fast at startup if `SYSTEM_DATABASE_URL`
  is unset when `ENVIRONMENT=production`, instead of silently falling back to the RLS-scoped app
  DSN — without this, every cross-tenant sweep query (`ListExpiringBefore`/`FindDueForDailyWarn`/
  `FindDueForAutoEnd`/`HardPurgeSoftDeletedBefore`) and the active-gauge exporter would silently
  return zero rows under RLS with no tenant GUC bound, with no alert firing (a different failure
  mode than the existing deferred-counter alerts cover). Also removed a stray, untracked ~88 MB
  `server` binary from the repo root and added `/server`/`/reconciler`/`/iam-delegation-server`/
  `/iam-delegation-reconciler` to `.gitignore` (only `/bin/` was covered before). Global statement
  coverage raised from 83.9% to 95.2% (CI's own gate, `.github/scripts/coverage-gate.sh`, threshold
  bumped 70% → 95% in the same pass) — the largest single gain was direct AWS Glue Schema Registry
  API mocking via a real `httptest.Server` speaking AWS JSON 1.1 (`GlueCodec`'s construction,
  version-cache refresh, and encode paths had zero coverage before, since `*glue.Client` is a
  concrete SDK type with no test seam); the rest closed handler auth-branch, cascade-consumer
  error/metrics, schema-validator error, and repository edge-case gaps. One real bug found and
  fixed along the way: a test-only `fakeLogger` field was read/written from two goroutines with no
  synchronization, caught by `-race` (which `make test-ci` always runs) as a genuine data race, not
  merely flaky timing.
- **Production-readiness sweep (DLG-D35):** `DelegationService.Create`'s idempotency-key dedup was
  a plain Valkey `GET`-then-`SET` — LLD §9.2 always specified `SETNX` as the guard, so this was an
  implementation gap, not a design change: two concurrent `POST /delegations` calls sharing one
  `Idempotency-Key` could both miss the `GET` and both insert a delegation. `port.IdempotencyStore`
  gained `Reserve`/`Release` (atomic `SET NX`, `internal/adapter/outbound/valkey/idempotency.go`);
  `Create` now reserves the key before any membership check/User Profile call/insert, releases it
  on any failure (so a retry isn't stuck for the 24 h TTL), and a losing concurrent caller gets a
  new `409 idempotency_key_in_flight` (`domain.ErrIdempotencyKeyInFlight`) instead of racing to a
  duplicate row. Also widened DLG-D34's `SYSTEM_DATABASE_URL` fail-fast from a literal
  `ENVIRONMENT=="production"` compare to a new `isDevLikeEnvironment` helper (`development`/`dev`/
  `local` exempt, everything else fails fast) — staging/uat previously fell through to the same
  silent zero-rows-no-alert failure mode DLG-D34 was written to close. Two additional hardening
  fixes with no LLD-visible design content: the Swagger/AsyncAPI docs bearer-token compare now uses
  `crypto/subtle.ConstantTimeCompare` instead of `!=` (closes a timing side-channel on the docs
  gate, not the API itself); the User Profile and Org Membership HTTP clients now cap
  response-body decoding at 1 MiB via a shared `httpx.LimitBody`, bounding worst-case memory use
  against a misbehaving mesh peer. See `ARCHITECTURE.md`'s DLG-D35 entry and
  `docs/lld/iam-lld-delegation-service.md` rev 2.12 for the full account.

### Known gaps

- HPA on `delegation-cascade-q` queue depth (LLD §16.3) is not wired — the chart scales on
  CPU/memory (and, optionally, RPS) only.
- Workflow Service (a separate repo/team not accessible from this session) has no fallback logic
  when a delegate becomes unavailable during an active OOO window — bug report gap #4. This
  service's `DelegationEscalationRequested` (DLG-D27) plus User Profile's corrected
  `UserAvailabilityChanged` (DLG-D26) together give that team everything needed to build one;
  closing it is a cross-team dependency this repo cannot resolve on its own.
- **Pre-deploy action item (DLG-D26):** `delegation-cascade-q`'s second SNS subscription — User
  Profile's `iam.user.events` topic, filtered to `EventType = "UserUpdated"` — is documented
  (LLD §10.1/§11.5a, `api/asyncapi.yaml`) but not yet provisioned anywhere; this repo owns no
  Terraform/CDK for SNS subscriptions (see `deploy/iam/policy.tf.example`'s own "illustrative,
  not applied" disclaimer). Without it, `EndForDisabledDelegate`/`DelegationEscalationRequested`
  are fully implemented and tested but will never actually run — the consumer code has nothing to
  consume. Platform/infra must add this subscription (with the same DLQ/`maxReceiveCount=5` as the
  existing `iam.membership.events` subscriptions) before this fix takes effect in any real
  environment.
