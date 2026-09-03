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

### Fixed

- DLG-5 Reassign didn't preserve the old delegation's `ends_at` when omitted.
- The monthly `delegation-cleanup` CronJob never purged `processed_events` — `cmd/reconciler`
  never wired a `ProcessedEvents` store.
- The two reconciler deferred-counter metrics were registered but never incremented (DLG-D19).
- Migration startup order broke every fresh database — outbox schema must apply before the
  domain migration's GRANT on `outbox_events`.
- The cascade consumer couldn't decode Core's Glue-encoded events — missing
  `events.WithConsumerCodec` (DLG-D21).
- Logging/metrics/traces audited to pass through `platform-gincommon` only — both binaries now
  call `logger.NewLogger` directly instead of hand-rolling `slog` (DLG-D22).
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

### Known gaps

- HPA on `delegation-cascade-q` queue depth (LLD §16.3) is not wired — the chart scales on
  CPU/memory (and, optionally, RPS) only.
- Most `iam_delegation_*` business metrics are registered but not yet instrumented at any call
  site (DLG-D19) — those counters report zero until that wiring lands.
