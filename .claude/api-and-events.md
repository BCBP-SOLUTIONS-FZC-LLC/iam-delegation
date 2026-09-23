# API Contract

**Routes under `/api/v1/delegations*`** (public) and `/internal/*` (mesh-only). Full endpoint table
already in `README.md § API overview` — this doc covers what's not there.

**Two route classes** (LLD §8.1/§8.2):
- **Public** (`/api/v1/delegations...`) — Envoy-fronted, gateway-injected `x-user-id`/`x-tenant-id`/`x-tenant-roles`. Gated by `tenantGUCMiddleware` (see `database.md § Row-Level Security`).
- **Internal** (`/internal/*`, DLG-I1..I4) — mesh-only mTLS trust boundary at the network layer (LLD §13.2). No RBAC/JWT check, no `ContextMiddleware` — there's no gateway identity to bridge. Reads go through `gucBoundReader` binding `userID="iam-system"` per call (DLG-D18).

**Identity model** — same as the sibling IAM services: trusts `x-user-id`/`x-tenant-id`/`x-tenant-roles` headers, never parses a JWT itself, never derives tenant/actor from the request body or path.

## Idempotency-Key (DLG-2, DLG-D8/DLG-Q3)

Enforced by middleware, not the service layer — `requireIdempotencyKey()` (`internal/adapter/inbound/http/middleware.go`), wired only on the create route (`router.go`: `public.POST("", requireIdempotencyKey(), delegationHandler.Create)`). Missing header → `400 validation_error` before the handler even runs.

Mechanism (`internal/adapter/outbound/valkey/idempotency.go`, `internal/core/service/delegation_service.go`'s `Create`):
```go
// Get fast path: a repeated key whose record is already "created" returns
// the original delegation without touching Reserve/membership checks/UP.
if rec, found, err := s.idempotency.Get(ctx, tenantID, idempotencyKey); err == nil && found {
    if rec.Status == "created" {
        if existing, ferr := s.delegations.FindByID(ctx, tenantID, rec.DelegationID); ferr == nil {
            return existing, nil   // return the ORIGINAL delegation, not a new one
        }
    } else {
        return nil, domain.NewError(domain.ErrIdempotencyKeyInFlight, "...") // "pending": another call holds it
    }
}
// SetNX-based claim (DLG-D35, replacing a plain check-then-act that could
// race — the pre-fix version of this doc called that gap out explicitly).
// A losing concurrent Reserve also returns ErrIdempotencyKeyInFlight (409).
if reserved, rerr := s.idempotency.Reserve(ctx, tenantID, idempotencyKey); rerr == nil {
    if !reserved {
        return nil, domain.NewError(domain.ErrIdempotencyKeyInFlight, "...")
    }
    defer func() { if err != nil { _ = s.idempotency.Release(ctx, tenantID, idempotencyKey) } }()
}
// rerr != nil (Valkey down): proceed unreserved, same best-effort philosophy as Get/Save.
```
Key: `del:idem:{tenantID}:{key}`, TTL 24h (`idempotencyTTL` constant, shared by the `"pending"` reservation placeholder and the final `"created"` record — a successful `Save` simply overwrites the reservation under the same key). `Get`/`Save`/`Reserve`/`Release` all treat any Valkey error as "no hit / best-effort, proceed unreserved" — a Valkey outage degrades idempotency protection, it does not block create (LLD §9.3). A losing concurrent caller — whether it lost to `Reserve` or found a `"pending"` record on `Get` — gets `409 idempotency_key_in_flight` (`domain.ErrIdempotencyKeyInFlight`), not a duplicate delegation. A failed create after a successful `Reserve` calls `Release` so a client retry with the same key isn't stuck waiting out the 24h TTL.

## `record_version` — query param vs body field (verify per-endpoint, they differ)

| Endpoint | Where `record_version` lives |
|---|---|
| DLG-3 Cancel (`DELETE /delegations/:id`) | **Query param** `?record_version=N` — `delegation_handler.go` parses via `strconv.ParseInt(c.Query("record_version"), 10, 64)`; malformed/missing defaults to `0` (fails the version check rather than erroring on parse) |
| DLG-4 Extend | **Body field**, alongside `extend_days` |
| DLG-5 Reassign | **Body field** |
| DLG-7 Set settings | **Body field** |

## Error envelope — flat shape, not the hybrid legacy shape

`internal/adapter/inbound/http/errors.go`'s `writeError` emits `gincommon.ErrorResponse{Error, Status, TraceID, RequestID}` — **no** `code`/`message`/`details` legacy fields (unlike `iam-user-profile`'s hybrid envelope). `domain.Error.Details` exists in Go (e.g. `record_version` on a conflict, `max_duration_days` on a window-too-long) but **is never serialized into the response** — `writeError` doesn't read it. If a client needs those details, that's a real gap, not a documented feature; don't assume `Details` reaches the wire.

```json
{ "error": "optimistic_lock_conflict", "status": 409, "trace_id": "...", "request_id": "..." }
```

`errorStatusByCode` (`errors.go`) maps all 20 `domain.Err*` sentinels to their HTTP status per LLD §20. Any code not in the map (there shouldn't be one) falls back to 500. Full taxonomy:

| Code | Status | Sentinel |
|---|---|---|
| `validation_error` | 400 | `ErrValidation` |
| `invalid_delegation_scope` | 400 | `ErrInvalidDelegationScope` |
| `invalid_delegation_max_duration_days` | 400 | `ErrInvalidDelegationMaxDurationDays` |
| `invalid_delegation_review_window_days` | 400 | `ErrInvalidDelegationReviewWindow` |
| `missing_identity_headers` | 401 | `ErrMissingIdentity` |
| `insufficient_role` | 403 | `ErrInsufficientRole` |
| `delegation_not_found` | 404 | `ErrDelegationNotFound` |
| `optimistic_lock_conflict` | 409 | `ErrOptimisticLockConflict` |
| `scope_id_required` / `invalid_scope_id` | 422 | `ErrScopeIDRequired` / `ErrInvalidScopeID` |
| `self_delegation` | 422 | `ErrSelfDelegation` |
| `invalid_delegate` / `delegate_unavailable` | 422 | `ErrInvalidDelegate` / `ErrDelegateUnavailable` |
| `reason_too_long` | 422 | `ErrReasonTooLong` |
| `delegation_window_inverted` / `_too_long` | 422 | `ErrDelegationWindowInverted` / `ErrDelegationWindowTooLong` |
| `delegation_start_in_past` / `_too_far_future` | 422 | `ErrDelegationStartInPast` / `ErrDelegationStartTooFarFuture` |
| `not_review_tracked` | 422 | `ErrNotReviewTracked` |
| `extend_days_out_of_range` | 422 | `ErrExtendDaysOutOfRange` |
| `org_membership_unavailable` | 503 | `ErrOrgMembershipUnavailable` |
| `user_profile_unavailable` | 503 | `ErrUserProfileUnavailable` |
| `catalog_admin_unavailable` | 503 | `ErrCatalogAdminUnavailable` — only on DLG-2 Create when `CATALOG_ADMIN_BASE_URL` is set and `scope=department`; 5xx/transport failure from Catalog Admin CAT-7 |

# Caching (LLD §9)

Valkey `del:` keyspace, two keys only — no cross-service config cache (tenant policy is a local table) and no I-8 projection (Option C dropped that entirely):

| Key | Value | TTL | Invalidated by |
|---|---|---|---|
| `del:list:{tenant}:{delegator}` | DLG-1 list projection | 60s | DLG-2/3/4/5 for that delegator (best-effort DEL) |
| `del:idem:{tenant}:{key}` | `{delegation_id, status}` for a create idempotency key | 24h | TTL only |

Read algorithm (DLG-1, `DelegationService.List`):
```go
if cached, hit := s.cache.GetDelegatorList(ctx, tenantID, delegatorID); hit { return cached, nil }
list, _ := s.delegations.ListByDelegator(ctx, tenantID, delegatorID)
s.cache.SetDelegatorList(ctx, tenantID, delegatorID, list)
return list
```
Cache is advisory — correctness never depends on it (LLD §9.3); a Valkey outage degrades to always-DB-read, not an error.

# Events (LLD §10) — this service is both producer AND consumer

Unlike most O&M-extraction siblings, this is a structural highlight of the service (see `README.md § Mental model`).

## Published — `iam.delegation.events` (one dedicated SNS topic, four types, no RoutingPublisher)

| Event | Emitted when | Consumers (LLD §10.5) |
|---|---|---|
| `DelegationStarted` | DLG-2 create, create-leg of DLG-5 reassign | **Workflow Service** (reroute — authoritative signal), Notification, Audit |
| `DelegationEnded` | DLG-3 cancel, `ends_at` expiry (DLG-I1), review auto-end (DLG-I2), end-leg of DLG-5 (`ended_reason=reassigned`), delegate-removed cascade (§11.5), delegate-disabled cascade (§11.5a, Bug 2) — `ended_reason ∈ {expired, cancelled, reassigned, delegate_removed, review_expired, delegate_disabled}` | **Workflow Service** (restore), Notification, Audit |
| `DelegationReviewRequested` | `delegation-review` sweep — once per calendar day for each of the 3 days before `review_due_at` — `days_remaining ∈ {3,2,1}` | Notification, Audit |
| `DelegationEscalationRequested` | Immediately after `DelegationEnded`, same tx, only when `ended_reason=delegate_disabled` (§11.5b, Bug 2a) | Notification (tenant_admin/tenant_owner only), Workflow Service (hook, not yet consumed — cross-team), Audit |

Cron-origin events carry system sentinels: `ip_address: "system"`, `user_agent: "iam-delegation/<job>-cron"`, `actor: SystemActorID`. The delegator-side end of a removal cascade is intentionally **silent — no event** (DEL-7 asymmetry, DLG-EVT-4) — only the delegate-side row emits `DelegationEnded`; this silence rule does not apply to the delegate-disabled cascade, where every ended row is delegate-side by construction and emits both `DelegationEnded` and `DelegationEscalationRequested`.

Every published envelope carries `specversion: "1"` (`eventbus.Publisher.EnqueueCtx` → `events.WithSchemaVersion("1")`, DLG-D24/DLG-D33) — matching `iam-user-profile` / `iam-org-membership`. TraceID and CorrelationID are stamped from the OTel span when present. IP/UA are omitted when empty. `eventbus.Publisher` reads the `RunInTx` pgx.Tx via `port.TxFromContext` (not the postgres adapter) and calls `outbox.Enqueue`.

`outbox.Runner` tunables come from `platform-events` `config.LoadOutbox` → `RunnerConfigFromEnv` (DLG-D44, matching `iam-user-profile` / `iam-org-membership`). Helm / `.env.example` keep the historical `OUTBOX_*` values (`500ms` poll, concurrency 4, 2s jitter, 10m claim-lease); library defaults apply only when those env vars are unset — `cmd/server/config.go`'s `ensureOutboxEnv` backfills them on any invocation path that doesn't set them, so behavior never silently regresses to the library's own (different) defaults. A `cmd/server` background goroutine calls `outboxRunner.PrunePublished` on a ticker (`OUTBOX_PRUNE_INTERVAL`/`_RETENTION`/`_LIMIT`, default daily/7d/1000 rows, matching `iam-user-profile`'s `runMaintenanceSweep`) — `LoadOutbox` does not cover prune. `outbox_dead_letters_total` (the library's own dead-letter metric) is alerted on in both `deploy/helm/iam-delegation/templates/prometheusrule.yaml` and `deploy/monitoring/app-alerts.yml`.

CI-enforced (`.github/scripts/check-forbidden-events-bypass.sh`): no direct `aws-sdk-go-v2/service/{sns,sqs}` import outside `cmd/server/main.go` (which only constructs the raw client to hand to `events.NewSNSPublisher`/`events.NewSQSConsumerWithClient`), no direct call to an SNS/SQS transport method anywhere, and no hand-built `events.Envelope{}` literal (always via `events.NewEnvelope`). Consumer-side dedup (`processed_events`, above) is the one deliberate exception to "platform-events only" — that library has no consumer-side idempotency mechanism, only the publish-side, SNS-FIFO-only `events.WithMessageDeduplicationID`.

Also CI-enforced (`.github/scripts/check-outbox-access.sh`, DLG-D47): `outbox_events` itself is never read/written via hand-rolled SQL — only `outbox.Enqueue` (write) and `outbox.Runner.PrunePublished` (delete) — scanning Go backtick string literals in `internal/`/`cmd/`/`pkg/` for `from|into|update outbox_events` outside `internal/adapter/outbound/eventbus`. Ported from `iam-org-membership`, which hit this exact bypass once (a hand-rolled batched DELETE duplicating `PrunePublished`) before removing it; this service has no history of the violation, but the same architectural gap applies equally here.

## Consumed — `delegation-cascade-q` (one SQS queue, two upstream producers, three event types)

| Source topic | Event | Dispatched to | `processed_events.consumer` bucket |
|---|---|---|---|
| `iam.membership.events` | `MembershipRevoked` | `CascadeService.EndForUser` (§11.5) | `"cascade"` |
| `iam.membership.events` | `TenantMembershipsPurged` | `CascadeService.ScrubTenant` (§11.6) | `"offboarding"` |
| `iam.user.events` | `UserUpdated` (only when decoded `status=="disabled"`; other deliveries are acked and recorded in `processed_events` without dispatch) | `CascadeService.EndForDisabledDelegate` (§11.5a, Bug 2/DLG-D26) | `"delegate_disable"` |

The first two event types are produced by Core / Org & Membership on `iam.membership.events`. `TenantMembershipsPurged` was renamed from `TenantOffboarded` by Core to avoid colliding with Realm Provisioner's own, differently-scoped `TenantOffboarded` event on `iam.tenant.events`, which this service does not consume. `UserUpdated` is produced by User Profile on a third, distinct topic — `iam.user.events` — a second SNS subscription onto this same queue (Bug 2/DLG-D26); its filter policy can only match on `EventType`, not payload content, so this service decodes every delivery and dispatches only on `status=="disabled"`.

Three distinct idempotency buckets (not one hardcoded consumer name like `iam-user-profile`'s single-subscription case) — `internal/adapter/inbound/consumer/cascade_consumer.go`'s `consumerCascade = "cascade"` / `consumerOffboarding = "offboarding"` / `consumerDelegateDisable = "delegate_disable"`, keyed into `processed_events (event_id, consumer)`. DLQ: `delegation-cascade-q-dlq`, `maxReceiveCount=5`.

# AWS Glue Schema Registry — `iam-delegation-events`

**Key difference from `iam-user-profile`: no runtime name translation.** User-profile's `domain.GlueSchemaName(eventType)` maps dot-notation event types (`user.provisioned`) to PascalCase Glue names (`UserProvisioned`) via an explicit switch. **This repo's event-type constants (`internal/core/domain/event.go`) already ARE the PascalCase Glue schema name** — `DelegationStarted`, `DelegationEnded`, `DelegationReviewRequested`, `DelegationEscalationRequested` — used verbatim as both the envelope `type` and the Glue `SchemaName`, no translation function exists or is needed (`internal/adapter/outbound/eventbus/codec.go`'s doc comment confirms this explicitly).

**Wire format** — identical 18-byte header mechanism to every sibling service:
```
byte 0     : 0x03  (header version)
byte 1     : 0x00  (no compression)
bytes 2-17 : schema version UUID (big-endian)
```

**Encoding flow** — same architecture as iam-realm-provisioner: validate at enqueue time (plain JSON via ValidatingCodec), encode at publish time (never touching Postgres):
```
json.Marshal(payload) → ValidatingCodec.Encode (schema pass-through if unregistered) → events.NewEnvelope → outbox.Enqueue(ctx, tx, env)
                                                                                              ↓
                                                                                outbox.Runner polls outbox_events
                                                                                              ↓
                                              events.WithCodec(GlueCodec|events.NoopCodec) → codec.Encode(...) → SNS Publish
```
`eventbus.Publisher` (injected by `postgres.TxRunner`) owns enqueue and takes `events.Codec` — `ValidatingCodec` wrapping `events.NoopCodec` (no local enqueue Codec). Missing schema is a pass-through (realm); `SchemaValidator` remains the fail-closed unit-test helper. `GlueCodec` resolves each schema's version ID once at startup **by definition** (`GetSchemaByDefinition` with this binary's embedded `eventschema.ByEventType` file, compact + ASCII-escaped exactly as `schema-gov register` uploads it; must be `AVAILABLE`) — no background refresh, so it always stamps the version it produces, never merely the latest; an unregistered definition fails startup (DLG-D50); `events.NoopCodec` is the SNS pass-through when `GLUE_REGISTRY_NAME` is unset.

**Schema files and names (DLG-D50/D49):** `api/asyncapi.yaml` is the single source — `make extract-schemas` generates `internal/eventschema/*.json` (4 produced + 3 consumed payload schemas) and CI's `extract --check` fails on drift. The files are snake_case (`delegation_started.json`, Pass 7's convention) but schema-gov names a schema after its file stem, while Glue names, asyncapi message names and the `EventType` metric label are PascalCase — so CI's `usage-check`/`diff`/`register` steps (and `make schema-register`) all read a PascalCase, produced-only copy staged by `.github/scripts/stage-produced-event-schemas.sh` into `.tmp/glue-schemas` (it skips the consumed files, which are never registered here). `GlueCodec` never sees file names — only the PascalCase Go constants and the embedded bytes. **Consumer-side validation:** `CascadeConsumer` validates each `MembershipRevoked`/`TenantMembershipsPurged`/`UserUpdated` payload against `eventschema.Consumed` (`eventbus.ConsumedValidator`, injected as a `PayloadValidator`) before dispatch; a violation is logged, counted on `iam_delegation_cascade_dlq_total`, not marked processed, and returned so redrive moves it to the DLQ. Consumed schemas match the producers' required fields/types, never stricter (`actor_id` optional; no `changed_fields` enum). See `docs/runbook-schema-registry.md`.

**Config vars:** `GLUE_REGISTRY_NAME` (registry `iam-delegation-events`; empty → `NoopCodec`), `GLUE_REGISTRY_ARN` (IAM policy scoping only, not read by the Go runtime), `AWS_REGION`.
