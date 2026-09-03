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

### Changed

- Default AWS region is now `ap-south-1` (`cmd/server` fallback, Helm `values.yaml`,
  docker-compose, LocalStack, `.env.example`).

### Fixed

- DLG-5 Reassign didn't preserve the old delegation's `ends_at` when omitted.
- `Idempotency-Key` accepted whitespace-only keys and unbounded values; blank/whitespace keys
  and keys over 1 KiB are now rejected as `ErrValidation`.
- `make test-ci` ran `go test ./...` three times in parallel (the unit/integration/rls
  targets are the same colocated suite) and the Glue refresher tests raced on the mock
  server's request log — both failed the CI `-race` gate. The suite now runs once, and
  the mock is synchronized. Postgres integration tests now share one Testcontainers
  instance (truncate between tests) so the ~50-container suite fits the runner
  timeout; the Glue refresher call counter is atomic.
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
  to `EventType = "user.updated"`; on a decoded payload with `status: "disabled"`,
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

### Known gaps

- HPA on `delegation-cascade-q` queue depth (LLD §16.3) is not wired — the chart scales on
  CPU/memory (and, optionally, RPS) only.
- Workflow Service (a separate repo/team not accessible from this session) has no fallback logic
  when a delegate becomes unavailable during an active OOO window — bug report gap #4. This
  service's `DelegationEscalationRequested` (DLG-D27) plus User Profile's corrected
  `UserAvailabilityChanged` (DLG-D26) together give that team everything needed to build one;
  closing it is a cross-team dependency this repo cannot resolve on its own.
- **Pre-deploy action item (DLG-D26):** `delegation-cascade-q`'s second SNS subscription — User
  Profile's `iam.user.events` topic, filtered to `EventType = "user.updated"` — is documented
  (LLD §10.1/§11.5a, `api/asyncapi.yaml`) but not yet provisioned anywhere; this repo owns no
  Terraform/CDK for SNS subscriptions (see `deploy/iam/policy.tf.example`'s own "illustrative,
  not applied" disclaimer). Without it, `EndForDisabledDelegate`/`DelegationEscalationRequested`
  are fully implemented and tested but will never actually run — the consumer code has nothing to
  consume. Platform/infra must add this subscription (with the same DLQ/`maxReceiveCount=5` as the
  existing `iam.membership.events` subscriptions) before this fix takes effect in any real
  environment.
