# Key Design Decisions

Pulled from `ARCHITECTURE.md`'s DLG-D1..D35 decision register — the ones most load-bearing for anyone touching this code:

1. **Availability-first ordering on create (DLG-D10/DEL-6)** — the User Profile availability call happens BEFORE the `RunInTx{INSERT delegations}` on create, and pointer-clear happens before the row-end on cancel/expiry/review. This guarantees no `DelegationStarted` without a matching `user_availability` pointer, at the cost of an extra synchronous hop most services wouldn't need.
2. **Idempotency key is mandatory on create, not optional (DLG-D8)** — `requireIdempotencyKey()` middleware rejects `POST /delegations` with 400 before the handler even runs. Closes the duplicate-on-retry gap left by there being no `UNIQUE(tenant_id, delegator_id)` constraint (a user can legitimately hold multiple concurrent delegations of different scopes).
3. **Reassign is end-then-create, and failure does not resurrect (DLG-D11)** — a reassign that fails after ending the old delegation but before creating the new one leaves the old one ended. This is deliberate, not a bug to "fix" — see `flows-and-concurrency.md`.
4. **3-day daily-cascade review warning, not a single deadline (DLG-D6/D7/DLG-Q6, revised in LLD v2.2)** — `review_last_warned_bucket` tracks the last `days_remaining` value (3, 2, or 1) notified, so a delegator gets one notice per calendar day for each of the 3 days before `review_due_at`, not a single 7d/3d pair. `ended_reason=review_expired` is a distinct value from `expired` so consumers can tell an auto-ended-on-review-lapse delegation from a plain `ends_at` expiry.
5. **The synchronous user-removal gate stays in Core; only the row-end moves here, async (DLG-D9)** — Core's Workflow-impact check and P-26 resolution happen before removal is applied. This service only reacts to `MembershipRevoked` afterward — never gates the removal itself.
6. **Three distinct GUC-binding paths, not one (DLG-D18)** — `tenantGUCMiddleware` (public routes), `gucBoundReader` (mesh-only single-statement reads, DLG-I3/I4), and injected `BindTenantGUC`/`GUCBinder` function parameters (reconciler jobs, cascade consumer). Repository methods never derive the GUC from their own `tenantID` argument — the caller must bind it first. Get the wrong one and RLS silently returns zero rows, not an error.
7. **DLG-I1/I2 have two real entry points sharing one implementation, not one canonical path (DLG-D17)** — the HTTP endpoints (`POST /internal/delegations/expire`/`review-sweep`) and the CronJob binary both ultimately call `cmd/reconciler/jobs`' `Expiry`/`ReviewSweep` functions; `cmd/server/adapters.go`'s `reconcilerRunner` is the bridge. Don't assume the HTTP route is a thin wrapper that could be deleted — the CronJob depends on the same code path being correct.
8. **Event-type constants ARE the Glue schema names (no DLG-D — but easy to get wrong by analogy)** — unlike `iam-user-profile`'s `domain.GlueSchemaName` dot-notation-to-PascalCase switch, `domain.EventDelegationStarted` etc. are already PascalCase and used verbatim as the Glue registry schema name. Do not introduce a translation layer by copying the user-profile pattern.
9. **`platform-schemagov` is a CI tool, not a validator replacement (DLG-D15/DLG-D20)** — the in-house `jsonschema/v6` `SchemaValidator` and CI-time `platform-schemagov` Docker image are not alternatives to each other; this service (like `iam-user-profile`) uses both. DLG-D15's original "instead of" framing was based on a misunderstanding, corrected by DLG-D20.
10. **Metrics are fully instrumented (DLG-D19 closed)** — `internal/adapter/outbound/metrics.Metrics` is registered with the Prometheus registerer at startup. `cmd/reconciler/jobs.Context.Metrics` calls `RecordExpiryDeferred`/`RecordReviewDeferred`/`RecordActivationDeferred` on every UP-failure defer (GAP-27/DLG-D25, the three counters the production alerts watch), wired from `cmd/server/main.go`. The service-layer counters (`RecordCreated`/`RecordEnded`/`RecordIdempotencyHit`/`RecordUPAvailabilityFailure`) go through `internal/core/service/metrics.go`'s injected `Metrics` port. `iam_delegation_active_gauge` — a count of current table state no request/reconciler path could otherwise maintain — is kept current by `cmd/server/exporters.go`'s 5-minute BYPASSRLS snapshot exporter. See Troubleshooting below.
11. **Future-dated `starts_at` is now a first-class `scheduled` status, not out of scope (DLG-D25)** — `Create` branches on `starts.After(now)`: a genuinely future `starts_at` skips the User Profile call and `DelegationStarted` entirely, inserting the row as `scheduled` instead of `active`. The `delegation-activation` CronJob (mirrors `delegation-expiry`'s shape exactly) promotes it to `active` once `starts_at` is reached. `End`/`EndForUser` treat `scheduled` as non-terminal alongside `active` — a scheduled delegation is cancellable and is ended (not stranded) if the delegator leaves the tenant first.
12. **"Disabled" is not "removed" — a second, independent cascade path (Bug 2, DLG-D26)** — `MembershipRevoked` (tenant departure) was the only inbound cascade signal before this; a disabled delegate (still a tenant member) kept receiving routed work indefinitely. `delegation-cascade-q` now also carries a second SNS subscription onto User Profile's `iam.user.events`, filtered to `EventType = "UserUpdated"`; `CascadeService.EndForDisabledDelegate` ends every active/scheduled delegation where the disabled user is the delegate, **without** setting `deleted_at` (the user is still a member — this is a normal historical record, not a scrub) and without a User Profile pointer-clear call back (User Profile already cleared it atomically before publishing the event this consumer reacts to). **Pre-deploy gap:** the SNS subscription itself is not yet provisioned in any environment this repo owns Terraform for.
13. **Escalation is a fourth, notify-only event — never auto-remediation (Bug 2a, DLG-D27)** — `EndForDisabledDelegate` also enqueues `DelegationEscalationRequested` immediately after `DelegationEnded`, same transaction, escalating to `tenant_admin`/`tenant_owner` specifically (the only role this service already recognizes as escalation-capable, and the only one that can call DLG-5 Reassign). It never auto-creates a replacement delegation — building that fallback is explicitly Workflow Service's cross-team responsibility, not this repo's.
14. **Idempotency create-guard is a `SETNX` reservation, not a plain check-then-act (DLG-D35)** — `port.IdempotencyStore.Reserve`/`Release` close a real race the original `Get`-then-`Save` implementation left open (two concurrent same-key `POST /delegations` could both miss the `Get` and both insert). `Create` reserves the key before any side effect and releases it on failure; a losing concurrent caller gets `409 idempotency_key_in_flight`, not a duplicate row. Also: `SYSTEM_DATABASE_URL`'s fail-fast (DLG-D34) now triggers outside any `isDevLikeEnvironment` name, not only a literal `"production"` — don't reintroduce a bare `== "production"` check when touching `cmd/server/config.go`.

# Extending the Service

- **New endpoint:** add the handler in `internal/adapter/inbound/http/`, register the route (with the correct GUC-binding middleware — see design decision 6 above) in `router.go`, add the service method in `internal/core/service/`, add the repository method to the relevant `internal/core/port/` interface and its `internal/adapter/outbound/postgres/` implementation. Run `make swag` and update the API overview table in `README.md`.
- **New domain event type:** fully documented in `CONTRIBUTING.md` § "Adding a new domain event type" — this repo has no `make extract-schemas` step (DLG-D20); `api/asyncapi.yaml` and `internal/eventschema/*.json` are both hand-maintained. Short version: add the constant to `internal/core/domain/event.go` (PascalCase — it IS the Glue name), the JSON schema file, add it to `eventschema.ByEventType`, extend `SCHEMA_NAME_MAP` in `.github/workflows/schema-registry.yml`, add `x-lifecycle`/`x-owner` to `api/asyncapi.yaml`, run `make schema-validate`.
- **New repository migration:** this repo ships a single consolidated migration so far, `000001_schema.{up,down}.sql`, under `internal/adapter/outbound/postgres/migrations/` — the next one is `000002_description.{up,down}.sql`. Since this service is still pre-deployment, schema fixes are folded back into `000001` rather than layered as new migrations; once deployed, start numbering forward instead. See `CONTRIBUTING.md` for the full checklist (`CREATE INDEX CONCURRENTLY` guidance, RLS test requirements).
- **New reconciler job (a fifth CronJob — four exist today: activation/expiry/review/cleanup):** add a new file to `cmd/reconciler/jobs/` following `delegation_expiry.go`'s shape (a function taking `*jobs.Context`, returning a result struct); wire the `--job=<name>` case in `cmd/reconciler/main.go`'s dispatch; if the job also needs an on-demand HTTP trigger (mirroring DLG-I1/I2), add a `RunX` method to `cmd/server/adapters.go`'s `reconcilerRunner` and a corresponding runner interface + handler in `internal/adapter/inbound/http/`; add the CronJob's schedule to `deploy/helm/iam-delegation/templates/`. Every job binds its own tenant GUC per-row via the injected `jctx.BindTenantGUC` (design decision 6) — never assume a GUC is already bound.
- **New environment variable:** documented in `CONTRIBUTING.md` § "Adding a new environment variable" — `.env.example`, a fail-fast check in `cmd/server/config.go`'s `loadConfig()` if required at startup, `README.md`'s env var table, and `deploy/helm/iam-delegation/values.yaml` if it needs a production default.

# Development Workflow

1. **New feature:** write a failing test alongside the code it covers (`*_test.go`, no separate `test/` tree here) → implement in the service layer → wire the handler → run `make swag` if the endpoint shape changed → `make test-rls` if it touches `delegations`/`delegation_tenant_settings`.
2. **DB schema change:** write the migration SQL in `internal/adapter/outbound/postgres/migrations/` → `make docker-up && make run` (the server self-migrates at startup, LLD §7.4/§16.4 — no separate migrate Job) → `make test-rls`.
3. **Event schema change:** additive fields only on the existing type (`"additionalProperties": true` stays); a breaking change needs a new versioned type (`X.v2`) per `api/asyncapi.yaml`'s `x-version-governance` block, not a mutation of the existing schema.
4. **Endpoint change:** update the handler + Swaggo annotations → `make swag` (checked-in output) → implement + test → update `README.md`'s API overview table.
5. **Cron/job change:** since DLG-I1/I2 share implementation between the HTTP route and the CronJob binary (design decision 7), a change to `cmd/reconciler/jobs/` affects both entry points — test via both `make run-reconciler JOB=...` and the HTTP route.

# Troubleshooting

**RLS test failure:** run `make test-rls` in isolation and check which of the three GUC-binding paths (design decision 6) is missing a bind — `tenantGUCMiddleware` not registered on a new public route, a new mesh-only handler not going through `gucBoundReader`, or a new reconciler/cascade call site not calling `jctx.BindTenantGUC`/the consumer's `GUCBinder`. A missing bind fails closed (zero rows), not with an error — it will look like the query is broken, not like an auth problem.

**Membership check unreachable during create:** `503 org_membership_unavailable` — the create fails cleanly, no partial write (DLG-FAIL-1). This is a **fail** response, not fail-open.

**User Profile unreachable — behavior differs by call site, don't assume symmetry:**
- **Create (DLG-2):** `503 user_profile_unavailable` — fails, no write.
- **Cancel (DLG-3):** fail-open — logs and proceeds; the availability pointer is left stale until the next expiry-cron tick re-clears it.
- **Expiry/review cron:** defer-and-retry — the row is left `active`, retried next tick (DEL-6). Not an error surfaced to any caller.

**`iam_delegation_*` metrics reading zero:** DLG-D19 is closed — every instrument has a real call site (service-layer counters via `internal/core/service/metrics.go`'s injected `Metrics` port, the three reconciler deferred counters via `cmd/reconciler/jobs.Context.Metrics` — GAP-27/DLG-D25 — and `active_gauge` via `cmd/server/exporters.go`'s 5-minute sysPool snapshot). A metric reading zero when its triggering condition is known to have occurred (e.g. deferred counters not moving on a sustained User Profile outage) is now a real bug, not a documented gap — check the wiring at the call site named above before assuming it's expected.

**Idempotency-Key replay:** a repeated key within 24h on `POST /delegations` returns the original `201` without creating a second row (`del:idem:{tenant}:{key}` in Valkey). Missing the header entirely is a 400 from `requireIdempotencyKey()` middleware, before the handler runs — not a 422 from the service layer. A second request that arrives *while the first is still in flight* (same key) gets `409 idempotency_key_in_flight` (DLG-D35), not a replay and not a duplicate row — the `Reserve`/`Release` pair in `internal/adapter/outbound/valkey/idempotency.go` is what makes this atomic; don't reintroduce a plain `Get`-then-`Save` on this path.

**Reassign "resurrecting" the old delegation:** it doesn't, deliberately (design decision 3, DLG-D11). If a reassign's new-delegation create fails after the old one ended, the old delegation stays ended — this is documented behavior, not a bug.

# Design patterns to follow

**Availability-first, never availability-after.** Every path that both touches `user_availability` (via `UserProfileClient`) and writes a `delegations` row does the UP call first, checks its result, and only then opens the `RunInTx` for the DB write + outbox enqueue. See `delegation_service.go`'s `Create`/`Cancel` and `cmd/reconciler/jobs/delegation_{expiry,review}.go`. Never reorder this to "write first, notify UP after" — it's what prevents `DelegationStarted`/`Ended` events without a matching availability pointer.

**Fail vs. fail-open vs. defer-and-retry are call-site-specific, not a single UP-down policy.** Don't generalize "what happens when User Profile is down" across the whole service — see Troubleshooting above for the three distinct behaviors and which call sites use which. Each is a deliberate LLD §11.1/§11.2/§11.3 decision, not an accident of implementation.

**GUC binding is always the caller's job, never the repository's.** `postgres.DelegationRepository`/`SettingsRepository` methods read whatever GUC is already on `ctx` — they never derive one from a `tenantID` parameter they receive (design decision 6, DLG-D18). Any new call path into these repositories must bind the GUC itself via one of the three established mechanisms before calling in.

**Dual entry point, one implementation, for cron-shaped work.** DLG-I1/I2's HTTP endpoints and the CronJob binary are not two independent implementations that happen to agree — `cmd/server/adapters.go`'s `reconcilerRunner` literally calls `cmd/reconciler/jobs`' functions (design decision 7). A fourth CronJob should follow the same shape if it also needs an on-demand HTTP trigger.

# Appendix — Error Codes

Full taxonomy from `internal/core/domain/errors.go`'s sentinels and `internal/adapter/inbound/http/errors.go`'s `errorStatusByCode` map (LLD §20 verbatim — a code absent from that map falls back to 500):

| Code | Status | Meaning |
|------|--------|---------|
| `validation_error` | 400 | Malformed request body/params, or missing `Idempotency-Key` header on create |
| `missing_identity_headers` | 401 | Gateway identity headers (`x-user-id`/`x-tenant-id`/`x-tenant-roles`) missing or invalid |
| `insufficient_role` | 403 | Caller lacks the role required for this route (DLG-3/4/5/7) |
| `optimistic_lock_conflict` | 409 | `record_version` mismatch on cancel/extend/reassign |
| `idempotency_key_in_flight` | 409 | `Idempotency-Key` already claimed by another in-flight (or, rarely, stale-`FindByID`) create (DLG-D35) |
| `invalid_delegation_scope` | 400 | `scope` not one of `all`/`department`/`tender` |
| `invalid_delegation_max_duration_days` | 400 | Tenant policy `max_duration_days` outside `[1,180]` |
| `invalid_delegation_review_window_days` | 400 | Tenant policy `review_window_days` outside `[1,180]` |
| `delegation_not_found` | 404 | `:id` does not exist, is cross-tenant, or is already terminal for an operation requiring active |
| `scope_id_required` | 422 | `scope` is `department`/`tender` but `scope_id` is missing |
| `invalid_scope_id` | 422 | `scope_id` malformed or not resolvable |
| `self_delegation` | 422 | `delegate_id` equals the delegator |
| `invalid_delegate` | 422 | Delegate not found or membership check reports inactive |
| `delegate_unavailable` | 422 | Delegate exists but fails an availability precondition |
| `reason_too_long` | 422 | `reason` exceeds 500 characters |
| `delegation_window_inverted` | 422 | `ends_at <= starts_at` |
| `delegation_window_too_long` | 422 | Span exceeds tenant's `max_duration_days` |
| `delegation_start_in_past` | 422 | `starts_at` before the allowed skew window |
| `delegation_start_too_far_future` | 422 | `starts_at` beyond the allowed 1-year window |
| `not_review_tracked` | 422 | Extend called on a fixed-`ends_at` (not open-ended) delegation |
| `extend_days_out_of_range` | 422 | `extend_days` outside `[1,180]` |
| `org_membership_unavailable` | 503 | Core's membership-existence check unreachable or timed out |
| `user_profile_unavailable` | 503 | User Profile's availability endpoint unreachable or timed out (create path only — see Design patterns above) |

---

**Document version:** 1.2 (updated for DLG-D35 — idempotency SETNX-reservation fix, widened `SYSTEM_DATABASE_URL` guard)

**Date:** 2026-09-05

**Audience:** Platform engineering, SRE, developers contributing to this service
