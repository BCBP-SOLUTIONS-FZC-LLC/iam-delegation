# Key Request Flows

Full sequence diagrams for every flow live in `docs/lld/iam-lld-delegation-service.md` §11 (also
extracted into `docs/architecture/mermaid/*.mmd` and embedded in `ARCHITECTURE.md` § Key request
flows). This section covers the non-obvious ordering rules and gotchas a diagram doesn't spell out —
grounded in the actual code (`internal/core/service/*.go`, `cmd/reconciler/jobs/*.go`), not just the LLD.

## Create (DLG-2) — `DelegationService.Create`

1. **Idempotency check first** — `idempotency.Get(tenantID, key)`; a hit returns the original
   delegation via `FindByID` without re-running any validation or side effect below. Only checked
   when a key is present.
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
7. Idempotency record save and cache invalidation happen **after** the tx commits, best-effort
   (`//nolint:errcheck` — a failed idempotency-save must never fail an already-committed create).

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

Ends every row where the removed user is delegator OR delegate, inside one tx, but **event emission
is asymmetric by design (DEL-7/DLG-EVT-4)**: only delegate-side rows get `DelegationEnded
{delegate_removed}` enqueued — `if d.DelegateID != userID { continue }` skips enqueue entirely for
delegator-side rows. Read this in `cascade_service.go` directly; it's easy to assume both sides fire
an event. The User Profile pointer-clear for each affected delegator happens **after the tx commits**,
outside the transaction, and is fire-and-forget (`//nolint:errcheck`) — the row is already ended and
inert once the user has no membership (§7.6.5), so there's nothing to retry against; a failed clear
here is not revisited by any cron (the expiry cron only scans still-*active* rows).

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
`pgcommon.RunInTx` (`internal/adapter/outbound/postgres/db.go`). **This repo has no
`RunInTxWithRetry`-equivalent** — unlike some sibling services, there is no automatic retry on
Postgres serialization failures (`40001`/`40P01`); a serialization conflict surfaces as a plain error
to the caller. Don't assume retry-on-conflict semantics exist here without adding them.

## Shutdown ordering (`cmd/server/main.go`)

The real order, inside an `errgroup` with one goroutine per: outbox runner, SQS consumer, the
outbox-prune sweep (DLG-D24), HTTP server, and a shutdown-trigger goroutine that fires on
`gCtx.Done()`:

1. `httpServer.Shutdown(shutdownCtx)` — stop accepting new requests, drain in-flight (30s budget).
   `shutdownCtx` is deliberately built from `context.Background()`, not derived from `gCtx` (which is
   already `Done()` at this point — deriving from it would give the shutdown sequence zero time to run).
2. `outboxRunner.Stop()` — flush in-flight event publishes.
3. `sqsConsumer.Stop()` — wait for in-flight cascade-consumer handlers.
4. `g.Wait()` returns → the deferred `redisClient.Close()` in `run()` fires last.

The outbox-prune goroutine has no explicit `Stop()` call in step 1-3: it's a plain
`select { case <-gCtx.Done(): return nil; case <-ticker.C: ... }` loop, so it exits on its own the
moment `gCtx` cancels (before the shutdown-trigger goroutine even starts running its steps) — there
is nothing in-flight for it to drain, unlike the outbox runner/SQS consumer's explicit `Stop()`s.

Since DLG-D24, this repo *does* have one long-running background sweep goroutine inside
`cmd/server` (the outbox-prune sweep, matching `iam-user-profile`'s `runMaintenanceSweep`) — but
expiry/review/cleanup are still cron-triggered externally (CronJob → HTTP or CronJob binary), not
long-running goroutines inside `cmd/server`; only the outbox prune runs as a `cmd/server` ticker.
