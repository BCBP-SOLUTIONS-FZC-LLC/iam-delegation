# Key Request Flows

Full sequence diagrams for every flow live in `docs/lld/iam-lld-delegation-service.md` §11 (also
extracted into `docs/architecture/mermaid/*.mmd` and embedded in `ARCHITECTURE.md` § Key request
flows). This section covers the non-obvious ordering rules and gotchas a diagram doesn't spell out —
grounded in the actual code (`internal/core/service/*.go`, `cmd/reconciler/jobs/*.go`), not just the LLD.

## Create (DLG-2) — `DelegationService.Create`

1. **Idempotency check first** — `idempotency.Get(tenantID, key)`; a hit whose record is already
   `"created"` returns the original delegation via `FindByID` without re-running any validation or
   side effect below. A hit whose record is still `"pending"` (another call holds it) instead
   returns `409 idempotency_key_in_flight` immediately. On any miss (or no key present), the key is
   atomically claimed via `Reserve` (`SETNX`, DLG-D35) before step 2 runs — a losing `Reserve` also
   returns `409 idempotency_key_in_flight`. A reservation is released (`Release`) if any later step
   fails, so a retry with the same key isn't stuck for the 24h TTL; a Valkey-down `Reserve` error
   degrades to best-effort (proceed unreserved), not a failure. This closes a real race the original
   `Get`-then-`Save` implementation left open: two concurrent same-key creates could both miss the
   `Get` and both reach step 6 below.
2. **Pre-flight validation** — self-delegation, scope/`scope_id` consistency, `reason` ≤500 chars,
   `starts_at` skew tolerance (±5s) and 1-year future bound, `ends_at > starts_at`.
3. **Tenant bounds** — `delegation_tenant_settings.Get` read in-process (DLG-D2, no cross-service
   call); `ends_at - starts_at` checked against `max_duration_days`.
4. **Two membership checks run CONCURRENTLY**, not sequentially — `checkBothMemberships` launches
   two goroutines (delegator, delegate) over unbuffered-result channels and blocks on both before
   proceeding. Either erroring → `503 org_membership_unavailable`; either inactive → `422
   invalid_delegate`. This is a real concurrency pattern, not just two `await`s in sequence.
5. **User Profile call happens AFTER both membership checks succeed, BEFORE the DB write**
   (availability-first, DEL-6) — `SetAvailability` with `status=ooo`. A `port.ErrDependencyUnavailable`
   → `503 user_profile_unavailable`; a `delegate_unavailable` substring match → `422
   delegate_unavailable` (the delegate is themselves OOO); any other error → `422 invalid_delegate`.
6. **Only then** `RunInTx{ Insert; enqueue DelegationStarted }` — the DB write and event enqueue are
   the LAST step, after every external dependency has already succeeded. If the tx fails, User
   Profile has already been told the user is OOO — this is a known, accepted small inconsistency
   window (LLD's availability-first tradeoff), not a two-phase-commit.
7. Idempotency record save (overwriting step 1's `"pending"` reservation with the final `{delegation_id, "created"}` record, same key/TTL) and cache invalidation happen **after** the tx commits, best-effort
   (`//nolint:errcheck` — a failed idempotency-save must never fail an already-committed create).

## Activation cron (`delegation-activation`, DLG-D25) — `jobs.Activation`

Mirrors Expiry's shape exactly but runs the opposite transition, `scheduled → active`, closing the
gap `Create` (above) opens: a genuinely future `starts_at` skips the User Profile call and
`DelegationStarted` entirely at create time, inserting the row as `scheduled`.

1. `ListScheduledBefore` finds rows `WHERE status='scheduled' AND starts_at <= now()`.
2. Per row, User Profile's `SetAvailability` is called first (same availability-first ordering as
   Create) — a failure leaves the row `scheduled`, increments
   `iam_delegation_activation_deferred_total`, and retries next tick; it does **not** proceed to
   activate on a UP failure, mirroring Expiry's defer-and-retry pattern exactly.
3. Only on UP success does `Activate` run inside a tenant-GUC-bound tx (`UPDATE status='active'
   WHERE status='scheduled' AND record_version=$expected`, then enqueue `DelegationStarted`) — the
   optimistic-lock guard means a row that already raced to another terminal/active state returns
   `nil`, not an error, counted toward neither `Succeeded` nor `Failed`.
4. `OOOUntil` sent to User Profile is `d.EndsAt`, falling back to `d.ReviewDueAt` for open-ended
   rows — the identical DLG-D29 fix applied at Create's own `SetAvailability` call site, found at
   a second independent call site during a follow-up audit. Get this wrong here and an open-ended
   scheduled delegation defers activation forever, resending the same rejected payload every tick.

## Cancel (DLG-3) — `DelegationService.Cancel`

**Asymmetric with Create**: the User Profile call here is fail-open, not fail-closed. `SetAvailability`
with `ClearDelegate: true` is called and its error is explicitly discarded
(`//nolint:errcheck // fail-open by design`) — cancellation proceeds to `RunInTx{ End; enqueue
DelegationEnded{cancelled} }` regardless of whether the pointer-clear succeeded. Rationale (LLD
§11.2): the delegation-expiry cron re-clears any stale pointer on its next tick (every 5 min), so a
failed clear here is self-healing, not a correctness gap — unlike Create, where an availability
failure would mean routing work to someone already OOO.

## Extend (DLG-4) — `DelegationService.Extend`

Open-ended only (`ends_at IS NULL`; a fixed-`ends_at` row → `422 not_review_tracked`).
`extend_days` (if given) must be `[1,180]`. Window-days precedence: **caller's `extend_days` >
per-delegation `ReviewWindowDays` override > tenant `delegation_tenant_settings` default** — read
this order directly from `Extend`'s code, not just the LLD prose, since it's easy to get backwards.
No User Profile call — extend never touches availability.

## Reassign (DLG-5) — `DelegationService.Reassign`

**Literally composed from Cancel then Create** — `Reassign` calls `s.Cancel(...)` then `s.Create(...)`
verbatim, not a duplicated code path. Confirmed in `delegation_service.go`: if `Create` fails after
`Cancel` already committed, **the old delegation is NOT resurrected** (DLG-D11) — the caller sees the
`Create` error and the tenant is left with zero active delegations from that delegator until they
retry. All reassign body fields are optional and default to the existing delegation's values;
`ScopeIDSet`/`EndsAtProvided` are explicit "was this JSON key present" flags (not just nil checks) so
the HTTP DTO layer can distinguish "omitted" from "explicitly null" — get this wrong and a reassign
that means to keep the current `scope_id` will instead clear it.

## Expiry cron (DLG-I1) and Review-window cron (DLG-I2)

Both jobs live in `cmd/reconciler/jobs/` **and** are reachable via `POST
/internal/delegations/{expire,review-sweep}` HTTP endpoints (DLG-D17) — `cmd/server/adapters.go`'s
`reconcilerRunner` wraps `jobs.Expiry`/`jobs.ReviewSweep` to satisfy the HTTP handler's
`ExpiryRunner`/`ReviewRunner` interfaces, so the CronJob binary and the on-demand HTTP trigger call
the exact same implementation — not two independently-maintained code paths.

- **Expiry**: per candidate, User Profile pointer-clear first; on failure, `Deferred++`, the row
  is left active for the next tick (DEL-6 self-retry), and `iam_delegation_expiry_deferred_total` is
  incremented (GAP-27) — **it does not proceed to end the row on a UP failure**. Only after a
  successful clear does it open a tenant-GUC-bound tx (`jctx.BindTenantGUC`) to `End` + enqueue
  `DelegationEnded{expired}`. A concurrent end/cancel racing the same row
  (`ErrDelegationNotFound`/`ErrOptimisticLockConflict`) is treated as `raced`, counted toward neither
  `Succeeded` nor `Failed`.
- **Review-window**: two passes per tick, each independently GUC-bound per row — daily cascade warn
  (`FindDueForDailyWarn`, `review_due_at ∈ (now, now+3d]`; `days_remaining = CEIL((review_due_at−now())/1day)`
  clamped to `[1,3]`, skipped when `review_last_warned_bucket` already equals that value — one notice
  per calendar-day mark, 3 → 2 → 1 across consecutive daily ticks), then auto-end (`FindDueForAutoEnd`,
  `review_due_at <= now`). Both finder queries filter `ends_at IS NULL` — a fixed-end-date delegation is
  never considered by this cron at all. `MarkReviewWarned` marks-then-enqueues inside one tx per row; a
  race on the mark is skipped, not failed. Auto-end follows the same User-Profile-first-then-defer
  pattern as Expiry, and increments `iam_delegation_review_deferred_total` on every defer (GAP-27).
- Cron-origin events are stamped with `ip_address="system"` and
  `user_agent="iam-delegation/<cron-name>-cron"` (`enqueueEvent` in `cmd/reconciler/jobs`) instead of
  a requestctx-derived actor — distinct from the HTTP-path `enqueue` helper in
  `internal/core/service`, which pulls `ClientIP`/`UserAgent` from `requestctx.FromContext`.

## Cascade removal — `CascadeService.EndForUser` (`MembershipRevoked`)

Ends every row where the removed user is delegator OR delegate — **both `active` and `scheduled`
rows** since DLG-D25 broadened the terminal-state check, so a scheduled delegation for a member who
leaves before `starts_at` is cancelled rather than left to activate against a departed member —
inside one tx, but **event emission is asymmetric by design (DEL-7/DLG-EVT-4)**: only delegate-side
rows get `DelegationEnded{delegate_removed}` enqueued — `if d.DelegateID != userID { continue }`
skips enqueue entirely for delegator-side rows. Read this in `cascade_service.go` directly; it's
easy to assume both sides fire an event. The User Profile pointer-clear for each affected delegator
happens **after the tx commits**, outside the transaction, and is fire-and-forget
(`//nolint:errcheck`) — the row is already ended and inert once the user has no membership (§7.6.5),
so there's nothing to retry against; a failed clear here is not revisited by any cron (the expiry
cron only scans still-*active* rows).

## Cascade removal — `CascadeService.EndForDisabledDelegate` (`UserUpdated{status:disabled}`, Bug 2/DLG-D26)

**A separate inbound signal from `EndForUser` above, not a variant of it** — "disabled" is a status
flip within the tenant, not a departure, so this reacts to a different upstream topic
(`iam.user.events`, User Profile) via a second SNS subscription onto the same `delegation-cascade-q`.
`CascadeConsumer.Handle` decodes every `UserUpdated` delivery and dispatches only when
`status == "disabled"` — the SNS filter policy can only match on `EventType`, not payload content,
so most deliveries are unrelated field changes that get acked without dispatch, not an error.

Ends every active/scheduled delegation where the disabled user is the **delegate** (never the
delegator — a disabled delegator's own OOO state is User Profile's concern, not this service's) —
`deleted_at` is deliberately **left unset** (unlike `EndForUser`'s hard soft-delete): the user is
still a tenant member, so the row stays a normal historical record. **No User Profile pointer-clear
call is made** — User Profile's own disable flow already clears the delegate pointer atomically,
inside the same transaction that publishes the very `UserUpdated` event this consumer reacts to, so
by the time it's seen, User Profile's side is guaranteed done; calling `SetAvailability` again here
would be redundant, not a missing safety net.

Every ended row here enqueues **both** `DelegationEnded{delegate_disabled}` and
`DelegationEscalationRequested{delegate_disabled}` in the same per-row transaction (Bug 2a/DLG-D27)
— unlike `EndForUser`'s delegator-side silence (DLG-EVT-4), there is no silent side here: every row
is delegate-side by construction, so both events always fire together. **Pre-deploy gap:** the
`iam.user.events` SNS subscription this flow depends on is documented but not yet provisioned in any
environment — this code path is fully implemented and tested but will never run until platform/infra
adds it.

## Cascade removal — `CascadeService.ScrubTenant` (`TenantMembershipsPurged`)

Two plain soft-delete calls (`delegations.SoftDeleteTenant`, `settings.SoftDeleteTenant`), no event
emission at all — the LLD reasons that soft-deleted rows are inert, so nothing downstream needs to
react. Not wrapped in a `RunInTx` — the two deletes are independent statements, not required to be
atomic with each other.

## Cleanup job (`delegation-cleanup`, monthly) — `jobs.Cleanup`

`HardPurgeSoftDeletedBefore(now - RetentionDays)`, default 90 days (LLD §18.4). No GUC binding at
all — this is a cross-tenant DELETE against the BYPASSRLS pool by design, same as the cron finder
queries. No event emission (pure storage retention).

# Concurrency & Consistency

## Optimistic concurrency

`record_version` (monotonic counter, not a timestamp) gates every mutating repository call —
`End`/`ExtendReview`/`MarkReviewWarned` all take an `expectedVersion` and the underlying SQL is a
`WHERE id = $1 AND record_version = $2` predicate; zero rows affected surfaces as
`ErrOptimisticLockConflict` up through the service layer (→ HTTP `409`). The cron paths treat this
race (`errors.Is(err, domain.ErrOptimisticLockConflict)`) as a benign no-op (`raced = true`), not an
error — a concurrent admin cancel or another tick already reached the desired end-state.

## Transaction discipline

Every write that emits an event goes through `port.TxRunner.RunInTx`, backed by
`pgcommon.RunInTxWithRetryOpts` (`internal/adapter/outbound/postgres/db.go`, DLG-D32,
matching `iam-realm-provisioner`). Deadlock (`40P01`) and serialization failure (`40001`)
retry up to 3 times with exponential backoff + jitter (10ms–500ms). Nested `withPool`
joins (already inside a tx) do not retry — the outer `TxRunner` owns the attempt.

## Shutdown ordering (`cmd/server/main.go`)

The real order, inside an `errgroup` with **six** real background goroutines — outbox runner, SQS
consumer, the outbox-prune sweep (DLG-D24), the `iam_delegation_active_gauge` exporter
(`runActiveGaugeExporter`, `cmd/server/exporters.go`), the HTTP API server, and a dedicated
`:METRICS_PORT` metrics server — plus a shutdown-trigger goroutine that fires on `gCtx.Done()`:

1. `httpServer.Shutdown(shutdownCtx)` — stop accepting new requests, drain in-flight (30s budget).
   `shutdownCtx` is deliberately built from `context.Background()`, not derived from `gCtx` (which is
   already `Done()` at this point — deriving from it would give the shutdown sequence zero time to run).
2. `outboxRunner.Stop()` — flush in-flight event publishes.
3. `sqsConsumer.Stop()` — wait for in-flight cascade-consumer handlers.
4. `g.Wait()` returns → the deferred `redisClient.Close()` in `run()` fires last.

The outbox-prune goroutine and the active-gauge exporter both have no explicit `Stop()` call in
step 1-3: each is a plain `select { case <-gCtx.Done(): return nil; case <-ticker.C: ... }` loop, so
both exit on their own the moment `gCtx` cancels (before the shutdown-trigger goroutine even starts
running its steps) — there is nothing in-flight for either to drain, unlike the outbox
runner/SQS consumer's explicit `Stop()`s. The dedicated metrics server is a second `http.Server`
with no drain call of its own in this sequence either — it is expected to be scraped, not to serve
in-flight request bodies that need draining the way the API listener does.

Since DLG-D24/DLG-D31, this repo has **two** long-running background sweep/exporter goroutines
inside `cmd/server` beyond the outbox runner and SQS consumer (the outbox-prune sweep, matching
`iam-user-profile`'s `runMaintenanceSweep`, and the active-gauge exporter) — but
activation/expiry/review/cleanup are still cron-triggered externally (CronJob → HTTP or CronJob
binary), not long-running goroutines inside `cmd/server`.
