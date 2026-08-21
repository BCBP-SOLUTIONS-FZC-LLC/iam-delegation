# Architecture

## Mental model

This service is the system of record for out-of-office delegations: two tables
(`delegations`, `delegation_tenant_settings`), an eleven-route API (DLG-1..7 public,
DLG-I1..I4 mesh-internal), two synchronous outbound dependencies of its own
(`iam-org-membership` for grant-time membership checks, `iam-user-profile` for
availability-first pointer set/clear), one inbound async subscription
(`delegation-cascade-q`), and its own outbound event topic (`iam.delegation.events`) — it is
both a producer and a consumer, the one structural difference from most of its O&M
extraction siblings.

It is the fourth and last wave of ADR-0008's decomposition of `iam-org-membership`, and the
only wave that changes Core's own I-8 endpoint: under Option C, Core drops the `delegations`
table and its `active_delegations[]` embed entirely rather than keeping any FK/projection back
to this service (§6.1). Nothing here feeds I-8.

## Layer model — Clean Architecture / Ports-and-Adapters

Same split its siblings (`iam-org-membership`, `iam-catalog-admin`, `iam-group-mapping`,
`iam-tender-acl`) use: `internal/core/{domain,port,service}` +
`internal/adapter/{inbound,outbound}`. Lifted near-verbatim from
`iam-org-membership`'s `internal/core/{domain,port,service}/delegation*.go`,
`delegation_handler.go`, `delegation_repository.go`, and
`cmd/reconciler/jobs/delegation_{expiry,review}.go` (LLD §6) — domain/port/service moved with
almost no change; adapters were rehosted; the tenant-policy read became a local table
(`delegation_tenant_settings`) instead of two `tenants` columns Core owned.

> Source: [`docs/architecture/mermaid/layer-model.mmd`](docs/architecture/mermaid/layer-model.mmd)

```mermaid
graph TD
    subgraph cmd["Composition Roots — cmd/"]
        main["server/main.go\nHTTP composition root (DLG-1..7, DLG-I1..I4)\n+ delegation-cascade-q SQS consumer, one process\nwires TxRunner/validator/GlueCodec/OutboxRunner\ntenantGUCMiddleware on the public route group"]
        reconciler["reconciler/main.go\n--job=<name> dispatch (this chart's own convention)\nsysPool (BYPASSRLS from SYSTEM_DATABASE_URL)"]
        jobs["reconciler/jobs/\ndelegation_expiry.go    -- DLG-I1 (*/5 * * * *)\ndelegation_review.go    -- DLG-I2 (0 * * * *, dual 7d/3d warn)\ndelegation_cleanup.go   -- soft-delete purge (0 4 1 * *)\nshares implementation with cmd/server's HTTP handlers\nvia cmd/server/adapters.go's reconcilerRunner (DLG-D17)"]
    end

    subgraph adapters_in["Inbound Adapters — internal/adapter/inbound/"]
        http_h["http/\nrouter.go — DLG-1..7, DLG-I1..I4, health, docs\ntenantGUCMiddleware · gucBoundReader (DLG-D18)\nAsyncAPI/Swagger doc handlers"]
        consumer["consumer/\nCascadeConsumer\n(MembershipRevoked -> CascadeService.EndForUser,\nTenantOffboarded -> CascadeService.ScrubTenant)\nprocessed_events idempotency dedup"]
    end

    subgraph adapters_out["Outbound Adapters — internal/adapter/outbound/"]
        postgres["postgres/\nDelegationRepository · SettingsRepository\nTxRunner (PayloadValidator hook) · migrations/\nWithTenantGUC · SystemDSNFromEnv (LLD §4.4-equivalent)"]
        userprofile["userprofile/\nHTTPClient — UserProfileClient impl\n(DEL-6 availability-first pointer set/clear)"]
        orgmembership["orgmembership/\nHTTPChecker — MembershipCheckClient impl\n(DLG-D3 grant-time membership-existence checks)"]
        eventbus["eventbus/\nSchemaValidator (jsonschema/v6, enqueue-time)\nGlueCodec · NoopCodec (events.Codec, wired via\nevents.WithCodec at SNS-publish time — iam-delegation-events)"]
        valkey["valkey/\ndel: cache — del:list: (60s) · del:idem: (24h)"]
        metrics["metrics/\niam_delegation_* Prometheus instruments\n(RecordCreated/Ended/ReviewWarned/CascadeProcessed/...)"]
    end

    subgraph eventschema_grp["Generated Schemas — internal/eventschema/"]
        eventschema["eventschema/\nEmbedded JSON Schemas for the three published events\nDelegationStarted · DelegationEnded · DelegationReviewRequested"]
    end

    subgraph core["Core — internal/core/"]
        service["service/\nDelegationService  -- DLG-1..5 orchestration\nSettingsService    -- DLG-6/7\nCascadeService     -- delegation-cascade-q business logic"]
        port["port/\nDelegationRepository · SettingsRepository\nUserProfileClient · MembershipCheckClient\nIdempotencyStore · EventPublisher · Cache · TxRunner"]
        domain["domain/\nDelegation · DelegationTenantSettings\nEventDelegation{Started,Ended,ReviewRequested}\ndelegation_* sentinel error taxonomy (LLD §20)"]
    end

    subgraph pkg["Shared Packages — pkg/"]
        requestctx["requestctx/\ngateway-identity / tenant-actor extraction helpers"]
    end

    subgraph apidir["Design-time Contract — api/"]
        api["asyncapi.yaml (hand-maintained) + embed.go\nvalidated by platform-schemagov against a live\nGlue registry (iam-delegation-events, DLG-D20)"]
    end

    main        --> http_h
    main        --> consumer
    main        --> postgres
    main        --> userprofile
    main        --> orgmembership
    main        --> eventbus
    main        --> valkey
    main        --> metrics
    reconciler  --> jobs
    jobs        --> postgres
    jobs        --> userprofile
    jobs        --> eventbus
    jobs        --> metrics
    http_h      --> service
    http_h      --> port
    http_h      --> requestctx
    http_h      --> metrics
    consumer    --> service
    service     --> port
    service     --> domain
    service     --> requestctx
    postgres    --> port
    postgres    --> domain
    userprofile --> port
    orgmembership --> port
    eventbus    --> port
    eventbus    --> eventschema
    valkey      --> port
    metrics     --> domain
    port        --> domain
    api         -.->|"make extract-schemas (n/a — hand-maintained\nboth sides here, DLG-D20)"| eventschema
```

Directory layout, for reference:

```
iam-delegation/
├── cmd/
│   ├── server/                              -- HTTP composition root (DLG-1..7, DLG-I1..I4)
│   │                                            + delegation-cascade-q SQS consumer, one process
│   │   └── main.go
│   └── reconciler/
│       ├── main.go                          -- --job=<name> dispatch (this chart's own convention)
│       └── jobs/
│           ├── delegation_expiry.go         -- DLG-I1 (*/5 * * * *, LLD §11.3)
│           ├── delegation_review.go         -- DLG-I2 (0 * * * *, dual 7d/3d warn, LLD §11.4)
│           └── delegation_cleanup.go        -- soft-delete purge (0 4 1 * *, LLD §11.6/§18.4)
├── internal/
│   ├── core/
│   │   ├── domain/
│   │   │   ├── delegation.go                -- Delegation, enums, EndReason (incl. review_expired)
│   │   │   ├── tenant_settings.go           -- DelegationTenantSettings (policy)
│   │   │   ├── event.go / event_payloads.go -- outbound event envelope + payload shapes
│   │   │   └── errors.go                    -- delegation_* sentinel error taxonomy (LLD §20)
│   │   ├── port/
│   │   │   ├── delegation_repository.go
│   │   │   ├── settings_repository.go       -- delegation_tenant_settings
│   │   │   ├── user_profile_client.go       -- UserProfileClient (DEL-6)
│   │   │   ├── membership_check_client.go   -- MembershipCheckClient (§7.6.2, swappable)
│   │   │   ├── idempotency_store.go         -- create-dedup (DLG-Q3)
│   │   │   ├── event_publisher.go
│   │   │   ├── cache.go
│   │   │   └── tx_runner.go
│   │   └── service/
│   │       ├── delegation_service.go        -- DLG-1..5 orchestration
│   │       ├── settings_service.go          -- DLG-6/7
│   │       └── cascade_service.go           -- delegation-cascade-q business logic
│   └── adapter/
│       ├── inbound/
│       │   ├── http/                        -- DLG-1..7, DLG-I1..I4, health, docs, router
│       │   └── consumer/                    -- delegation-cascade-q (§11.5/§11.6)
│       └── outbound/
│           ├── postgres/                    -- repositories, migrations, processed_events
│           ├── userprofile/                 -- UserProfileClient HTTP impl
│           ├── orgmembership/               -- MembershipCheckClient HTTP impl
│           ├── eventbus/                    -- SNS publisher (iam-delegation-events) + Glue codec
│           ├── valkey/                      -- del: cache + idempotency store
│           └── metrics/                     -- iam_delegation_* Prometheus instruments
├── internal/eventschema/                    -- embedded JSON Schemas for the three published events
├── pkg/requestctx/                          -- gateway-identity / tenant-actor extraction helpers
├── api/                                     -- asyncapi.yaml (hand-maintained) + embed.go
└── deploy/helm/iam-delegation/              -- this service's independent Helm chart
```

**Dependency rule** (CI-enforced — `.go-arch-lint.yml`, `make arch-lint`, LLD §6.2):
`internal/core/*` imports no adapter, Gin, pgx, or AWS SDK; `port` depends only on `domain`;
`service` depends on `domain`+`port`+`requestctx`; inbound adapters depend on
`service`+`port`+`domain` (never an outbound adapter directly — that's wired only from `cmd`);
outbound adapters depend on `port`+`domain`+`eventschema` (never `service`, never inbound);
`cmd/*` is the only place concretes get wired together. No session-scoped
`SET app.tenant_id` — only `SET LOCAL` via `pgcommon.GUCSetFromContext` (CI greps the forbidden
form — `.github/scripts/check-forbidden-set-guc.sh`, RLS-6). Every outbound client sits behind a
port interface; every event publish goes through the transactional outbox.

## Data model

Two tenant-scoped tables in the `delegation` logical database, RLS via the `app.tenant_id` GUC, no cross-database foreign keys — the three FKs the O&M split loses are replaced with logical references plus the synchronous/async patterns below (LLD §7.6).

> Source: [`docs/architecture/mermaid/data-model.mmd`](docs/architecture/mermaid/data-model.mmd)

```mermaid
erDiagram
    delegations {
        uuid id PK
        uuid tenant_id "NOT NULL, logical ref to Core tenants(id), no local FK (§7.6.6)"
        uuid delegator_id "user delegating"
        uuid delegate_id "user receiving"
        uuid delegator_membership_id "NOT NULL, logical ref to Core tenant_memberships(id), no local FK (§7.6.1)"
        uuid delegate_membership_id "NOT NULL, logical ref to Core tenant_memberships(id), no local FK (§7.6.1)"
        delegation_scope scope "ENUM all-department-tender (DEL-2)"
        uuid scope_id "dept or tender UUID, NULL when scope=all"
        text reason "optional, max 500 chars, audit-only (DEL-10)"
        timestamptz starts_at "NOT NULL DEFAULT now()"
        timestamptz ends_at "NULL = open-ended (DEL-8)"
        timestamptz review_due_at "open-ended only, starts_at + review window (DEL-13)"
        int review_last_warned_bucket "7 or 3 or NULL, tracks which review notice fired (DLG-Q6)"
        int review_window_days "per-delegation override, range 1..180 (DEL-14)"
        delegation_status status "ENUM active-ended-cancelled (DEL-3)"
        bigint record_version "optimistic lock"
        timestamptz created_at
        timestamptz updated_at
        timestamptz deleted_at "soft-delete"
    }
    delegation_tenant_settings {
        uuid tenant_id PK "one policy row per tenant (DLG-Q2)"
        int max_duration_days "range 1..180, default 90 (DEL-14)"
        int review_window_days "range 1..180, default 90 (DEL-13/14)"
        bigint record_version
        timestamptz created_at
        timestamptz updated_at
    }
    processed_events {
        text event_id PK "envelope id, SQS dedup"
        text consumer PK "cascade or offboarding"
        timestamptz processed_at
    }
```

## Key request flows

One sequence diagram per meaningful path (LLD §11), covering the shared preamble every public route runs, the two mutating public endpoints, the two CronJobs, the inbound cascade, and the remaining read/policy endpoints. See `docs/architecture/README.md` for the full index.

### Request preamble

Every public route runs the same gateway hand-off before reaching a handler.

> Source: [`docs/architecture/mermaid/request-preamble-flow.mmd`](docs/architecture/mermaid/request-preamble-flow.mmd)

```mermaid
sequenceDiagram
    autonumber
    actor C as Caller
    participant E as Envoy Gateway
    participant AZ as AuthZ Enrichment
    participant DLG as Delegation Service
    C->>E: HTTP request (Bearer JWT)
    E->>E: jwt_authn validate signature, iss, exp
    E->>AZ: ext_authz (gRPC)
    AZ-->>E: Allow plus x-tenant-id, x-user-id, x-tenant-roles
    E->>DLG: forward request plus x-* headers (mTLS)
    Note over DLG: RequireAuth, ContextMiddleware, GUC-bridge<br/>set app.tenant_id (txn-local, RLS-6)
    alt headers missing or invalid
        DLG-->>C: 401 unauthorized
    else caller lacks required role (DLG-3/4/5/7)
        DLG-->>C: 403 insufficient_role
    end
```

### Create (DLG-2)

Availability-first ordering: both membership checks, then User Profile, then the atomic write + outbox enqueue.

> Source: [`docs/architecture/mermaid/create-flow.mmd`](docs/architecture/mermaid/create-flow.mmd)

```mermaid
sequenceDiagram
    autonumber
    actor DR as Delegator (or admin)
    participant DLG as Delegation Service
    participant Core as Core (Org and Membership)
    participant UP as User Profile
    participant PG as Delegation Postgres

    DR->>DLG: POST /api/v1/delegations {...} + Idempotency-Key
    DLG->>DLG: idempotency check (del:idem), pre-flight (self-delegation, scope_id, bounds vs local delegation_tenant_settings)
    par membership checks (both parties)
        DLG->>Core: GET /internal/tenants/:id/members/:delegator_id/exists
        DLG->>Core: GET /internal/tenants/:id/members/:delegate_id/exists
    end
    alt either Core unreachable
        DLG-->>DR: 503 org_membership_unavailable
    else either not active
        Core-->>DLG: {active false}
        DLG-->>DR: 422 invalid_delegate
    else both active (each returns tenant_membership_id)
        DLG->>UP: PUT /internal/users/:delegator_id/availability {status ooo, delegate_id, ooo_note, ends_at}
        alt UP 5xx/timeout
            DLG-->>DR: 503 user_profile_unavailable
        else UP 200
            DLG->>PG: RunInTx { INSERT delegations with both membership ids, then outbox DelegationStarted }
            DLG->>DLG: SET del:idem, DEL del:list cache
            DLG-->>DR: 201 Created
            Note over DLG,PG: DelegationStarted to iam.delegation.events, Workflow reroutes
        end
    end
```

### Cancel (DLG-3) — fail-open

> Source: [`docs/architecture/mermaid/cancel-flow.mmd`](docs/architecture/mermaid/cancel-flow.mmd)

```mermaid
sequenceDiagram
    autonumber
    actor U as Delegator / admin
    participant DLG as Delegation Service
    participant UP as User Profile
    participant PG as Delegation Postgres

    U->>DLG: DELETE /api/v1/delegations/:id?record_version=N
    DLG->>PG: FindByID (RLS-scoped)
    DLG->>UP: PUT /internal/users/:delegator_id/availability {delegate_id null} (clear pointer only, never status available)
    Note over DLG,UP: fail-open — if UP is down, log and proceed, the expiry cron re-clears later
    DLG->>PG: RunInTx { End status=cancelled at record_version N, then outbox DelegationEnded cancelled }
    alt version mismatch
        DLG-->>U: 409 optimistic_lock_conflict
    else terminal already
        DLG-->>U: 404 delegation_not_found
    else ok
        DLG-->>U: 200 {delegation}
    end
```

### Expiry cron (DLG-I1) — self-retrying

> Source: [`docs/architecture/mermaid/expiry-cron-flow.mmd`](docs/architecture/mermaid/expiry-cron-flow.mmd)

```mermaid
sequenceDiagram
    autonumber
    participant CR as delegation-expiry CronJob
    participant DLG as Delegation Service
    participant UP as User Profile
    participant PG as Delegation Postgres

    CR->>DLG: POST /internal/delegations/expire
    DLG->>PG: SELECT ... WHERE status=active AND deleted_at IS NULL AND ends_at IS NOT NULL AND ends_at < now() LIMIT 50
    loop per expired delegation
        DLG->>UP: PUT /internal/users/:delegator_id/availability {delegate_id null}
        alt UP fail
            DLG->>DLG: leave active, defer (DEL-6), retry next tick
        else UP 200
            DLG->>PG: RunInTx { bind app.tenant_id, UPDATE status=ended at record_version v, then outbox DelegationEnded expired }
        end
    end
    DLG-->>CR: {attempted, succeeded, failed}
```

### Review-window cron (DLG-I2) — dual 7d/3d warn, then auto-end

> Source: [`docs/architecture/mermaid/review-cron-flow.mmd`](docs/architecture/mermaid/review-cron-flow.mmd)

```mermaid
sequenceDiagram
    autonumber
    participant CR as delegation-review CronJob
    participant DLG as Delegation Service
    participant UP as User Profile
    participant PG as Delegation Postgres

    CR->>DLG: POST /internal/delegations/review-sweep
    DLG->>PG: Pass 1a (7d warn) WHERE ends_at IS NULL AND status=active AND review_due_at in (now()+3d, now()+7d] AND review_last_warned_bucket IS NULL
    loop per 7d target
        DLG->>PG: RunInTx { UPDATE review_last_warned_bucket=7, then outbox DelegationReviewRequested days_remaining=7 }
    end
    DLG->>PG: Pass 1b (3d warn) WHERE ends_at IS NULL AND status=active AND review_due_at in (now(), now()+3d] AND review_last_warned_bucket IS DISTINCT FROM 3
    loop per 3d target
        DLG->>PG: RunInTx { UPDATE review_last_warned_bucket=3, then outbox DelegationReviewRequested days_remaining=3 }
    end
    DLG->>PG: Pass 2 (auto-end) WHERE ends_at IS NULL AND status=active AND review_due_at <= now()
    loop per end target
        DLG->>UP: PUT /internal/users/:delegator_id/availability {delegate_id null}
        alt UP ok
            DLG->>PG: RunInTx { UPDATE status=ended, then outbox DelegationEnded review_expired }
        else UP fail
            DLG->>DLG: defer (DEL-6), retry next tick
        end
    end
    DLG-->>CR: {warned_7d, warned_3d, expired, deferred}
```

### Cascade removal — Core `MembershipRevoked`

The stranding hazard (workflow reassignment) is resolved synchronously in Core before removal; row-ending here is asynchronous and safe.

> Source: [`docs/architecture/mermaid/cascade-removal-flow.mmd`](docs/architecture/mermaid/cascade-removal-flow.mmd)

```mermaid
sequenceDiagram
    autonumber
    actor Admin
    participant Core as Core (MembershipService.RemoveUser)
    participant WF as Workflow Service
    participant DLG as Delegation Service
    participant PG as Delegation Postgres
    participant UP as User Profile

    Note over Core,WF: SYNCHRONOUS gate stays in Core
    Admin->>Core: DELETE user (P-8 / I-5)
    Core->>WF: GetDelegateImpact(tenant, user, optional delegation_id)
    Core->>DLG: dept-level trigger, GET /internal/delegations/dept-delegate to get delegation_id (WFI-11)
    alt active_workflows > 0
        Core-->>Admin: 409 workflow_resolution_required
        Admin->>Core: P-26 (replace_delegate to ReassignDelegate, or stop_workflows to CancelByDelegate)
        Core->>WF: reassign or cancel, then re-check GetDelegateImpact (WFI-6)
    end
    Note over Core,DLG: removal applied, ASYNC row-end here
    Core->>Core: RunInTx { remove membership }, then emit MembershipRevoked
    Core-->>DLG: MembershipRevoked (SQS delegation-cascade-q)
    DLG->>PG: end all active delegations where delegator_id=user OR delegate_id=user
    loop per ended delegation
        DLG->>UP: PUT /internal/users/:delegator_id/availability {delegate_id null} (DEL-6)
        DLG->>PG: RunInTx { UPDATE status=ended, then outbox DelegationEnded delegate_removed } (delegate-side, delegator-side silent per DLG-EVT-4)
    end
```

### Read and policy endpoints (DLG-1/4/5/6/7)

> Source: [`docs/architecture/mermaid/read-policy-flow.mmd`](docs/architecture/mermaid/read-policy-flow.mmd)

```mermaid
sequenceDiagram
    autonumber
    actor C as Caller
    participant DLG as Delegation Service
    participant V as Valkey (del:)
    participant PG as Delegation Postgres

    Note over C,PG: DLG-1 GET /delegations (list, self)
    C->>DLG: GET /api/v1/delegations
    DLG->>V: GET del:list:{tenant}:{caller}
    alt hit
        V-->>DLG: cached list
    else miss
        DLG->>PG: SELECT ... WHERE tenant_id=$1 AND delegator_id=$2 AND deleted_at IS NULL
        DLG->>V: SET del:list (60s)
    end
    DLG-->>C: 200 {items}

    Note over C,PG: DLG-4 POST /delegations/:id/extend (open-ended only, no UP call)
    C->>DLG: POST /delegations/:id/extend {extend_days?}
    DLG->>PG: FindByID (RLS-scoped)
    alt fixed ends_at (review_due_at IS NULL)
        DLG-->>C: 422 not_review_tracked
    else extend_days outside [1,180]
        DLG-->>C: 422 extend_days_out_of_range
    else ok
        DLG->>PG: RunInTx { ExtendReview push review_due_at, reset review_last_warned_bucket=NULL at record_version N }
        DLG-->>C: 200 {review_due_at, review_last_warned_bucket=null}
    end

    Note over C,PG: DLG-5 POST /delegations/:id/reassign (end old, create new)
    C->>DLG: POST /delegations/:id/reassign {new_delegate_id?, scope?, ...}
    DLG->>DLG: Cancel(old) flow (§11.2) — emits DelegationEnded cancelled
    DLG->>DLG: Create(new) flow (§11.1) — membership checks + UP + DelegationStarted
    Note over DLG: if the new create fails after the old ended, old is NOT resurrected (DLG-D11)
    DLG-->>C: 201 {new delegation}

    Note over C,PG: DLG-6/7 tenant policy (settings)
    C->>DLG: GET /delegations/settings
    DLG->>PG: SELECT ... FROM delegation_tenant_settings WHERE tenant_id=$1 (default 90/90 if no row)
    DLG-->>C: 200 {max_duration_days, review_window_days}
    C->>DLG: PUT /delegations/settings {max_duration_days, review_window_days}  (tenant_admin/owner)
    alt either outside [1,180]
        DLG-->>C: 400 invalid_delegation_max_duration_days / invalid_delegation_review_window_days
    else ok
        DLG->>PG: UPSERT delegation_tenant_settings (optimistic-locked)
        DLG-->>C: 200 {settings, record_version}
    end
```

Not diagrammed: LLD §11.6 tenant-lifecycle cleanup (`TenantOffboarded` → soft-delete, monthly hard-purge) is one sentence of prose in the LLD, not a sequence.

## Package dependency graph

Arrows represent Go `import` relationships (module-internal only), read from each package's actual import block — not derived from the Layer model diagram above, which shows composition-root wiring rather than package-level imports. `core/domain` imports nothing internal (LLD §6.2); `core/port` imports only `domain`; the two `core/service` types (`DelegationService`/`SettingsService` and `CascadeService`) both stay inside `domain`+`port` (+`requestctx` for the two DLG-1..7 services). Notably, `adapter/inbound/consumer` does **not** import `core/service` directly — it accepts `*service.CascadeService` through a small duck-typed interface defined locally in the `consumer` package (the same seam pattern as `postgres.PayloadValidator`), so the inbound adapter layer never has a compile-time dependency on the service layer's concrete type. `adapter/outbound/metrics` and `pkg/requestctx` have zero internal imports.

> Source: [`docs/architecture/mermaid/package-dependencies.mmd`](docs/architecture/mermaid/package-dependencies.mmd)

```mermaid
graph LR
    domain["core/domain\n(no internal imports —\nstdlib + uuid only, LLD §6.2)"]
    port["core/port\n(imports domain only)"]
    service["core/service\nDelegationService · SettingsService\n(imports domain, port, requestctx)"]
    cascadeservice["core/service\nCascadeService\n(imports domain, port)"]
    requestctx["pkg/requestctx\n(no internal imports)"]
    eventschema["internal/eventschema\n(no internal imports)"]

    http["adapter/inbound/http\n(imports domain, service, requestctx)"]
    consumer["adapter/inbound/consumer\n(imports domain only — accepts\n*service.CascadeService via a\nlocal duck-typed interface, no\ndirect service import)"]

    postgres["adapter/outbound/postgres\n(imports domain, port)"]
    userprofile["adapter/outbound/userprofile\n(imports port only)"]
    orgmembership["adapter/outbound/orgmembership\n(imports port only)"]
    eventbus["adapter/outbound/eventbus\n(imports domain, eventschema)"]
    valkey["adapter/outbound/valkey\n(imports domain, port)"]
    metrics["adapter/outbound/metrics\n(no internal imports —\npure prometheus/client_golang)"]

    port --> domain
    service --> domain
    service --> port
    service --> requestctx
    cascadeservice --> domain
    cascadeservice --> port
    http --> domain
    http --> service
    http --> requestctx
    consumer --> domain
    postgres --> domain
    postgres --> port
    userprofile --> port
    orgmembership --> port
    eventbus --> domain
    eventbus --> eventschema
    valkey --> domain
    valkey --> port
```

## Cache strategy

Much simpler than a projection-heavy cache: Valkey `del:` keyspace, two keys, both advisory — correctness never depends on either (LLD §9).

| Key | Value | TTL | Invalidated by |
|---|---|---|---|
| `del:list:{tenant}:{delegator}` | DLG-1 list projection | 60 s | DLG-2/3/4/5 for that delegator (explicit `DEL` on write) |
| `del:idem:{tenant}:{key}` | `{delegation_id, status}` for a create `Idempotency-Key` (DLG-Q3) | 24 h | TTL only |

**Read algorithm (DLG-1, LLD §9.1):** `GET del:list:{tenant}:{delegator}` → on hit, deserialize and return; on miss, `SELECT ... FROM delegations WHERE tenant_id=$1 AND delegator_id=$2 AND deleted_at IS NULL ORDER BY starts_at DESC`, then `SET` the key with a 60 s TTL before returning. **Failure mode (LLD §9.3):** a down Valkey falls straight through to Postgres on every read and the idempotency check degrades to best-effort (a retried create within the outage window could in rare cases create a duplicate delegation rather than replaying the original `201`) — correctness of the delegation data itself is never affected, only the idempotency guarantee's strength. There is no cross-service config cache (tenant policy is a local table, `delegation_tenant_settings`) and no I-8 projection cache — both existed in the O&M-era design and were removed under Option C.

## Observability stack

**Tracing:** entirely `platform-gincommon`'s — `gincommon.TracingOptions` configures OTel export in `cmd/server/main.go`; there is no hand-rolled span code in this service. The outbox stamps `trace_id` onto every published event (LLD §14.3) so a create → `DelegationStarted` → Workflow reroute is one trace end-to-end.

**Logging:** structured via `slog`, wired through `platform-gincommon` the same way every sibling IAM service does. Logs carry `delegation_id`, `tenant_id`, `actor`, and DEL-6 defer reasons (LLD §14.3) — no PII beyond what's already gateway-scoped.

**Metrics:** `internal/adapter/outbound/metrics` defines every `iam_delegation_*` Prometheus instrument this service is specified to emit (LLD §14.2: `iam_delegation_created_total{scope}`, `iam_delegation_ended_total{ended_reason}`, `iam_delegation_active_gauge{tenant}`, `iam_delegation_expiry_deferred_total`, `iam_delegation_review_deferred_total`, `iam_delegation_review_warned_total{days_remaining}`, `iam_delegation_review_expired_total`, `iam_delegation_membership_check_duration_seconds`, `iam_delegation_membership_check_failures_total`, `iam_delegation_up_availability_failures_total{path}`, `iam_delegation_idempotency_hits_total`, `iam_delegation_cascade_processed_total`, `iam_delegation_cascade_dlq_total`), registered onto `gincommon.MetricsRegisterer()` at `cmd/server` startup. Generic per-request HTTP metrics and SQS/events metrics are deliberately not reimplemented here — `gincommon.ObservabilityMiddlewares` and `platform-events`' consumer already emit `http_*`/`events_*`/`sqs_*`.

**Known gap (DLG-D19, `IMPLEMENTATION_NOTES.md`):** the `Metrics` type and its instruments are registered but **no call site in `internal/core/service` or `cmd/reconciler/jobs` actually calls a `Metrics` method today** — none of the service-layer constructors accept a `*metrics.Metrics`. Every `iam_delegation_*` counter above will report zero until that wiring lands; this is explicitly scoped as follow-up work, not a design gap. Don't build dashboards or alerts against these business metrics yet — only the passthrough `http_*`/`events_*`/`sqs_*` metrics are live.

## Row-Level Security (RLS) and GUC injection

Unlike a service with a single gateway-bridged GUC path, this service has **three distinct binding paths** converging on the same `postgres.WithTenantGUC` call (DLG-D18) — the LLD's RLS/GUC wiring description (§7.3/§13.1) doesn't specify the exact seam, so this is "as built":

1. **Public routes (DLG-1..7):** `tenantGUCMiddleware(bindTenantGUC)`, registered on the public route group right after `ContextMiddleware()` (`router.go`). Runs once per request, before the handler, binding from the already-bridged `requestctx.RequestContext`.
2. **Mesh-only internal reads (DLG-I3/I4):** `gucBoundReader` (`cmd/server/adapters.go`) wraps `*postgres.DelegationRepository` and binds `app.tenant_id` (with `userID="iam-system"`) **per call**, immediately before each single-statement query — no middleware runs on `/internal/*` (there's no gateway identity to bridge there), and this is safe specifically because these are single-statement reads outside any transaction opened earlier by a `TxRunner`.
3. **Reconciler jobs and the cascade consumer:** `jobs.Context.BindTenantGUC` and `consumer.GUCBinder` are injected function parameters (both satisfied by `postgres.WithTenantGUC` at wire-up in `cmd/reconciler/main.go` / `cmd/server/main.go`), called explicitly per row before each `RunInTx` — there's no HTTP request to bridge from in a cron or an SQS message handler.

All three ultimately call the same `postgres.WithTenantGUC(ctx, tenantID, userID)`, which sets a `pgcommon.GUCSet` on `ctx`; the next pool checkout issues `SET LOCAL app.tenant_id = '...'` (transaction-scoped — CI greps for the forbidden session-scoped form via `.github/scripts/check-forbidden-set-guc.sh`, RLS-6), and Postgres enforces `FORCE ROW LEVEL SECURITY` with a fail-closed `NULLIF` policy on both `delegations` and `delegation_tenant_settings` (LLD §7.3).

> Source: [`docs/architecture/mermaid/rls-guc-flow.mmd`](docs/architecture/mermaid/rls-guc-flow.mmd)

```mermaid
graph TD
    subgraph path1["Path 1 — Public routes (DLG-1..7)"]
        p1a["tenantGUCMiddleware(bindTenantGUC)\nregistered after ContextMiddleware()\non the public route group (router.go)"]
        p1b["binds ctx from requestctx.FromContext(rc)\nonce per request, before the handler runs"]
        p1a --> p1b
    end

    subgraph path2["Path 2 — Mesh-only internal reads (DLG-I3/I4)"]
        p2a["gucBoundReader (cmd/server/adapters.go)\nwraps *postgres.DelegationRepository"]
        p2b["binds app.tenant_id + userID=\"iam-system\"\nper call, immediately before each\nsingle-statement query — no middleware,\nno open tx (LLD §7.3/RLS-6)"]
        p2a --> p2b
    end

    subgraph path3["Path 3 — Reconciler jobs + cascade consumer"]
        p3a["jobs.Context.BindTenantGUC (cmd/reconciler/jobs)\nconsumer.GUCBinder (internal/adapter/inbound/consumer)\ninjected function params — no HTTP request to bridge"]
        p3b["called explicitly per-row before each\nRunInTx (delegation_expiry.go, delegation_review.go,\ncascade_consumer.go)"]
        p3a --> p3b
    end

    withTenantGUC["postgres.WithTenantGUC(ctx, tenantID, userID)\n(internal/adapter/outbound/postgres/db.go)\nall three paths converge here"]

    p1b --> withTenantGUC
    p2b --> withTenantGUC
    p3b --> withTenantGUC

    gucset["pgcommon.WithGUCSet(ctx, GUCSet{TenantID, UserID})"]
    withTenantGUC --> gucset

    setlocal["Next pool checkout issues:\nSET LOCAL app.tenant_id = '...'\n(transaction-scoped, never session-scoped —\nCI greps for the forbidden SET form,\n.github/scripts/check-forbidden-set-guc.sh)"]
    gucset --> setlocal

    rls["Postgres RLS — FORCE ROW LEVEL SECURITY,\nfail-closed NULLIF policy on delegations\nand delegation_tenant_settings (LLD §7.3)"]
    setlocal --> rls
```

## Concurrency and optimistic locking

`record_version` is trigger-managed (TRG-1..3, LLD §7.5) on `delegations`. Cancel/extend/reassign issue `UPDATE ... WHERE id=$1 AND record_version=$2`, mapping zero affected rows to `409 optimistic_lock_conflict` (current version echoed) or `404 delegation_not_found` (terminal/absent) — the cascade consumer's bulk row-end is deliberately **not** optimistic-locked (LLD §12.1), since it's a terminal, one-directional bulk operation, not a user-driven read-modify-write.

Create (DLG-2) runs its two grant-time membership checks **concurrently** (`par` in the LLD §11.1 sequence — a Go `errgroup`/goroutine pair against Core, not sequential round-trips), then the availability-first User Profile call, then the atomic `INSERT` + outbox enqueue inside one `RunInTx`. The mandatory `Idempotency-Key` (DLG-D8/DLG-Q3, 24 h TTL in `del:idem:`) makes retries of that whole sequence safe — a repeated key returns the original `201` rather than re-running the side effects — which also closes the gap left by there being no `UNIQUE(tenant_id, delegator_id)` constraint (a user may legitimately hold multiple concurrent delegations of different scopes, so that constraint would be wrong, not just missing).

## Failure domains

| Scenario | Effect on this service | Effect on the platform |
|---|---|---|
| Core (`iam-org-membership`) membership-check down, during create | `503 org_membership_unavailable`, no write (DLG-FAIL-1) | New grants blocked; I-8 unaffected (Option C removed that coupling entirely) |
| User Profile down, during create | `503 user_profile_unavailable`, no write | Create blocked, retryable by the caller |
| User Profile down, during expiry/review cron or the cascade row-end | Row left active, deferred, retried next tick (DEL-6) | Pointer re-cleared on the next successful tick — no split-brain |
| User Profile down, during cancel (DLG-3) | Fail-open — cancel proceeds anyway | Interactive cancel is never blocked by an availability-service outage; the pointer is re-cleared by the next expiry tick |
| This service itself is down | DLG-1..7 unavailable; CronJobs paused | I-8 unaffected; Core's user-removal gate degrades to tenant-wide impact instead of department-precise (DLG-I3 unreachable) — still correct, just less precise |
| Postgres down | All reads/writes fail | No fallback — this is the system of record |
| Valkey down | DLG-1 list falls through to Postgres; idempotency check degrades to best-effort | Slightly higher latency; delegation data correctness is unaffected (see Cache strategy above) |
| SQS/SNS down | Outbox retries up to `OUTBOX_MAX_ATTEMPTS`, then dead-letters | Downstream consumers (Workflow, Notification, Audit) see delayed or (after DLQ) missing events until manually reprocessed |

## Key invariants

Hard invariants this service maintains, drawn from the LLD's DLG-D/DEL-/DLG-EVT- decision families (§10.7/§12.4/§12.5):

- **DLG-FAIL-1/2/3:** a Core or User Profile outage during create fails cleanly with no partial write; this service's own outage never produces a wrong I-8 authorization answer (I-8 no longer references delegations at all under Option C); every synchronous call this service makes is on its own write path — the only coupling *toward* Core (the removal cascade) is asynchronous.
- **DLG-EVT-3/DEL-7:** `DelegationEnded` fires on every end path (cancel, expiry, review auto-end, delegate-removed cascade) — never path-dependent — **except** delegator-side removal, which ends the row silently with no event at all (DLG-EVT-4, a deliberate carried-over asymmetry from O&M).
- **DLG-EVT-5:** the review sweep emits `DelegationReviewRequested` at most once per bucket (7 then 3) per delegation per cycle, tracked by `review_last_warned_bucket`; `review_last_warned_bucket` only applies to open-ended delegations (`ends_at IS NULL`) — fixed-`ends_at` delegations are never considered by the review sweep at all.
- **DLG-EVT-7:** every published payload is a self-contained snapshot, not a delta — a consumer that never saw a `DelegationStarted` can still safely process the matching `DelegationEnded` (nothing to restore).
- **DLG-D11:** if a reassign's new-delegation create fails after the old delegation was already ended, the old delegation is **not** resurrected — the caller sees a partial failure (old ended, new not created) rather than the service attempting a compensating undo.
- **DLG-D12:** v1 is active-at-create only — no future-dating; a delegation is either active immediately or rejected, never scheduled to start later.
- **DLG-D8/DLG-Q3:** the create idempotency window is exactly 24 h, keyed on `(tenant, Idempotency-Key)`, not on any business field.

## Consumer conformance checklist

`iam.delegation.events` has three real consumers (LLD §10.4): **Workflow Service** (`DelegationStarted`/`DelegationEnded` — reroute/restore, DEL-4/DEL-5), **Notification** (all three types), and **Audit Log** (all three, no filter). All three payload schemas declare `"additionalProperties": true` — an explicit open-schema contract enforced by CI (`schema-gov validate` Pass 5 rejects any schema that re-introduces `additionalProperties: false`). Before subscribing, a consumer must be configured for lenient decoding — see `api/asyncapi.yaml`'s `x-forward-compatibility.required-settings` block (added alongside the CI governance pipeline, DLG-D20) for the exact Go `encoding/json`/`sonic` settings; this service's own consumers are other internal Go services, so that block only documents the Go-side settings, unlike a public multi-language contract.

## Schema lifecycle

Schema states (`active`/`deprecated`/`retired`) and the compaction protocol for a schema that has accumulated too many Glue versions are governed by `api/asyncapi.yaml`'s `x-version-governance`/`x-usage-override` blocks and enforced by the CI pipeline added under DLG-D20 (`schema-registry.yml` validates/registers on every push, `schema-prune.yml` handles monthly orphan cleanup, `schema-health-quarterly.yml` is a read-only quarterly report, `freeze-watchdog.yml` guards against a forgotten `SCHEMA_FREEZE`). See `docs/runbook-schema-registry.md` for the operational detail (pre-deploy checklist, failure modes, adding a new event type) — this section intentionally doesn't re-derive it.

## Trust boundaries and isolation

This is deliberately narrower than a formal threat model — the LLD (§13) documents trust boundaries and isolation layers, not a STRIDE-style analysis, and this section doesn't invent one:

- **Tenant isolation, three layers (LLD §13.1):** Postgres RLS (`FORCE`, fail-closed `NULLIF` policy on both tables — see RLS and GUC injection above); `SET LOCAL app.tenant_id` per transaction, never session-scoped; gateway-identity headers (`x-tenant-id`/`x-user-id`/`x-tenant-roles`), never a request-body-derived tenant or actor.
- **Network isolation (LLD §13.2):** public routes sit behind Envoy; `/internal/*` (DLG-I1..I4) is mesh-only mTLS via the internal-route guard — there is no RBAC/JWT check on those routes because there's no gateway identity to check, only the network boundary. Cross-service calls to `iam-org-membership`/`iam-user-profile` are intra-mesh mTLS.
- **Input validation (LLD §13.3):** mirrors the DB CHECK constraints and DEL invariants directly — scope enum membership, scope↔scope_id consistency, `reason` length ≤ 500, `starts_at` within `(now()−skew, now()+1yr]`, `ends_at > starts_at`, span ≤ tenant `max_duration_days`, `extend_days`/policy days in `[1,180]`.

## Developer tools

Swagger UI (`/swagger`) and the AsyncAPI viewer (`/asyncapi`, raw spec at `/asyncapi.yaml`) are described from a usage angle in `README.md`; architecturally, both are served from the same binary with no external tooling dependency: `api/embed.go`'s `//go:embed asyncapi.yaml` compiles the spec directly into the binary (`apispec.AsyncAPISpec`), and `router.go` mounts both doc routes gated by the same `DocsConfig` (`DOCS_ENABLED`/`DOCS_AUTH_TOKEN`, active outside `production` unconditionally, opt-in with an optional bearer-token gate in `production`).

## Documentation assets

This repo's own doc assets, beyond this file: `docs/lld/iam-lld-delegation-service.md` (the full LLD — data model, event architecture, request flows, observability, testing strategy in depth), `docs/architecture/` (this file's diagram sources, indexed in `docs/architecture/README.md`), `docs/runbook-schema-registry.md` (Glue Schema Registry operations), `docs/swagger/` (generated OpenAPI spec), `CHANGELOG.md`, and `IMPLEMENTATION_NOTES.md` (the as-built decision register, DLG-D13 onward). See "Where to look next" below for pointers by topic.

## Decision Register (LLD §23, DLG-D1 through DLG-D12)

These are the decisions the original Low-Level Design
(`docs/lld/iam-lld-delegation-service.md`) shipped with, made before this repo existed:

| # | Decision |
|---|---|
| DLG-D1 | **Option C for I-8** — `active_delegations[]` is removed from I-8 and Core drops the `delegations` table entirely; the would-be consumers (Workflow, Dashboard, AuthZ Enrichment) are re-sourced to this service's own events/endpoints instead (§6.1). |
| DLG-D2 | **Tenant delegation policy moves into this service** as `delegation_tenant_settings`, read in-process at create/extend; Core drops the two `tenants` columns it used to own. |
| DLG-D3 | **The two lost composite membership FKs → two synchronous grant-time checks** against Core's `GET /internal/tenants/:id/members/:user_id/exists`, which must return `tenant_membership_id` when active, behind a swappable `MembershipCheckClient`. |
| DLG-D4 | **The lost `fk_del_tenant` cascade → an async tenant-offboarding consumer** on `delegation-cascade-q` (§11.6). |
| DLG-D5 | **Delegation events get a dedicated topic**, `iam.delegation.events` (`source: iam-delegation`); Core's own AsyncAPI drops them entirely. |
| DLG-D6 | **`DelegationEnded.ended_reason` gains `review_expired`** — the shipped code's value becomes contract, so review auto-ends are distinguishable from plain `ends_at` expiry. |
| DLG-D7 | **The review sweep warns twice, at 7 d and 3 d**, tracked via `review_last_warned_bucket`; `days_remaining` is always one of `{7, 3}`. |
| DLG-D8 | **Create takes a mandatory `Idempotency-Key`** (24 h dedup), closing the duplicate-on-retry gap left by the absence of a `UNIQUE(tenant_id, delegator_id)` constraint. |
| DLG-D9 | **The synchronous removal gate stays in Core; only the row-end moves here (async)** — Core's dept-scope precision check becomes a Core-to-Delegation call (DLG-I3) on the admin removal path. |
| DLG-D10 | **`port.UserProfileClient` + DEL-6 availability-first / pointer-clear-only move here intact** — cancel is fail-open; scheduled ends defer-and-retry rather than fail. |
| DLG-D11 | **Reassign takes the fuller body** (`new_delegate_id?/scope?/scope_id?/ends_at?/reason?`), and the error taxonomy is canonicalised (§20). |
| DLG-D12 | **v1 scope is active-at-create only** — no future-dating; future-dated activation is deferred to a v2 start-scheduler feature. |

## Session-specific decisions (DLG-D13 onward)

Decisions **DLG-D13 through DLG-D17** — made during *this* build, not present in the original
LLD — are recorded separately in `IMPLEMENTATION_NOTES.md` (written by a later build step, not
this one). Nothing in this document or in the Helm chart/CI scaffolding contradicts that file;
where this build had to make its own call ahead of that record existing (e.g. the
`--job=<name>` CronJob dispatch convention, or the `CRON_BATCH_LIMIT` env var name), it is
called out inline as "this chart's own convention" rather than asserted as an LLD decision.

## Where to look next

- `README.md` — what the service does, local dev setup, testing.
- `CHANGELOG.md` — notable changes.
- `deploy/helm/iam-delegation/values.yaml` — every configuration key this service reads, with
  production defaults; the single source of truth `docker-compose.yml` and this document's env
  var references are kept in sync with.
- `docs/lld/iam-lld-delegation-service.md` — the full LLD, in particular §7 (data
  model/RLS), §10 (event architecture), §11 (request flows), §14 (observability/alerting), and
  §17 (testing strategy).
