# iam-delegation

Out-of-office delegation for the IAM subsystem: who handed off their work to whom, over what
scope, for how long, and why it ended.

**Module:** Go 1.26.6 (pinned exactly) · private module · not yet deployed to any live
environment — no Git tag has ever been pushed (see [`VERSIONING.md`](VERSIONING.md)).

## Mental model

| | |
|---|---|
| **Owns** | `delegations` / `delegation_tenant_settings` tables; the DLG-1..7 public API; the DLG-I1..I4 mesh-only internal API; the `iam.delegation.events` SNS topic |
| **Does NOT own** | Tenant membership (Core / `iam-org-membership`), the synchronous user-removal gate (stays in Core), availability state itself (`iam-user-profile` — this service only sets/clears a pointer), Core's I-8 `active_delegations[]` embed (dropped entirely, ADR-0008 Option C) |
| **Synchronous dependencies (outbound)** | `iam-org-membership` — grant-time membership-existence checks; `iam-user-profile` — delegate OOO pre-flight (`GetAvailability`), availability-first pointer set (`SetAvailability`), dedicated pointer-clear (`ClearDelegatePointer`) |
| **Asynchronous dependency (inbound)** | `delegation-cascade-q` — `MembershipRevoked` / `TenantMembershipsPurged` (Core) and `UserUpdated{status:disabled}` (User Profile) |
| **Structural highlight** | This service is **both a producer and a consumer** of domain events — the one difference from most of its O&M-extraction siblings |

It is the fourth and last O&M extraction from `iam-org-membership` (ADR-0008, Option C).

## Why this service exists

Delegation used to live inside `iam-org-membership` as a side responsibility of tenant
membership. Pulling it out gives delegation its own tenant-scoped RLS boundary, its own event
topic (Workflow Service needs `DelegationStarted`/`DelegationEnded` to reroute and restore work —
Core's AsyncAPI dropped these entirely), and its own reconciler cadence (four independent CronJobs
instead of piggybacking on membership's). The trade-off: two composite foreign keys
(`delegator_membership_id`/`delegate_membership_id` → Core's `tenant_memberships`) became logical
references with no local FK, replaced by synchronous grant-time existence checks against
`iam-org-membership` (DLG-D3).

## API overview

**Public** (`/api/v1/delegations*`, gateway-fronted, `x-user-id`/`x-tenant-id`/`x-tenant-roles`
trusted from Envoy — never a parsed JWT, never derived from the body):

| ID | Method & path | Purpose |
|---|---|---|
| DLG-1 | `GET /api/v1/delegations` | List the caller's delegations (cached, `del:list:{tenant}:{delegator}`, 60s TTL) |
| DLG-2 | `POST /api/v1/delegations` | Create — requires `Idempotency-Key` header (400 if missing) |
| DLG-3 | `DELETE /api/v1/delegations/:id?record_version=N` | Cancel — `record_version` is a **query param** here |
| DLG-4 | `POST /api/v1/delegations/:id/extend` | Extend an open-ended delegation's review window |
| DLG-5 | `POST /api/v1/delegations/:id/reassign` | End the current delegation, create a new one to a different delegate |
| DLG-6 | `GET /api/v1/delegations/settings` | Get the tenant's policy defaults |
| DLG-7 | `PUT /api/v1/delegations/settings` | Set the tenant's policy defaults — `tenant_admin`/`tenant_owner` only |

**Internal** (`/internal/*`, mesh-only mTLS trust boundary — no RBAC, no JWT parsing, no
`ContextMiddleware`; reads bind `app.tenant_id` per call via `gucBoundReader`, `userID="iam-system"`):

| ID | Method & path | Purpose |
|---|---|---|
| DLG-I1 | `POST /internal/delegations/expire` | On-demand trigger for the expiry sweep (shares code with the `delegation-expiry` CronJob) |
| DLG-I2 | `POST /internal/delegations/review-sweep` | On-demand trigger for the review-warning sweep (shares code with `delegation-review`) |
| DLG-I3 | `GET /internal/delegations/dept-delegate` | Cross-tenant lookup: who is delegate for a given department scope |
| DLG-I4 | `GET /internal/users/:id/active-delegations` | Escape-hatch replacement for Core's removed `active_delegations[]` embed — unused today |

`GET /healthz` (liveness, `gincommon.HealthHandler()` verbatim) and `GET /readyz` (readiness — see
Docker § Health and readiness below) are unauthenticated.

DLG-3's `record_version` lives in the query string; DLG-4/5/7 all take it as a body field —
verify per-endpoint if you're writing a client, they genuinely differ.

### Error envelope

Flat shape, not the `code`/`message`/`details` hybrid some sibling services use:

```json
{ "error": "optimistic_lock_conflict", "status": 409, "trace_id": "...", "request_id": "..." }
```

`errorStatusByCode` (`internal/adapter/inbound/http/errors.go`) maps every `domain.Err*` sentinel
to its HTTP status per the LLD's error taxonomy. A code missing from that map falls back to 500 —
there shouldn't be one.

## Input validation

Validated in `DelegationService.Create`/`Extend` (`internal/core/service/delegation_service.go`),
not at the HTTP layer:

| Field / rule | Constraint | Error code | Status |
|---|---|---|---|
| `delegate_id` | Must differ from the delegator | `self_delegation` | 422 |
| `scope` | One of `all` / `department` / `tender` | `invalid_delegation_scope` | 400 |
| `scope_id` | Required when `scope != all`; must be omitted when `scope == all` | `scope_id_required` / `invalid_scope_id` | 422 |
| `reason` | ≤ 500 runes | `reason_too_long` | 422 |
| `starts_at` | Not more than 5s in the past (clock-skew tolerance); not more than 365 days in the future | `delegation_start_in_past` / `delegation_start_too_far_future` | 422 |
| `ends_at` | Must be after `starts_at` when set | `delegation_window_inverted` | 422 |
| `ends_at - starts_at` | ≤ tenant's `max_duration_days` (default 90) | `delegation_window_too_long` | 422 |
| `delegator_id` / `delegate_id` | Both must resolve to an active membership (`iam-org-membership`, concurrent checks) | `invalid_delegate` | 422 |
| Delegate availability | Delegate must not be OOO at `starts_at` — checked by DEL via `GET /api/v1/users/:id/availability` before calling UP (GAP-DEL-2) | `delegate_unavailable` | 422 |
| `extend_days` (DLG-4) | 1–180 | `extend_days_out_of_range` | 422 |
| `Idempotency-Key` header (DLG-2 only) | Required, enforced by middleware before the handler runs | `validation_error` | 400 |
| `Idempotency-Key` (DLG-2 only) | Must not already be claimed by another in-flight create for the same key | `idempotency_key_in_flight` | 409 |
| `record_version` | Must match the current row (optimistic lock) | `optimistic_lock_conflict` | 409 |
| `max_duration_days` / `review_window_days` (DLG-7) | 1–180 | `invalid_delegation_max_duration_days` / `invalid_delegation_review_window_days` | 400 |

Dependency-unavailable paths fail closed with a 503, not a masked 500:
`org_membership_unavailable` / `user_profile_unavailable`.

## Architecture

Clean Architecture — dependencies point inward; outer layers never import inner layers, enforced
in CI by `go-arch-lint` (`.go-arch-lint.yml`).

```
internal/
├── core/
│   ├── domain/    # Delegation, enums, DomainEvent + event-type constants, domain.Err* sentinels
│   ├── port/      # repository/client/cache/tx-runner/event-publisher interfaces
│   └── service/   # delegation_service.go (DLG-1..5) · settings_service.go (DLG-6/7) · cascade_service.go
└── adapter/
    ├── inbound/
    │   ├── http/      # router, handlers, middleware, error mapping
    │   └── consumer/  # delegation-cascade-q SQS consumer
    └── outbound/
        ├── postgres/    # TxRunner, repositories, migrations, sysPool config
        ├── userprofile/ # DEL-6 HTTP client: OOO pre-flight (GetAvailability, GAP-DEL-2) + availability-pointer set (SetAvailability) + pointer-clear (ClearDelegatePointer)
        ├── orgmembership/ # DLG-D3 HTTP client — grant-time membership-existence checks against Core's I-15
        ├── eventbus/    # enqueue-time validation, SNS-publish-time Glue codec
        ├── valkey/      # cache + idempotency store
        └── metrics/     # iam_delegation_* Prometheus instruments
```

| Rule | Enforced by |
|---|---|
| `internal/core/*` imports no adapter, Gin, pgx, or AWS SDK | `go-arch-lint` |
| `core/port` depends only on `core/domain`; `core/service` depends on `domain`+`port`+`requestctx` | `go-arch-lint` |
| Inbound adapters depend on `service`+`port`+`domain` — never an outbound adapter directly | `go-arch-lint` |
| Outbound adapters depend on `port`+`domain`+`eventschema` — never `service`, never inbound | `go-arch-lint` |
| No session-scoped `SET app.tenant_id` — only `SET LOCAL` via `pgcommon.GUCSetFromContext` | `.github/scripts/check-forbidden-set-guc.sh` |

### Storage and messaging

| Concern | Technology |
|---|---|
| Database | PostgreSQL, RLS `FORCE`d on `delegations`/`delegation_tenant_settings`; `processed_events` is RLS-exempt |
| Cache / idempotency | Valkey, `del:` keyspace |
| Outbound events | Transactional outbox (`platform-events`) → AWS SNS, optional Glue Schema Registry wire-format at publish time |
| Inbound events | AWS SQS (`delegation-cascade-q`), at-least-once, deduped via `processed_events` |

### Shared library dependencies

```go
require (
    github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events     v1.4.0
    github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon  v1.3.0
    github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon   v1.3.0
)
```

- `platform-gincommon` — HTTP middleware, structured logging (Zap), OTel OTLP tracing, Prometheus registerer.
- `platform-pgcommon` — PostgreSQL pool (`pgx/v5`), RLS GUC injection, migrations.
- `platform-events` — transactional outbox, SNS publisher, SQS consumer.

## Integrating with other services

### 1. Calling the public API

```go
req, _ := http.NewRequest(http.MethodPost, "https://iam.internal/api/v1/delegations", body)
req.Header.Set("X-Tenant-Id", tenantID)
req.Header.Set("X-User-Id", userID)
req.Header.Set("X-Tenant-Roles", "tenant_admin")
req.Header.Set("Idempotency-Key", uuid.NewString()) // required on create only
resp, err := httpClient.Do(req)
```

### 2. Create a delegation (DLG-2)

```bash
curl -X POST https://iam.internal/api/v1/delegations \
  -H "X-Tenant-Id: $TENANT_ID" -H "X-User-Id: $USER_ID" \
  -H "Idempotency-Key: $(uuidgen)" -H "Content-Type: application/json" \
  -d '{
    "delegate_id": "8f14e45f-ceea-467e-add9-a3c00e8fc2a1",
    "scope": "department",
    "scope_id": "0b4e2f10-...",
    "reason": "Annual leave",
    "starts_at": "2026-09-10T00:00:00Z",
    "ends_at": "2026-09-20T00:00:00Z"
  }'
```

Creating with a `starts_at` in the future inserts the row as `scheduled`, not `active` — no
User Profile call, no `DelegationStarted` event, yet. The `delegation-activation` CronJob flips it
to `active` (calling User Profile and emitting `DelegationStarted`) once `starts_at` is reached
(DLG-D25). Omitting `ends_at` creates an open-ended delegation, tracked instead by
`review_due_at` (DLG-4/DLG-I2's entire reason for existing).

### 3. Extend / reassign / settings

```bash
# DLG-4 — extend_days and record_version are both body fields here
curl -X POST https://iam.internal/api/v1/delegations/$ID/extend \
  -H "X-Tenant-Id: $TENANT_ID" -H "X-User-Id: $USER_ID" -H "Content-Type: application/json" \
  -d '{"extend_days": 14, "record_version": 3}'

# DLG-5 — ends the current delegation, creates a new one to a different delegate
curl -X POST https://iam.internal/api/v1/delegations/$ID/reassign \
  -H "X-Tenant-Id: $TENANT_ID" -H "X-User-Id: $USER_ID" -H "Content-Type: application/json" \
  -d '{"delegate_id": "...", "record_version": 3}'

# DLG-7 — tenant_admin/tenant_owner only
curl -X PUT https://iam.internal/api/v1/delegations/settings \
  -H "X-Tenant-Id: $TENANT_ID" -H "X-User-Id: $USER_ID" -H "X-Tenant-Roles: tenant_admin" \
  -H "Content-Type: application/json" \
  -d '{"max_duration_days": 60, "review_window_days": 45}'
```

### 4. Subscribing to `iam.delegation.events`

One dedicated SNS topic, four event types, no per-consumer routing publisher:

| Event | Emitted when | `ended_reason` values (DelegationEnded only) |
|---|---|---|
| `DelegationStarted` | DLG-2 create (immediate), create-leg of DLG-5, `delegation-activation` flip | — |
| `DelegationEnded` | DLG-3 cancel, `ends_at` expiry, review auto-end, end-leg of DLG-5, membership-revoked cascade, delegate-disabled cascade | `expired`, `cancelled`, `delegate_removed`, `review_expired`, `delegate_disabled` |
| `DelegationReviewRequested` | Daily review sweep, once per calendar day for each of the 3 days before `review_due_at` | — (`days_remaining ∈ {3,2,1}`) |
| `DelegationEscalationRequested` | Immediately after `DelegationEnded`, same transaction, only when `ended_reason=delegate_disabled` | — |

Every envelope carries `specversion: "1"`; cron-origin events stamp `actor: SystemActorID` and
`user_agent: "iam-delegation/<job>-cron"`. The delegator-side end of a membership-revoked cascade
is intentionally silent — no event — only the delegate-side row emits `DelegationEnded`; this
silence rule does **not** apply to the delegate-disabled cascade, where every ended row is
delegate-side by construction.

Wire format: AWS Glue Schema Registry (`iam-delegation-events`), 18-byte header
(`0x03` version, `0x00` no compression, 16-byte schema-version UUID). The local dev stack
uses Floci, which includes Glue Schema Registry in its free tier — `GLUE_REGISTRY_NAME` is
set by default so `GlueCodec` runs locally with the real wire format (no `NoopCodec` fallback).

### 5. What this service consumes — `delegation-cascade-q`

One SQS queue, two upstream SNS topics, three event types:

| Source topic | Event | Dispatched to |
|---|---|---|
| `iam.membership.events` (Core) | `MembershipRevoked` | `CascadeService.EndForUser` — ends every delegation where the user is delegator or delegate |
| `iam.membership.events` (Core) | `TenantMembershipsPurged` | `CascadeService.ScrubTenant` — soft-deletes the tenant's rows |
| `iam.user.events` (User Profile) | `UserUpdated` (only when `status=="disabled"`; other deliveries are acked without dispatch) | `CascadeService.EndForDisabledDelegate` — ends every active/scheduled delegation where the disabled user is the delegate, `deleted_at` deliberately left unset |

At-least-once delivery, deduped via `processed_events (event_id, consumer)` — three distinct
consumer buckets (`cascade`, `offboarding`, `delegate_disable`). DLQ:
`delegation-cascade-q-dlq`, `maxReceiveCount=5`.

**Subscription requirements:** Both SNS subscriptions onto `delegation-cascade-q` must have
`RawMessageDelivery=true` — `platform-events` unmarshals `msg.Body` directly as a CloudEvents
envelope; an SNS wrapper envelope causes permanent silent message loss (DLG-D41, corrected in
`deploy/messaging/sns_subscriptions.tf.example`). The `iam.membership.events` subscription is
provisioned by `iam-org-membership`'s `scripts/init-floci.sh` (Gap OM-2); the `iam.user.events`
subscription is provisioned by this service's own `scripts/init-floci.sh`.

**Additionally**, when this service emits `DelegationEnded`, iam-user-profile's `DelegationEventConsumer` (`delegation-events-user-profile-q`) receives it and clears the stale `delegate_id` pointer as a self-healing fallback — regardless of whether the synchronous `ClearDelegatePointer` call above succeeded. This closes Gap 7 Option C.

### 6. Error handling

Every 4xx/5xx response uses the flat envelope above. Treat `409 optimistic_lock_conflict` as
"re-fetch and retry with the new `record_version`" — never blind-retry with the stale value.
Treat `409 idempotency_key_in_flight` (DLG-2 only) as "another request with this exact
`Idempotency-Key` is still being processed" — retry with backoff using the *same* key, not a new
one; retrying with a new key would create a second delegation.
`503` responses (`org_membership_unavailable`, `user_profile_unavailable`, `db_unavailable`) are
safe to retry with backoff.

### 7. Background jobs

Four CronJobs, all sharing the reconciler binary (`--job=<name>`):

| Job | Schedule | Purpose |
|---|---|---|
| `delegation-activation` | `*/5 * * * *` | Flip `scheduled` → `active` once `starts_at` is reached (DLG-D25) |
| `delegation-expiry` | `*/5 * * * *` | End delegations past `ends_at` (self-retrying, availability-first) |
| `delegation-review` | `0 * * * *` | 3-day daily-cascade review warning, then auto-end on the review deadline |
| `delegation-cleanup` | `0 4 1 * *` | Hard-purge soft-deleted rows and prune `processed_events` |

One additional **metric-exporter goroutine** runs inside `cmd/server` (not a CronJob):
`runActiveGaugeExporter` refreshes `iam_delegation_active_gauge{tenant}` every 5 minutes from the BYPASSRLS `sysPool` — was wired but permanently empty until DLG-D31 populated it.

## Local development

### Prerequisites

- Go 1.26.6 (pinned exactly — matches `go.mod`'s `go 1.26.6`)
- Docker (Postgres, Valkey, Floci — `make docker-up`)
- `GOPRIVATE=github.com/BCBP-SOLUTIONS-FZC-LLC/*` (`GONOSUMDB` too) and an SSH key registered with the BCBP org

### Setup

```bash
git clone git@github.com:BCBP-SOLUTIONS-FZC-LLC/iam-delegation.git
cd iam-delegation
make setup          # copies .env.example -> .env, installs git pre-commit hook
make docker-up      # Postgres (5537) + Valkey (6383) + Floci (4570) + Floci UI (4501)
make run            # go run cmd/server, sourcing .env
```

Or run the whole stack, including the app itself, in containers:

```bash
make compose-up     # self-migrates at startup, no separate migrate step
make compose-down   # stop and remove, including volumes
```

### Common commands

| Command | Description |
|---|---|
| `make setup` | Copy `.env.example` → `.env`, install git pre-commit hook |
| `make tidy` / `make fmt` / `make fmt-check` / `make vet` | Go basics; `fmt-check` mirrors CI, does not modify files |
| `make lint` | `golangci-lint` via `go tool` |
| `make arch-lint` | Architecture dependency-direction check (`go-arch-lint`) |
| `make mod-verify` | `go mod verify` |
| `make vuln-check` | `govulncheck ./cmd/... ./internal/...` |
| `make test` | Unit + integration + RLS in parallel (Docker required) |
| `make test-ci` | Same, with `-race` + merged `coverage.out` (used in CI) |
| `make test-unit` | Unit tests only, no Docker |
| `make test-integration` | Integration tests (Postgres/Valkey/SQS via testcontainers) |
| `make test-rls` | RLS tests (Postgres via testcontainers) |
| `make test-e2e` | End-to-end tests |
| `make race` | All suites with `-race`, no coverage merge |
| `make run` | Run the server locally (sources `.env`) |
| `make run-reconciler` | Run a reconciler job; pass `JOB=delegation-activation\|delegation-expiry\|delegation-review\|delegation-cleanup` |
| `make build` | Compile both binaries to `bin/` |
| `make cover` / `make cover-func` | Coverage HTML report / per-function summary |
| `make swag` / `make swag-check` | Regenerate / verify freshness of `docs/swagger/` |
| `make schema-validate` | AsyncAPI + event schema governance, 8 passes (no AWS needed) |
| `make docker-up` / `make docker-down` | Start/stop Postgres + Valkey + Floci + Floci UI |
| `make compose-up` / `make compose-down` | Start/stop the full stack including this service |
| `make clean` | Remove `bin/` artefacts and coverage files |

### Running a single test

```bash
go test ./internal/core/service/... -run TestDelegationService_Create -v
go test -tags=rls ./internal/adapter/outbound/postgres/... -run TestRLS -v
```

Every test in this repo is colocated white-box (`*_test.go` next to the source it covers) — there
is no separate `test/` tree, and `go test ./...` alone runs the complete suite.

### Running a reconciler job locally

```bash
JOB=delegation-activation make run-reconciler
```

---

## Testing domain events locally

Every mutating write publishes a domain event through a **transactional outbox → SNS → SQS** pipeline.

### How the pipeline works

```
HTTP write / reconciler job
    │
    ▼
service layer  ──(same tx)──▶  outbox_events (Postgres)
                                      │
                               outbox runner (OUTBOX_POLL_INTERVAL, 500 ms)
                                      │
                                      ▼
                          SNS: iam.delegation.events   (Floci)
                                      │
                    SNS fan-out to downstream SQS queues (local dev only)
```

An outbox insert is atomic with the business write — a `2xx` response guarantees an `outbox_events` row exists.

### Step 1 — Start infrastructure

```bash
make docker-up
```

`scripts/init-floci.sh` runs automatically inside the `floci` container and provisions:

- The `iam-delegation-events` Glue registry with all four event schemas — Floci includes Glue Schema Registry in its free tier, so `GLUE_REGISTRY_NAME` is set in `docker-compose.yml` by default and the real `GlueCodec` wire format runs locally (no `NoopCodec` fallback, no Pro token).
- The outbound `iam-delegation-events` SNS topic.
- The inbound `delegation-cascade-q` (+ DLQ), subscribed to `iam-user-events` for `UserUpdated`.
- Four downstream fan-out queues (+ DLQs) for Workflow / Notification / Audit / User Profile, each with the SNS filter policy those services actually apply.

### Step 2 — Verify SNS/SQS/Glue exist

```bash
docker compose exec floci aws --region ap-south-1 sns list-topics
docker compose exec floci aws --region ap-south-1 sqs list-queues
docker compose exec floci aws --region ap-south-1 glue list-schemas \
  --registry-id RegistryName=iam-delegation-events
```

### Step 2b — Verify event delivery in the browser (Floci UI)

`make docker-up` also starts **Floci UI** at **http://localhost:4501** — a faster way to confirm an event landed on the right queue than shelling into the CLI each time:

1. Open **http://localhost:4501** → sidebar → **Integration → SQS**. You'll see all 10 queues `init-floci.sh` provisioned (`delegation-cascade-q`, `delegation-workflow-q`, `delegation-notification-q`, ...) each with a **Messages** column.
2. Trigger an event — exercise a real endpoint (`make run` + a DLG-2 create call) or publish one directly:
   ```bash
   docker compose exec floci aws --region ap-south-1 sns publish \
     --topic-arn arn:aws:sns:ap-south-1:000000000000:iam-delegation-events \
     --message '{"id":"demo-1","type":"DelegationStarted","tenant_id":"t1"}' \
     --message-attributes 'EventType={DataType=String,StringValue=DelegationStarted}'
   ```
3. Refresh the SQS list. The **Messages** count should go to `1` on exactly the queues whose filter policy matches that `EventType` — for `DelegationStarted` that is `delegation-workflow-q`, `delegation-notification-q`, and `delegation-audit-q` (catch-all), but **not** `delegation-events-user-profile-q` (which only receives `DelegationEnded`). A wrong or missing count means the filter policy or the event's `EventType` attribute is wrong.

Two things the UI does **not** do yet:
- **Read a message's payload** — the UI shows queue metadata only, no message browser. Use the CLI (Step 2c).
- **Browse SNS topics/subscriptions** — no SNS adapter in the UI yet (Step 2d).

### Step 2c — Retrieve the event body (CLI)

```bash
# Peek without consuming — message becomes visible again after VisibilityTimeout (30 s)
docker compose exec floci aws --region ap-south-1 sqs receive-message \
  --queue-url http://floci:4566/000000000000/delegation-notification-q \
  --max-number-of-messages 10 --message-attribute-names All
```

Every subscription sets `RawMessageDelivery=true`, so `Body` is already the plain event JSON — no SNS envelope to unwrap. Pretty-print with `jq`:

```bash
docker compose exec floci aws --region ap-south-1 sqs receive-message \
  --queue-url http://floci:4566/000000000000/delegation-notification-q \
  --max-number-of-messages 10 --message-attribute-names All \
  | jq -r '.Messages[] | .Body | fromjson'
```

Or skip SQS and read the outbox directly — shows the plain-JSON payload before Glue wire-format encoding:

```bash
docker compose exec postgres psql -U delegation -d delegation -c \
  "SELECT event_type, jsonb_pretty(payload::jsonb) FROM outbox_events ORDER BY created_at DESC LIMIT 5;"
```

### Step 2d — Inspect SNS topics and subscriptions (CLI)

```bash
# All subscriptions on iam-delegation-events
docker compose exec floci aws --region ap-south-1 sns list-subscriptions-by-topic \
  --topic-arn arn:aws:sns:ap-south-1:000000000000:iam-delegation-events \
  --query 'Subscriptions[].{Queue:Endpoint,Arn:SubscriptionArn}' --output table

# Filter policy on a specific subscription (grab SubscriptionArn from above)
docker compose exec floci aws --region ap-south-1 sns get-subscription-attributes \
  --subscription-arn <SubscriptionArn> \
  --query 'Attributes.{FilterPolicy:FilterPolicy,RawMessageDelivery:RawMessageDelivery}'
```

A missing `FilterPolicy` means the subscription is a catch-all (`delegation-audit-q` — receives all four event types). A queue you expected an event on but that showed `Messages: 0` in Floci UI almost always means the event's `EventType` message attribute isn't in that queue's filter list.

### Step 3 — Send a cascade message and inspect the outbox

Send a test `MembershipRevoked` directly to the inbound queue (no SNS involved):

```bash
docker compose exec floci aws --region ap-south-1 sqs send-message \
  --queue-url http://floci:4566/000000000000/delegation-cascade-q \
  --message-body '{
    "id": "b6a1e45f-0000-0000-0000-000000000001",
    "type": "MembershipRevoked", "source": "iam-org-membership",
    "specversion": "1", "tenant_id": "...", "time": "2026-09-19T00:00:00Z",
    "data": {"user_id": "..."}
  }'
```

Inspect the outbox after triggering a normal write (`make run` + a DLG-2 create):

```bash
docker compose exec postgres psql -U delegation -d delegation -c \
  "SELECT id, event_type, published_at IS NOT NULL AS published, attempts
   FROM outbox_events ORDER BY created_at DESC LIMIT 10;"
```

### Troubleshooting events

| Symptom | Likely cause | Fix |
|---|---|---|
| `outbox_events` row never gets `published_at` set | Topic ARN mismatch, or outbox runner not started | Re-check `.env`'s `SNS_TOPIC_ARN` against `docker compose exec floci aws --region ap-south-1 sns list-topics` (or Floci UI at http://localhost:4501) |
| `outbox_events` empty after a write | Row was published and pruned, or write never committed | Re-check the HTTP response code — a `2xx` guarantees the row was committed |
| Messages keep reappearing after `receive-message` | Normal — SQS visibility timeout, not deletion | Use `delete-message` to consume permanently |
| Glue schema not found at startup | Init script hasn't finished yet | Wait for the `floci` container healthcheck (checks `DelegationEscalationRequested` schema) to pass before starting the app |
| `delegation-cascade-q` receives no `MembershipRevoked` | iam-org-membership's subscription not provisioned locally | Send directly via `sqs send-message` (Step 3 above); the SNS→SQS subscription is owned by org-membership's `init-floci.sh` (Gap OM-2) |

---

## Testing

CI enforces a single global statement-coverage gate of **95%** on the merged `coverage.out`
(`.github/scripts/coverage-gate.sh`, `COVERAGE_THRESHOLD` in `validate-test.yml`), measured with
`-coverpkg=$(go list ./internal/... ./pkg/...)` across the unit + integration + RLS suites
together. `-race` is a hard gate on every suite, not opt-in.

## Environment variables

See `.env.example` for the full, current, authoritative list with inline explanations — this
table covers only the ones most likely to trip someone up.

| Variable | Required | Default | Notes |
|---|---|---|---|
| `DATABASE_URL` | Yes (or `PG_HOST`+`PG_USER`+`PG_PASSWORD`) | — | App role (`delegation_app`), `NOBYPASSRLS` — RLS is enforced even locally. `loadConfig` fails fast at startup if neither form is set |
| `MIGRATION_DATABASE_URL` | Required when `PG_BOUNCER_MODE=true` | falls back to `DATABASE_URL`'s DSN shape | BYPASSRLS role; the server self-migrates at startup, no separate migrate step. Migrations take a session-scoped `pg_advisory_lock`, so they must bypass PgBouncer's transaction pooling |
| `SYSTEM_DATABASE_URL` | Required outside `development`/`dev`/`local`/`test` (resolved via `APP_ENV`, then `ENVIRONMENT`; both binaries fail fast otherwise); required in Helm always | falls back with a startup warning in local/dev | BYPASSRLS pool for cross-tenant reconciler sweeps, cascade `processed_events`, and the active-gauge exporter — unset outside local/dev means cross-tenant queries silently return zero rows |
| `USER_PROFILE_BASE_URL` / `ORG_MEMBERSHIP_BASE_URL` | Yes | — | Client constructors fail fast at startup if empty; neither service is part of this repo's compose stack |
| `SNS_TOPIC_ARN` / `CASCADE_QUEUE_URL` | **Yes** | — | `loadConfig` returns an error and the process never starts if either is empty |
| `AWS_REGION` | No | `ap-south-1` | |
| `GLUE_REGISTRY_NAME` | No | `""` → `NoopCodec` | Set to `iam-delegation-events` to activate the real Glue codec (set automatically in the local dev compose stack via Floci) |
| `GLUE_REGISTRY_ARN` | No | `""` | Full ARN of the Glue registry; set alongside `GLUE_REGISTRY_NAME` in non-local environments |
| `IDEMPOTENCY_TTL_SECONDS` / `LIST_CACHE_TTL_SECONDS` | No | `86400` / `60` | |
| `POLICY_DEFAULT_MAX_DURATION_DAYS` / `POLICY_DEFAULT_REVIEW_WINDOW_DAYS` | No | `90` / `90` | Fallback tenant policy when no `delegation_tenant_settings` row exists |
| `DOCS_ENABLED` / `DOCS_AUTH_TOKEN` | No | `true` outside production | Gates `/swagger`, `/asyncapi` |

## Security

| Layer | Mechanism |
|---|---|
| Tenant isolation | Postgres RLS (`FORCE`, fail-closed), `SET LOCAL app.tenant_id` per transaction — never session-scoped |
| Identity | Gateway-injected `x-user-id`/`x-tenant-id`/`x-tenant-roles` headers — never a parsed JWT, never body-derived |
| Network | Public routes behind Envoy; `/internal/*` is mesh-only mTLS with no RBAC/JWT check at the application layer |
| Cross-service calls | Intra-mesh mTLS to `iam-org-membership` / `iam-user-profile`; response bodies capped at 1 MiB (`httpx.LimitBody`) against a misbehaving peer |
| Create idempotency | Atomic `SETNX` reservation (`IdempotencyStore.Reserve`/`Release`), not check-then-act — closes a duplicate-insert race on concurrent same-key creates |
| Docs auth | Swagger/AsyncAPI bearer-token check via `crypto/subtle.ConstantTimeCompare`, not `!=` |

Three distinct GUC-binding paths converge on the same RLS: the public-route middleware, the
mesh-only internal reads' per-call binder, and the reconciler jobs/cascade consumer's injected
binder function — see `docs/architecture/mermaid/rls-guc-flow.mmd`.

## CI

GitHub Actions (`.github/workflows/`):

1. **`validate-test.yml`** — unit + integration + RLS tests with `-race`, merged coverage → 95%
   gate → architecture lint → Swagger drift check → end-to-end tests.
2. **`validate-quality.yml`** — gofmt, `go mod tidy` drift, `go vet`, `golangci-lint`,
   `govulncheck`, `go mod verify`, Dockerfile base-image digest check, and two repo-specific
   greps: no HTML-escaped operators in workflow files, no session-scoped `SET app.tenant_id`.
3. **`ci.yml`** — Hadolint + Buildx build (`linux/amd64` only) → Trivy CVE scan (fails on
   CRITICAL/HIGH/UNKNOWN) → smoke tests against **both** the `server` and `reconciler`
   entrypoints from the same image → push to GHCR with provenance+SBOM → Cosign keyless sign.
4. **`changelog-check.yml`** — fails if `CHANGELOG.md` isn't updated alongside source changes.
5. **`release.yml`** (`v*` tags) — same build with SLSA provenance attestation and a GitHub
   Release carrying checksums/SBOM/provenance.
6. **Schema governance** (`schema-registry.yml`, `schema-prune.yml`, `schema-health-quarterly.yml`,
   `freeze-watchdog.yml`) — `platform-schemagov` CLI: validate/diff/register against the
   dedicated `iam-delegation-events` Glue registry. See `docs/runbook-schema-registry.md`.

## Docker

### What the bundled `docker-compose.yml` starts

| Container | Image | Host port(s) | Purpose |
|---|---|---|---|
| `iam-delegation` | built from local `Dockerfile` | `8080`, `9090` | This service itself, when run via `make compose-up` |
| `postgres` | `postgres:16-alpine` | `5537 → 5432` | Primary store |
| `valkey` | `valkey/valkey:8-alpine` | `6383 → 6379` | Advisory cache + idempotency store |
| `floci` | `floci/floci:2.1.0-compat` | `4570 → 4566` | SNS + SQS + Glue Schema Registry (all services free) |
| `floci-ui` | `floci/floci-ui:0.5.0` | `4501 → 4500` | Web console for `floci` — http://localhost:4501 |

`make docker-up` starts only `postgres`/`valkey`/`floci`/`floci-ui` — not `iam-delegation` — so local dev typically runs the service via `make run` for fast rebuilds. Host ports are deliberately offset from sibling services' stacks (iam-user-profile: 5433/6379/4566, iam-org-membership: 5533-5534/6380/4567) so all stacks can run side-by-side.

### Building the service image

One image, two binaries — `iam-delegation-server` and `iam-delegation-reconciler` — selected at
runtime by overriding the container's `command`. There is no separate migrate image or Job; the
server binary self-migrates at startup.

```bash
make docker-build    # build the image
make compose-up      # Postgres + Valkey + Floci + the app itself
```

### Health and readiness

- `GET /healthz` — liveness only, `gincommon.HealthHandler()` verbatim.
- `GET /readyz` — Postgres (app pool) and sys Postgres (BYPASSRLS pool) are both hard-blocking:
  either being unhealthy returns overall `503`. Valkey and the outbox runner are degraded-only —
  neither ever flips overall readiness.

### Minimum required env vars to start

`DATABASE_URL`, `USER_PROFILE_BASE_URL`, `ORG_MEMBERSHIP_BASE_URL`, `SNS_TOPIC_ARN`,
`CASCADE_QUEUE_URL` — the process fails fast at startup if any of these five is empty (the last
two unconditionally; the dependency base URLs via their client constructors).

## Cross-service dependencies

Reads (DLG-1, DLG-6, DLG-I3, DLG-I4) and read-only reconciler sweeps (`delegation-review`, `delegation-cleanup`) have **no** synchronous cross-service dependency — Postgres + Valkey only.

| Operation | Sync dependency | Posture | On failure |
|---|---|---|---|
| Create (DLG-2) — grant-time membership-existence check (delegator + delegate, concurrent) | `iam-org-membership` I-15 × 2 | fail-closed | `503 org_membership_unavailable`; no delegation written |
| Create (DLG-2) — delegate OOO pre-flight (GAP-DEL-2): `GetAvailability` | `iam-user-profile` `GET /api/v1/users/:id/availability` | fail-closed | `422 delegate_unavailable` if OOO window overlaps `starts_at`; `503 user_profile_unavailable` if the call fails |
| Create (DLG-2, immediate activation) — set availability pointer: `SetAvailability` | `iam-user-profile` `PUT /internal/users/:id/availability` | fail-closed | `503 user_profile_unavailable`; no delegation written |
| Create (DLG-2, future-dated `scheduled`) — membership + OOO pre-flight only; User Profile `SetAvailability` deferred to activation flip | `iam-org-membership` I-15 × 2 + `iam-user-profile` `GetAvailability` | fail-closed | `503`; no delegation written; `SetAvailability` not called until `delegation-activation` promotes the row |
| Cancel (DLG-3) — clear availability pointer: `ClearDelegatePointer` | `iam-user-profile` `DELETE /internal/users/:id/availability/delegate` (Gap 3) | fail-open | Cancel commits; pointer-clear omitted |
| Extend (DLG-4) — re-sync `ooo_until` after review deadline extends: `SetAvailability` | `iam-user-profile` `PUT /internal/users/:id/availability` | fail-open | Extend commits; UP timestamp goes stale until next extend or expiry |
| Reassign (DLG-5) — end-leg: clear old delegate's pointer: `ClearDelegatePointer` | `iam-user-profile` `DELETE /internal/users/:id/availability/delegate` (Gap 3) | fail-open | End-leg commits; pointer-clear omitted |
| Reassign (DLG-5) — create-leg: membership check + OOO pre-flight + set new delegate's pointer | `iam-org-membership` I-15 × 2 + `iam-user-profile` `GetAvailability` + `SetAvailability` | fail-closed | `503`; full reassign rolled back |
| `delegation-activation` CronJob (scheduled → active) — set availability pointer: `SetAvailability` | `iam-user-profile` `PUT /internal/users/:id/availability` | fail-closed | Row stays `scheduled`; retried at next `*/5` run |
| `delegation-expiry` CronJob — clear availability pointer on expiry: `ClearDelegatePointer` | `iam-user-profile` `DELETE /internal/users/:id/availability/delegate` (Gap 3) | self-retrying | Pointer-clear deferred; row remains `active`; retried at next `*/5` run |
| Cascade `MembershipRevoked` (inbound async, `delegation-cascade-q`) | — | at-least-once | Message requeued on processing failure; `delegation-cascade-q-dlq` after `maxReceiveCount=5` |
| Cascade `TenantMembershipsPurged` (inbound async, `delegation-cascade-q`) | — | at-least-once | Message requeued on processing failure; `delegation-cascade-q-dlq` after `maxReceiveCount=5` |
| Cascade `UserUpdated{status:disabled}` (inbound async, `delegation-cascade-q`) | — | at-least-once | Message requeued on processing failure; `delegation-cascade-q-dlq` after `maxReceiveCount=5` |

---

## Out of scope

| Concern | Owner |
|---|---|
| Tenant membership itself | Core / `iam-org-membership` |
| Synchronous user-removal gate | Core (DLG-D9) |
| Availability state (the actual OOO record) | `iam-user-profile` — this service only sets/clears a pointer |
| Core's `active_delegations[]` embed | Dropped entirely by ADR-0008 Option C; `DLG-I4` is the escape hatch, unused today |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup, extending the service, testing requirements, and the PR checklist.

| Document | Description |
|---|---|
| [`.claude/CLAUDE.md`](.claude/CLAUDE.md) | Top-level guidance for Claude Code working in this repo |
| [`.claude/database.md`](.claude/database.md) | Tables, RLS, pool config, migrations, triggers |
| [`.claude/api-and-events.md`](.claude/api-and-events.md) | Endpoints, cache keys, event types, Glue wire format |
| [`.claude/flows-and-concurrency.md`](.claude/flows-and-concurrency.md) | Create/cancel/cron/cascade flows, optimistic locking, shutdown ordering |
| [`.claude/development-guide.md`](.claude/development-guide.md) | Design decisions, extending, workflow, troubleshooting, error codes |
| [`.claude/operations.md`](.claude/operations.md) | Security, observability, configuration, CI/CD, schema governance |
| [`ARCHITECTURE.md`](ARCHITECTURE.md) | Detailed architecture narrative with Mermaid diagrams; the as-built decision register (DLG-D13+) |
| [`docs/lld/iam-lld-delegation-service.md`](docs/lld/iam-lld-delegation-service.md) | Full LLD v2.14 — the design document this service implements |
| [`docs/runbook-schema-registry.md`](docs/runbook-schema-registry.md) | Schema-governance operator runbook |

## License / ownership

BCBP Solutions FZC LLC — internal platform service. Not for external distribution.
