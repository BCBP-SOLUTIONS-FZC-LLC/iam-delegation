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
// Plain check-then-act — NOT a SetNX-based lock (unlike iam-user-profile's
// provision:lock:{key} race-serialization). A near-simultaneous duplicate
// request within the same few milliseconds can still race past the Get.
if rec, found, err := s.idempotency.Get(ctx, tenantID, idempotencyKey); err == nil && found {
    if existing, ferr := s.delegations.FindByID(ctx, tenantID, rec.DelegationID); ferr == nil {
        return existing, nil   // return the ORIGINAL delegation, not a new one
    }
}
```
Key: `del:idem:{tenantID}:{key}`, TTL 24h (`idempotencyTTL` constant). `Get`/`Save` both treat any Valkey error as "no hit / best-effort" — a Valkey outage degrades idempotency protection, it does not block create (LLD §9.3).

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

Every published envelope carries `specversion: "1"` (`eventbus.Publisher.EnqueueCtx` → `events.WithSchemaVersion("1")`, DLG-D24/DLG-D33) — matching `iam-realm-provisioner`. TraceID and CorrelationID are stamped from the OTel span when present. IP/UA are omitted when empty.

`outbox.Runner`'s own tunables (`PollInterval`/`BatchSize`/`MaxAttempts`/`DrainTimeout`/`PublishConcurrency`/`PublishTimeout`/`StartupJitter`/`ClaimLeaseDuration`) are `OUTBOX_*`-env-configurable (DLG-D24, matching `iam-org-membership`'s surface) rather than hardcoded. A fourth `cmd/server` background goroutine calls `outboxRunner.PrunePublished` on a ticker (`OUTBOX_PRUNE_INTERVAL`/`_RETENTION`/`_LIMIT`, default daily/7d/1000 rows, matching `iam-user-profile`'s `runMaintenanceSweep`) — without it, published `outbox_events` rows accumulate forever. `outbox_dead_letters_total` (the library's own dead-letter metric) is alerted on in both `deploy/helm/iam-delegation/templates/prometheusrule.yaml` and `deploy/monitoring/app-alerts.yml`.

## Consumed — `delegation-cascade-q` (one SQS queue, two upstream producers, three event types)

| Source topic | Event | Dispatched to | `processed_events.consumer` bucket |
|---|---|---|---|
| `iam.membership.events` | `MembershipRevoked` | `CascadeService.EndForUser` (§11.5) | `"cascade"` |
| `iam.membership.events` | `TenantMembershipsPurged` | `CascadeService.ScrubTenant` (§11.6) | `"offboarding"` |
| `iam.user.events` | `UserUpdated` (only when decoded `status=="disabled"`; other deliveries are acked without dispatch) | `CascadeService.EndForDisabledDelegate` (§11.5a, Bug 2/DLG-D26) | `"delegate_disable"` |

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
`eventbus.Publisher` (injected by `postgres.TxRunner`) owns enqueue. Missing schema is a pass-through (realm); `SchemaValidator` remains the fail-closed unit-test helper. `GlueCodec` pre-fetches each schema's latest version ID from Glue at startup; `events.NoopCodec` is the SNS pass-through when `GLUE_REGISTRY_NAME` is unset.

**CI governance pipeline naming reconciliation (this session's addition, `.github/workflows/schema-registry.yml`):** the JSON schema files on disk are snake_case (`delegation_started.json`) to satisfy `schema-gov validate`'s Pass 7 coverage rule, but the *registered* Glue names are PascalCase. This is purely a CI-script concern — the workflow's hand-rolled `aws glue get-schema`/`diff` steps need a `SCHEMA_NAME_MAP` bash associative array (`delegation_started → DelegationStarted`, etc.) to resolve the actual registered name, since a raw file-basename lookup would never find the real schema. This is **not** a runtime translation — `GlueCodec` never sees the snake_case filenames, only the PascalCase Go constants. See `docs/runbook-schema-registry.md` for the full CI pipeline and required IAM SIDs.

**Config vars:** `GLUE_REGISTRY_NAME` (registry `iam-delegation-events`; empty → `NoopCodec`), `GLUE_REGISTRY_ARN` (IAM policy scoping only, not read by the Go runtime), `AWS_REGION`.
