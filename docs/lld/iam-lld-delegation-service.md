# Delegation Service — Low-Level Design

## Tender Management SaaS Platform — IAM Subsystem

| Field | Value |
|---|---|
| Document Type | Low-Level Design (LLD) |
| Service | Delegation Service (`iam-delegation`) |
| Parent decision | **ADR-0008** (`02-hld-delta-delegation.md`, v2 / Option C) — the fourth O&M extraction, authorised by ADR-0007's explicit deferral of `delegations` |
| Subsystem | Identity & Access Management |
| Wave | 4 of 4 (the deferred hot-path table; resolved by removing delegations from I-8 entirely) |
| Version | 2.1 |
| Date | 2026-08-20 |
| Status | Approved for implementation |
| Audience | IAM platform engineering (owner), Core Org & Membership engineering (drops `delegations`; membership-existence + dept-delegate callee; removal-signal producer), Workflow Service (delegation-event consumer), User Profile (availability callee), AuthZ Enrichment (drops `active_delegations[]`), SRE |
| Owner database | RDS PostgreSQL `delegation` (Multi-AZ, PgBouncer transaction pooling) |
| Go module | `github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation` |

### Revision history

| Version | Date | Notes |
|---|---|---|
| 1.0 | 2026-08-20 | Initial extraction LLD under a no-contract-change constraint (**Option A** — Core kept an event-synced `delegations` projection so I-8 still served `active_delegations[]`). Carried O&M invariants DEL-1…DEL-14, introduced the `DLG-*` families, and left DLG-Q1…Q10 as sign-off items. |
| 2.0 | 2026-08-20 | **Free-hand revision — the IAM subsystem is in development, nothing deployed.** With the backward-compatibility constraint lifted, DLG-Q9 resolves to **Option C**: `active_delegations[]` is removed from I-8 and **Core drops the `delegations` table entirely**, making the extraction symmetric with the three ADR-0007 cuts and leaving I-8 *faster* (four-table join). All other open questions are resolved as decisions (§19/§23): dedicated event topic `iam.delegation.events` (Q1); tenant delegation policy moves into this service as `delegation_tenant_settings` (Q2); mandatory create idempotency key (Q3); Core `MembershipRevoked`/`TenantOffboarded` signals (Q4); `ended_reason` gains `review_expired` (Q5); dual 7 d/3 d review warnings (Q6); fuller reassign body (Q7); canonical error taxonomy (Q8); active-at-create v1 scope (Q10). v1's projection is retained only as a documented fallback (§22). |
| 2.1 | 2026-08-20 | **Depth/quality uplift to the `iam-lld-user-profile.md` standard.** No design change — this revision raises the document to the maturity bar of the User Profile LLD: §10 Event Architecture is expanded to that doc's §7 depth (inbound-consumer table with queue/DLQ/`maxReceiveCount`/filter policy; serialization; a full hand-authored `api/asyncapi.yaml` 3.0.0 skeleton with an `EventEnvelope` + `allOf` per-payload schemas; a Glue Schema Registry layout table, schema-evolution rules, and enqueue-vs-publish Go codec wiring; an outbound consumer→queue→DLQ fan-out table; a published-events catalogue; and an idempotency/ordering subsection). §7.3 RLS is expanded to the three-function fail-closed design (`app_tenant_id()`/`rls_check_tenant()`/`log_rls_violation()`) + `rls_violation_log` + per-table policies + roles + CI-verified RLS-1/2/4/6. §7.5 now shows the `touch_row()` trigger SQL + TRG-1/2/3. §11 gains a shared request preamble (§11.0) and per-endpoint diagrams for the remaining routes (§11.7). §14.1 gains a second SLO table (cache-vs-DB + background freshness) and a 99.9% availability target. No schema, API, or event *content* changed — this revision only brings the write-up to the sibling doc's standard. |

---

## Table of Contents

1. [Service Overview](#1-service-overview)
2. [Responsibilities](#2-responsibilities)
3. [Non-goals](#3-non-goals)
4. [Document Overview](#4-document-overview)
5. [Service Responsibilities and Boundaries](#5-service-responsibilities-and-boundaries)
6. [Architecture and Package Layout](#6-architecture-and-package-layout)
7. [Data Model](#7-data-model)
8. [API Contract](#8-api-contract)
9. [Caching Design](#9-caching-design)
10. [Event Architecture](#10-event-architecture)
11. [Key Request Flows](#11-key-request-flows)
12. [Concurrency, Consistency, and Failure Handling](#12-concurrency-consistency-and-failure-handling)
13. [Security](#13-security)
14. [Observability](#14-observability)
15. [Configuration](#15-configuration)
16. [Deployment and Scaling](#16-deployment-and-scaling)
17. [Testing Strategy](#17-testing-strategy)
18. [GDPR, Data Lifecycle, and Compliance](#18-gdpr-data-lifecycle-and-compliance)
19. [Open Questions and Sign-off Register](#19-open-questions-and-sign-off-register)
20. [Appendix — Error Taxonomy](#20-appendix--error-taxonomy)
21. [Migration Plan](#21-migration-plan)
22. [Future Options](#22-future-options)
23. [Decision Register](#23-decision-register)

---

## 1. Service Overview

The Delegation Service (`iam-delegation`) is the system of record for **out-of-office (OOO) delegations** — the authoritative, time-bounded grants that drive workflow rerouting when a user hands their in-flight and incoming work to a colleague. It owns the `delegations` table, the per-tenant delegation policy (`delegation_tenant_settings`), the five public lifecycle endpoints plus two policy endpoints, the two lifecycle CronJobs, and the three delegation events consumed by the Workflow, Notification, and Audit services.

It is the **fourth and last** service carved out of Org & Membership (O&M / "Core"), and the only one whose table sat on a hot path. The other three ADR-0007 extractions were chosen partly because none of their tables was on **I-8** (`GET /api/v1/internal/users/:id/memberships`, 15/30 ms p99 — the platform's tightest SLO). `delegations` was one of the five tables I-8 joined, and I-8's response carried an `active_delegations[]` sub-object. ADR-0007 deferred the table for that reason.

Because the platform is **in development and nothing is deployed**, ADR-0008 resolves the hot-path question by removing the premise rather than engineering around it (**Option C**): `active_delegations[]` is **removed from I-8**, Core **drops the `delegations` table entirely**, and the field's would-be consumers read delegation state where it actually lives — Workflow from events, the dashboard from User Profile's `user_availability.delegate_id` and the Workflow Service, and any future synchronous caller from this service's `GET /internal/users/:id/active-delegations`. No consumer reads I-8's `active_delegations[]` for a decision (§6.1), so dropping it costs nothing and makes I-8 *faster*. This LLD specifies the standalone service and the small, subtractive changes Core and AuthZ Enrichment make to shed the field.

Delegation is a "two records, two owners" concern (O&M §2.3): this service owns the authoritative routing record (`delegations`); User Profile owns the presentation record (`user_availability`). Creating a delegation is an availability-first two-phase coordination between the two (DEL-6), with this service — not Core — holding `port.UserProfileClient`.

---

## 2. Responsibilities

- **Own the `delegations` aggregate** — tenant-scoped, RLS-protected: schema, enums, indexes, optimistic-lock `record_version`, soft-delete, and the `review_*` open-ended-review columns.
- **Own per-tenant delegation policy** — `delegation_tenant_settings` (`max_duration_days`, `review_window_days`), relocated from Core's `tenants` row (DLG-Q2), read in-process at create/extend.
- **Serve the lifecycle + policy API** — DLG-1…DLG-5 (list / create / cancel / extend / reassign) byte-compatible with old P-18/19/20/32/33, plus DLG-6/DLG-7 (get/set tenant policy).
- **Coordinate availability-first with User Profile** — DEL-6 two-phase write on create; pointer-clear-only (`{delegate_id:null}`) on every end path.
- **Run the two reconcilers** — `delegation-expiry` and `delegation-review` (dual 7 d/3 d warnings then auto-end), both availability-first and self-retrying.
- **Produce the delegation events** on the dedicated topic `iam.delegation.events`, driving Workflow reroute/restore, Notification fan-out, and Audit.
- **Run the removal / offboarding cascade** — consume Core's `MembershipRevoked` / `TenantOffboarded` and end affected delegations asynchronously.
- **Answer two internal reads for Core/consumers** — `GET /internal/delegations/dept-delegate` (Core's §8.8.4 removal precision) and `GET /internal/users/:id/active-delegations` (the escape hatch replacing I-8's removed field).

---

## 3. Non-goals

- **Not** workflow rerouting/restoration. This service emits events; the **Workflow Service** performs the reroute/restore (DEL-4).
- **Not** OOO presentation state. `user_availability` is owned by **User Profile**; this service only calls it (DEL-6). It is also the dashboard's source for a user's current delegate pointer.
- **Not** the delegate-impact *decision* on user removal. Whether a removal strands active workflows is answered synchronously by **Core's** `MembershipService.RemoveUser` against the **Workflow Service** (O&M §8.8); this service only ends rows afterward, asynchronously (§11.5).
- **Not** an I-8 participant. `GET /internal/users/:id/memberships` stays in **Core** and, after this extraction, does **not** reference delegations at all (§6.1). This service never serves I-8.

---

## 4. Document Overview

This LLD sits below ADR-0008 v2 and is the fourth sibling to the three ADR-0007 LLDs. It follows the tender-ACL LLD's section template. Tender-ACL is the closest precedent — it lost a composite membership FK on its split (its §7.6) and solved it with a grant-time membership-existence check against Core; this service has that problem **doubled** (§7.6) but, thanks to Option C, avoids tender-ACL's hot-path concerns entirely because its data leaves the hot path rather than being projected onto it.

Where the O&M LLD (`org_membership_lld_5.md` rev 1.71) and the shipped `iam-org-membership` code disagreed, v1 flagged the gaps for sign-off; **v2 resolves each as a decision** (§19/§23), and since the code is in development it aligns to this contract.

### 4.1 Relationship to the HLD / ADR-0008

| Source | What it specifies | Where this LLD refines it |
|---|---|---|
| `IAM HLD v1.41` | Platform SLOs, DB topology, shared libs, the delegation concept (§5.6/§7.3) | §7 (schema), §6.1 (I-8 simplification), §13 (security) |
| `ADR-0008` v2 | Option C, the doubled-FK replacement, tenant-policy relocation, dedicated topic, dev-stage migration | §6.1, §7, §7.6, §10, §21 |
| `org_membership_lld_5.md` | Authoritative pre-split delegation surface (DDL, DEL-1…14, P-18/19/20/32/33, §8.6/8.7/8.7.1/8.8, events, §17 errors) | Carried across; re-examined per invariant (§7.5) |
| `iam-lld-tender-acl-service.md` | Section template + single-composite-FK-loss precedent | Doubled (§7.6) |
| `iam-lld-authz-enrichment.md` | That `active_delegations[]` was informational passthrough, consumed by no policy | The basis for removing it from I-8 (§6.1) |

---

## 5. Service Responsibilities and Boundaries

### 5.1 In scope

- `delegations` system of record + `delegation_tenant_settings` policy.
- Lifecycle + policy API (DLG-1…7), cron entry points (DLG-I1/I2), and the two internal reads (DLG-I3/DLG-I4).
- Availability-first coordination (DEL-6) on every create and end path.
- Event production on `iam.delegation.events`; consumption of Core's removal/offboarding signals.
- Two reconcilers and the soft-delete purge job.

### 5.2 Out of scope (owned elsewhere)

| Concern | Owner | Why not here |
|---|---|---|
| Workflow reroute/restore | Workflow Service | This service emits intent; Workflow acts (DEL-4). |
| OOO presentation + delegate pointer display | User Profile | The dashboard reads `user_availability.delegate_id` from User Profile (§6.1). |
| I-8 membership projection | Core | Does not reference delegations after this cut (§6.1). |
| Delegate-impact removal gate (`409`, P-26) | Core | Membership-lifecycle; stays in Core (§11.5). |
| Membership existence / activeness | Core (`tenant_memberships`) | Checked at grant time (§7.6.2). |

### 5.3 The producer-and-coordinator distinction (design note)

Unlike the three ADR-0007 services (config/overlay stores), this service is a **lifecycle coordinator and event producer**: it orchestrates a two-phase availability-first write, publishes three event types on its own topic, and runs two reconcilers. This is why it stays with the IAM platform team — its coupling to membership lifecycle (the removal cascade, §11.5) is intrinsic. Option C means, however, that it has **no coupling at all to the I-8 hot path** — the property that made delegation the hard extraction is gone.

---

## 6. Architecture and Package Layout

**Stack:** Go 1.23, Gin (`platform-gincommon`), pgx v5 over `platform-pgcommon` (RLS/GUC bridge), `platform-events` (outbox publisher + SQS consumer), AWS SNS/SQS, ElastiCache Valkey (`del:` keyspace), RDS PostgreSQL `delegation`. Clean Architecture / ports-and-adapters, identical layout and shared-lib rules to the existing IAM services (HLD §15.3/§15.4).

**History:** lifted from `iam-org-membership`'s `internal/core/{domain,port,service}/delegation*.go`, `delegation_handler.go`, `delegation_repository.go`, and `cmd/reconciler/jobs/delegation_{expiry,review}.go`. Domain/port/service move near-verbatim; adapters are rehosted; the tenant-policy read becomes a local table rather than a `tenants` column.

> As built (verified against the repo — see `ARCHITECTURE.md` §Layer model and its diagram, `docs/architecture/mermaid/layer-model.mmd`, which this tree is kept in sync with):

```
iam-delegation/
├── cmd/
│   ├── server/                              -- HTTP composition root (DLG-1..7, DLG-I1..I4)
│   │                                            + delegation-cascade-q SQS consumer, one process
│   │   └── main.go
│   └── reconciler/
│       ├── main.go                          -- --job=<name> dispatch (this chart's own convention)
│       └── jobs/
│           ├── delegation_expiry.go         -- DLG-I1 (*/5 * * * *, §11.3)
│           ├── delegation_review.go         -- DLG-I2 (0 * * * *, dual 7d/3d warn, §11.4)
│           └── delegation_cleanup.go        -- soft-delete purge (0 4 1 * *, §11.6/§18.4)
├── internal/
│   ├── core/
│   │   ├── domain/
│   │   │   ├── delegation.go                -- Delegation, enums, EndReason (incl. review_expired)
│   │   │   ├── tenant_settings.go           -- DelegationTenantSettings (policy)
│   │   │   ├── event.go / event_payloads.go -- outbound event envelope + payload shapes
│   │   │   └── errors.go                    -- delegation_* sentinel error taxonomy (§20)
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
│   │       └── cascade_service.go           -- delegation-cascade-q business logic (§11.5/§11.6)
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

**Dependency rule:** `internal/core/*` imports no adapter/framework; adapters depend inward; `cmd/*` wires concretes. CI-enforced (§6.2).

### 6.1 The I-8 change (Option C) — what this service does NOT do

This service does **not** feed I-8. Under Option C, Core's I-8 SQL drops the `LEFT JOIN delegations` and its response drops `active_delegations[]`; Core drops the `delegations` table. The would-be consumers of that field are re-sourced:

- **Workflow** reroutes/restores from this service's `DelegationStarted`/`DelegationEnded` events (§10) — never reads I-8.
- **Dashboard** shows a user's current delegate from **User Profile** `user_availability.delegate_id`, and "Delegated To Me" from the **Workflow Service** (OOO-delegation workflow §Stage 4).
- **AuthZ Enrichment** drops `active_delegations[]` from `ae:ctx` and header injection — it consumed the field for no decision.
- **Any future synchronous need** for "delegations a user has handed out" calls this service's `GET /internal/users/:id/active-delegations` (DLG-I4), which returns the identical six-field shape I-8 used to embed. Nothing calls it today; it exists so no one keeps delegations in I-8 "just in case." If a genuine hot-path need ever emerges, v1's Core-side projection is the documented fallback (§22).

### 6.2 Dependency rules (enforced in CI)

- `internal/core` imports no adapter, Gin, pgx, or AWS SDK.
- No session-scoped `SET app.tenant_id` — only `SET LOCAL` via `pgcommon.GUCSetFromContext` (CI greps the forbidden form; RLS-6).
- Every outbound client behind a port interface (swappable/mockable).
- Every event publish goes through the outbox (atomic with the DB write).

### 6.3 Shared-library integration

`platform-gincommon` (ErrorResponse, requestctx, internal-route guard), `platform-pgcommon` (RLS/GUC bridge, reused; `delegation_app` no `BYPASSRLS`, `delegation_migrator` yes), `platform-events` (transactional outbox to `iam-delegation-events` **and** the `delegation-cascade-q` SQS consumer). This service is **both producer and consumer**, so it wires the full surface, and it additionally uses Valkey for the idempotency-key store (DLG-Q3).

---

## 7. Data Model

Two tenant-scoped tables in a new RDS PostgreSQL logical database `delegation` (shared Multi-AZ instance, PgBouncer). RLS via the `app.tenant_id` GUC; **no cross-database foreign keys** (the three FKs the split loses are replaced per §7.6).

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

### 7.1 Extensions and enums

```sql
CREATE TYPE delegation_scope  AS ENUM ('all', 'department', 'tender');
CREATE TYPE delegation_status AS ENUM ('active', 'ended', 'cancelled');
```

`EndReason` is **event-payload-only** (never a column), and — per DLG-Q5 — its domain is now `expired | cancelled | delegate_removed | review_expired`. Adopting `review_expired` (the shipped code's value) as contract makes review-driven auto-ends distinguishable from `ends_at` expiry in Audit and Notification, which is strictly more useful than overloading `expired`.

### 7.2 Tables

#### 7.2.1 `delegations`

From O&M §4.2, with the three cross-database FKs removed and `review_notice_sent_at` replaced by `review_last_warned_bucket` (DLG-Q6):

```sql
CREATE TABLE delegations (
  id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id               uuid NOT NULL,
  delegator_id            uuid NOT NULL,
  delegate_id             uuid NOT NULL,
  delegator_membership_id uuid NOT NULL,   -- composite-FK anchor (delegator); FK dropped on split (§7.6.1)
  delegate_membership_id  uuid NOT NULL,   -- composite-FK anchor (delegate);  FK dropped on split (§7.6.1)
  scope          delegation_scope NOT NULL DEFAULT 'all',
  scope_id       uuid,
  reason         text,
  starts_at      timestamptz NOT NULL DEFAULT now(),
  ends_at        timestamptz,                     -- NULL = open-ended (DEL-8)
  review_due_at            timestamptz,           -- open-ended only (DEL-13)
  review_last_warned_bucket int CHECK (review_last_warned_bucket IN (7, 3)),  -- which review notice fired (DLG-Q6); NULL = none this cycle
  review_window_days       int CHECK (review_window_days IS NULL OR review_window_days BETWEEN 1 AND 180),
  status         delegation_status NOT NULL DEFAULT 'active',
  record_version bigint NOT NULL DEFAULT 1 CHECK (record_version > 0),
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now(),
  deleted_at     timestamptz,
  CONSTRAINT chk_scope_id      CHECK ((scope = 'all' AND scope_id IS NULL)
                                      OR (scope IN ('department','tender') AND scope_id IS NOT NULL)),
  CONSTRAINT chk_no_self_delegate  CHECK (delegator_id <> delegate_id),
  CONSTRAINT chk_ends_after_starts CHECK (ends_at IS NULL OR ends_at > starts_at)
);

CREATE INDEX idx_delegations_tenant       ON delegations (tenant_id)               WHERE deleted_at IS NULL AND status = 'active';
CREATE INDEX idx_delegations_delegator    ON delegations (tenant_id, delegator_id) WHERE deleted_at IS NULL AND status = 'active';
CREATE INDEX idx_delegations_delegate     ON delegations (tenant_id, delegate_id)  WHERE deleted_at IS NULL AND status = 'active';
CREATE INDEX idx_delegations_ends_at      ON delegations (ends_at)                 WHERE deleted_at IS NULL AND status = 'active' AND ends_at IS NOT NULL;
CREATE INDEX idx_delegations_review_due   ON delegations (review_due_at)           WHERE ends_at IS NULL AND status = 'active';
```

The three intra-row CHECKs survive the split unchanged. The two former FK-support indexes (`idx_delegations_*_mem`) are dropped with the FKs. `idx_delegations_delegate` now also serves DLG-I3 (Core's dept-delegate lookup) and DLG-I4.

#### 7.2.2 `delegation_tenant_settings` (relocated from Core, DLG-Q2)

```sql
CREATE TABLE delegation_tenant_settings (
  tenant_id          uuid PRIMARY KEY,
  max_duration_days  int NOT NULL DEFAULT 90 CHECK (max_duration_days  BETWEEN 1 AND 180),
  review_window_days int NOT NULL DEFAULT 90 CHECK (review_window_days BETWEEN 1 AND 180),
  record_version     bigint NOT NULL DEFAULT 1 CHECK (record_version > 0),
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now()
);
```

Replaces the `tenants.delegation_max_duration_days` / `delegation_review_window_days` columns Core drops. Read **in-process** at create/extend (DEL-14); a missing row means the 90/90 defaults (lazily created on first DLG-7 write). RLS-scoped like `delegations`.

#### 7.2.3 `processed_events`

Idempotency ledger for the inbound cascade consumer (`consumer ∈ {cascade, offboarding}`). Exempt from RLS (operational). Monthly-pruned.

#### 7.2.4 Data ownership summary

| Table | System of record | Read path | Write path | Note |
|---|---|---|---|---|
| `delegations` | Delegation Service | DLG-1; DLG-I3/I4 (internal) | DLG-2/3/4/5, crons, cascade | **Core drops its copy** — symmetric with the ADR-0007 extractions |
| `delegation_tenant_settings` | Delegation Service | in-process (create/extend), DLG-6 | DLG-7 | relocated from Core's `tenants` (DLG-Q2) |
| `processed_events` | Delegation Service | consumer dedup | consumer | operational |

### 7.3 Row-Level Security

Identical mechanism to Core (O&M RLS-1…RLS-6), reused via `platform-pgcommon`, applied to **both** tenant tables (`delegations`, `delegation_tenant_settings`; `processed_events` is exempt — operational). The design is three SQL helper functions + a per-table policy + a violation-audit table + a read-only role, all fail-closed.

**Enable, force, default-deny (per tenant table):**

```sql
ALTER TABLE delegations ENABLE ROW LEVEL SECURITY;
ALTER TABLE delegations FORCE ROW LEVEL SECURITY;   -- applies even to the table owner
REVOKE ALL ON delegations FROM PUBLIC;              -- default-deny
GRANT SELECT, INSERT, UPDATE, DELETE ON delegations TO delegation_app;
-- delegation_tenant_settings identical
```

**The GUC + fail-closed helpers** (mirroring O&M `app_tenant_id()` / `rls_check_tenant()` / `log_rls_violation()`):

```sql
-- Reads the transaction-local GUC; NULLIF makes an empty-string GUC fail closed (zero rows),
-- not raise a cast error (RLS-2/RLS-6; the pooled-connection hardening §17.5 Case 5 exercises).
CREATE FUNCTION app_tenant_id() RETURNS uuid LANGUAGE sql STABLE AS $$
  SELECT NULLIF(current_setting('app.tenant_id', true), '')::uuid
$$;

-- SECURITY DEFINER: logs an attempted cross-tenant access to rls_violation_log, then returns
-- the boolean the policy uses. Called from USING so a mismatch is both denied and recorded.
CREATE FUNCTION rls_check_tenant(row_tenant uuid, tbl text) RETURNS boolean
  LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE current_tenant uuid := app_tenant_id();
BEGIN
  IF current_tenant IS NULL THEN RETURN false; END IF;          -- fail closed (missing/empty GUC)
  IF row_tenant <> current_tenant THEN
    INSERT INTO rls_violation_log (attempted_tenant, row_tenant, table_name, at)
      VALUES (current_tenant, row_tenant, tbl, now());
    RETURN false;
  END IF;
  RETURN true;
END $$;

CREATE TABLE rls_violation_log (            -- audit of denied cross-tenant attempts
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  attempted_tenant uuid, row_tenant uuid, table_name text, at timestamptz NOT NULL DEFAULT now()
);
```

**Per-table policy** (read denies via the logging check; write additionally `WITH CHECK`s the inserted/updated `tenant_id` against the GUC so a row can never be written into another tenant):

```sql
CREATE POLICY delegations_rls ON delegations FOR ALL
  USING      (rls_check_tenant(tenant_id, 'delegations'))
  WITH CHECK (tenant_id = app_tenant_id());
CREATE POLICY delegation_tenant_settings_rls ON delegation_tenant_settings FOR ALL
  USING      (rls_check_tenant(tenant_id, 'delegation_tenant_settings'))
  WITH CHECK (tenant_id = app_tenant_id());
```

**Roles.** `delegation_app` (the runtime role) holds no `BYPASSRLS` (RLS-4); `delegation_migrator` holds `BYPASSRLS` for migrations only; an optional `admin_readonly` role gets `SELECT` for support tooling and is still RLS-bound. **GUC binding** is transaction-local (`SET LOCAL app.tenant_id`, RLS-6) on every pool checkout via `pgcommon.GUCSetFromContext` — from `requestctx` for handlers, and from an `iam-system` + row-tenant binding for the two crons and the cascade consumer.

**Invariants (carried from O&M, verified in CI):**

| # | Invariant |
|---|---|
| RLS-1 | Both tenant tables run `ENABLE` + `FORCE ROW LEVEL SECURITY`, `REVOKE ALL … FROM PUBLIC`, and an `*_rls` policy; CI asserts `rowsecurity = true AND forcerls = true` for each. |
| RLS-2 | A missing/empty/malformed `app.tenant_id` yields **zero rows** on read and **no** writes — fail-closed by construction (`NULLIF` + `IS NULL` guard). |
| RLS-4 | `delegation_app` does **not** hold `BYPASSRLS` (CI-verified); only `delegation_migrator` does. |
| RLS-6 | The GUC is bound `SET LOCAL` (transaction-local), never session-scoped; CI greps for a forbidden non-`LOCAL` `SET app.tenant_id`. **The reconcilers and the cascade consumer MUST bind it before any `UPDATE`**, or the `WITH CHECK` silently affects zero rows — the split-brain trap the shipped cron code guards against (DEL-6). |

Validated by the canonical §17.5 matrix (Cases 1–5).

### 7.4 Migrations

`golang-migrate`, transaction-wrapped (no `CREATE INDEX CONCURRENTLY`). `000001` creates enums, both tables, indexes, RLS, and the `record_version` trigger. Migrator role:

```sql
ALTER ROLE delegation_migrator BYPASSRLS;   -- delegation_app never holds BYPASSRLS (RLS-4)
```

### 7.5 Triggers and the DEL-1…DEL-14 re-examination

**`updated_at` + `record_version` trigger** (moves verbatim from O&M §4.5; TRG-1…TRG-3). The trigger — not application code — owns both `updated_at` and `record_version`; the `WHEN` guard means a no-op `UPDATE` does not churn the version:

```sql
CREATE FUNCTION touch_row() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  NEW.updated_at     := now();
  NEW.record_version := OLD.record_version + 1;
  RETURN NEW;
END $$;

CREATE TRIGGER trg_touch_delegations BEFORE UPDATE ON delegations
  FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*) EXECUTE FUNCTION touch_row();
CREATE TRIGGER trg_touch_delegation_tenant_settings BEFORE UPDATE ON delegation_tenant_settings
  FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*) EXECUTE FUNCTION touch_row();
```

**Trigger invariants:** TRG-1 — application code never sets `record_version` in a `SET` clause (it reads it in the `WHERE` for optimistic locking, §12.1); TRG-2 — both `updated_at` and `record_version` are DB-managed, not client-controlled; TRG-3 — a no-op update (`OLD.* IS NOT DISTINCT FROM NEW.*`) does not fire, so the version does not advance.

**Per-invariant effect of the split on the O&M data invariants (v2):**

| Invariant | Substance | Effect |
|---|---|---|
| **DEL-1** | Both parties active members of the same tenant at create; no self-delegation | **DB-mechanism changed** — existence + same-tenant half becomes two grant-time Core checks (§7.6.2); `active` half stays service-layer; `chk_no_self_delegate` stays. |
| **DEL-2** | `scope`↔`scope_id` bound | Unchanged (`chk_scope_id`). |
| **DEL-3** | `active→ended/cancelled` terminal | Unchanged. |
| **DEL-4** | Only active rows route; routing owned by Workflow | Unchanged (events on the new topic). |
| **DEL-5** | Prospective effect | Unchanged. |
| **DEL-6** | Availability-first; pointer-clear-only | **Owner changed** — this service holds `UserProfileClient`; mechanics unchanged. |
| **DEL-7** | Every end path emits `DelegationEnded{reason}`; delegator-side removal silent | **Owner changed** — cascade moves here (§11.5); asymmetry preserved (DLG-EVT-4). |
| **DEL-8** | `ends_at IS NULL OR > starts_at`; NULL = open-ended | Unchanged. |
| **DEL-9** | Both sides DB-anchored to same-tenant memberships | **DB-mechanism changed** — the composite FKs → §7.6.2 checks; `*_membership_id` stay `NOT NULL`. |
| **DEL-10** | `reason` optional, ≤500, audit-only | Unchanged. |
| DEL-11/12 | do not exist | n/a |
| **DEL-13** | Open-ended review window | Unchanged mechanism; **now genuinely dual-warns (7 d + 3 d)** via `review_last_warned_bucket` (DLG-Q6). |
| **DEL-14** | Bounds by tenant `[1,180]`; `starts_at ≤ now()+1yr` | **Read-source changed** — caps read **in-process** from `delegation_tenant_settings` (DLG-Q2), no cross-service call; flat start bounds unchanged. |

### 7.6 Loss of the composite FKs — explicit treatment (the doubled §7.6)

Two composite membership FKs (one per party) plus the tenant cascade FK are unenforceable across the split.

#### 7.6.1 What is lost (the two membership FKs)

```sql
-- Source (Core, pre-extraction):
CONSTRAINT fk_del_delegator_membership FOREIGN KEY (delegator_membership_id, tenant_id, delegator_id)
                                         REFERENCES tenant_memberships(id, tenant_id, user_id),
CONSTRAINT fk_del_delegate_membership  FOREIGN KEY (delegate_membership_id,  tenant_id, delegate_id)
                                         REFERENCES tenant_memberships(id, tenant_id, user_id);
```

Each guaranteed the party's membership existed, belonged to this tenant, and matched the id/user pairing (DEL-1's existence + same-tenant half, DEL-9).

#### 7.6.2 What replaces them (two synchronous grant-time checks)

Reusing the endpoint ADR-0007 introduced for tender-ACL, returning the `tenant_membership_id` needed to populate the two `NOT NULL` columns:

```
POST /api/v1/delegations   (DLG-2, and the create leg of DLG-5)
  1. Delegation to Core: GET /internal/tenants/:id/members/:delegator_id/exists
     Delegation to Core: GET /internal/tenants/:id/members/:delegate_id/exists
     each returns { "active": true, "tenant_membership_id": "<uuid>" } | { "active": false }
     (read of tenant_memberships, status='active' AND deleted_at IS NULL — TM-9; never a 404)
  2. either active:false → 422 invalid_delegate
  3. both active:true    → store both tenant_membership_id values in the NOT NULL columns (no local FK)
```

The shipped `Create` already looked up both memberships locally and rejected an inactive delegator (the B-17 fix); those two local lookups become the two Core calls, issued concurrently, on the write path only.

**Call path:** Delegation to Core, two internal `GET`s, DLG-2/DLG-5 only. **Latency:** ≤50 ms p99 each. **Availability:** Core down means `503 org_membership_unavailable`, no write (DLG-FAIL-1); existing delegations, DLG-1, crons unaffected; **I-8 never calls this and no longer references delegations at all**. **Behind a swappable `MembershipCheckClient`** (§6).

#### 7.6.3 Provider-side contract (`tenant_membership_id` required)

The two `*_membership_id` columns are `NOT NULL`, so Core's `GET /internal/tenants/:id/members/:user_id/exists` **must** return `tenant_membership_id` when `active` (tender-ACL's TAC-D11 requirement, needed here twice per create). Decision **DLG-D3**.

#### 7.6.4 Ordering

Local pre-flight → both membership checks → User Profile availability → `RunInTx{ INSERT; outbox }`. No cross-service HTTP inside a transaction (WFI-7).

#### 7.6.5 Why the FK loss (and Option C) does not weaken authorization

The FKs were a grant-time safety net, never a routing/authorization input. Under Option C this is even clearer: **nothing reads `delegations` for an authz decision** — I-8 no longer touches it. A stale row for a departed party is inert (the departed user gets `404` from I-8 and no headers). Losing the FK at grant time (mitigated by §7.6.2) and ending rows asynchronously both cost nothing at authorization time.

#### 7.6.6 The third FK loss — `fk_del_tenant` cascade

Replaced by the async tenant-offboarding consumer (§11.6), the same pattern the sibling services use. Not a live gap (§7.6.5). Decision **DLG-D4**.

---

## 8. API Contract

### 8.1 Conventions

Public routes `/api/v1/delegations*`, Envoy-fronted (`jwt_authn` + `ext_authz`), gateway-injected `x-user-id`/`x-tenant-id`/`x-tenant-roles`. Internal routes `/internal/*` mesh-only (mTLS). Lifecycle shapes are byte-compatible with old P-18/19/20/32/33. Errors via `gincommon.ErrorResponse` (§20).

### 8.2 Authorization rules per route

| Route | Rule |
|---|---|
| DLG-1 `GET /delegations` | self (RLS-scoped) |
| DLG-2 `POST /delegations` | self (delegator = `x-user-id`); requires `Idempotency-Key` |
| DLG-3 `DELETE /delegations/:id` | self or `tenant_admin`/`tenant_owner` |
| DLG-4 `POST /delegations/:id/extend` | self or `tenant_admin`/`tenant_owner` |
| DLG-5 `POST /delegations/:id/reassign` | self or `tenant_admin`/`tenant_owner` |
| DLG-6 `GET /delegations/settings` | any tenant member |
| DLG-7 `PUT /delegations/settings` | `tenant_admin`/`tenant_owner` |
| DLG-I1/I2/I3/I4 `/internal/*` | `iam-system`, mesh-only |

### 8.3 Endpoint catalogue

| ID | Old ID | Method & path | Auth | Notes |
|---|---|---|---|---|
| DLG-1 | P-18 | `GET /api/v1/delegations` | self | List active delegations (RLS-scoped) |
| DLG-2 | P-19 | `POST /api/v1/delegations` | self | Create; availability-first; two membership checks; `Idempotency-Key` required (DLG-Q3) |
| DLG-3 | P-20 | `DELETE /api/v1/delegations/:id` | self/admin | Cancel; pointer-clear UP first |
| DLG-4 | P-32 | `POST /api/v1/delegations/:id/extend` | self/admin | Push `review_due_at`; open-ended only |
| DLG-5 | P-33 | `POST /api/v1/delegations/:id/reassign` | self/admin | End + create; fuller body (DLG-Q7) |
| DLG-6 | from P-1 | `GET /api/v1/delegations/settings` | member | Tenant delegation policy (relocated from Core, DLG-Q2) |
| DLG-7 | from P-2 | `PUT /api/v1/delegations/settings` | admin | Set policy `{max_duration_days, review_window_days}` in `[1,180]` |
| DLG-I1 | (cron) | `POST /internal/delegations/expire` | iam-system | `delegation-expiry` |
| DLG-I2 | (cron) | `POST /internal/delegations/review-sweep` | iam-system | `delegation-review` (dual 7d/3d) |
| DLG-I3 | new | `GET /internal/delegations/dept-delegate?tenant_id=&user_id=&dept_id=` | iam-system | Core's §8.8.4 removal precision (WFI-11) |
| DLG-I4 | new | `GET /internal/users/:id/active-delegations` | iam-system | Escape hatch replacing I-8's removed field (§6.1); unused today |

### 8.4 Key endpoint specifications

#### DLG-2 — `POST /api/v1/delegations` (old P-19)

```jsonc
// POST /api/v1/delegations
// x-user-id: <delegator_id>  x-tenant-id: <tenant_id>  Idempotency-Key: <opaque>
{
  "delegate_id": "9ac3...",
  "scope": "department",
  "scope_id": "engr-uuid",
  "starts_at": "2026-07-01T00:00:00Z",
  "ends_at": "2026-07-14T23:59:59Z",
  "ooo_note": "On annual leave — contact Carol for Engineering queries."
}
// 201 Created
{
  "delegation_id": "del-uuid",
  "delegator_id": "2b1f...",
  "delegate_id": "9ac3...",
  "scope": "department",
  "scope_id": "engr-uuid",
  "starts_at": "2026-07-01T00:00:00Z",
  "ends_at": "2026-07-14T23:59:59Z",
  "status": "active",
  "record_version": 1
}
```

Ordering (§7.6.4): pre-flight → two membership checks → UP availability (on `200`) → `RunInTx{ INSERT; outbox DelegationStarted }`. Bounds from **local** `delegation_tenant_settings` (DEL-14): span ≤ `max_duration_days` (`422 delegation_window_too_long`); `starts_at` within `(now()−skew, now()+1yr]`; `ends_at > starts_at`. `ooo_note` rides the UP call (not stored); `reason` is stored and forwarded to UP as the display note. **Idempotency (DLG-Q3):** a repeated `Idempotency-Key` within 24 h returns the original `201` without creating a second delegation.

#### DLG-4 — extend (old P-32)

```jsonc
// POST /api/v1/delegations/:id/extend
{ "extend_days": 90 }   // optional; default = tenant review_window_days (or row override); must be [1,180]
// 200 OK
{ "delegation_id": "del-uuid", "review_due_at": "2026-11-08T00:00:00Z", "review_last_warned_bucket": null }
```

Open-ended only (`422 not_review_tracked` if fixed `ends_at`); `422 extend_days_out_of_range` outside `[1,180]`; computed from current `review_due_at`; resets `review_last_warned_bucket` to NULL (re-arms both warnings). No UP call.

#### DLG-5 — reassign (old P-33; fuller body, DLG-Q7)

```jsonc
// POST /api/v1/delegations/:id/reassign
{ "new_delegate_id": "7cd1...", "scope": "department", "scope_id": "engr-uuid", "ends_at": null, "reason": "..." }
// 201 Created — NEW row; old delegation ended, not mutated
{ "delegation_id": "del-uuid-2", "delegator_id": "2b1f...", "delegate_id": "7cd1...", "status": "active" }
```

Ends the current delegation (`DelegationEnded{cancelled}`) then creates a new one (full DLG-2 pre-flight incl. both membership checks). All body fields optional; each defaults to the current delegation's value. On UP failure for the new delegation, reassign aborts after the old ended (old not resurrected).

#### DLG-7 — set tenant policy (relocated from Core P-2, DLG-Q2)

```jsonc
// PUT /api/v1/delegations/settings   (tenant_admin/owner)
{ "max_duration_days": 60, "review_window_days": 45 }
// 200 OK  { "max_duration_days": 60, "review_window_days": 45, "record_version": 2 }
```

Both `[1,180]` (`400 invalid_delegation_max_duration_days` / `invalid_delegation_review_window_days`). Upserts the tenant's `delegation_tenant_settings` row.

#### DLG-I4 — `GET /internal/users/:id/active-delegations` (escape hatch)

```jsonc
// 200 OK — identical shape to I-8's removed active_delegations[]
{ "active_delegations": [
    { "delegation_id":"del-uuid","delegator_id":"2b1f...","delegate_id":"9ac3...","scope":"all","scope_id":null,"ends_at":"2026-07-10T00:00:00Z" }
] }
```

Returns the delegations the user has handed out (delegator side, active). Provided for any future synchronous consumer; **no component calls it today** (§6.1).

### 8.5 Status codes

200 (DLG-1/3/4/6/7, DLG-I3/I4) · 201 (DLG-2, DLG-5) · 400 (malformed / `invalid_delegation_scope` / invalid policy days) · 403 (role) · 404 (`delegation_not_found`) · 409 (`optimistic_lock_conflict`) · 422 (validation, §20) · 503 (`org_membership_unavailable`, `user_profile_unavailable`).

---

## 9. Caching Design

Valkey `del:` keyspace. **No cross-service config cache** (tenant policy is now a local table, §7.2.2) and **no I-8 projection** (Option C). Two small caches:

| Key | Value | TTL | Invalidated by |
|---|---|---|---|
| `del:list:{tenant}:{delegator}` | DLG-1 list projection (optional) | 60 s | DLG-2/3/4/5 for that delegator |
| `del:idem:{tenant}:{key}` | `{delegation_id, status}` for a create idempotency key (DLG-Q3) | 24 h | TTL only |

### 9.1 Read algorithm (DLG-1)

```
key := "del:list:" + tenant + ":" + delegator
if v := valkey.GET(key); hit { return deserialize(v) }
rows := SELECT ... FROM delegations WHERE tenant_id=$1 AND delegator_id=$2 AND deleted_at IS NULL ORDER BY starts_at DESC
valkey.SET(key, serialize(rows), 60s)
return rows
```

### 9.2 Idempotency store (DLG-Q3)

On DLG-2, `SETNX del:idem:{tenant}:{key}` guards the create; a hit returns the stored `delegation_id`'s `201` without a second insert. Written after the transaction commits; 24 h TTL. Valkey unavailability degrades to best-effort (create proceeds; duplicate risk reverts to the pre-idempotency behaviour, logged).

### 9.3 Failure mode

Valkey down means DLG-1 falls through to Postgres (advisory cache); idempotency degrades to best-effort. Correctness never depends on the cache. **Nothing here is on a hot path** — Option C removed the only hot-path relationship delegation ever had.

---

## 10. Event Architecture

This service is **both a producer and a consumer** — the first of the four O&M extractions to be a producer. It publishes three events on its own dedicated topic `iam.delegation.events` (DLG-Q1) and consumes Core's removal/offboarding signals. Every published event is written to the transactional outbox in the same transaction as the state change that caused it (DLG-EVT-1); publishing is at-least-once (outbox → SNS → SQS fan-out), and every consumer is idempotent via `processed_events`.

### 10.1 Inbound — SQS consumer

The service has **one** active inbound subscription: Core's user-removal / tenant-offboarding signals, which drive the delegate-side cascade (§11.5, DLG-Q4).

| Source topic | Event type(s) | SQS queue | DLQ | `maxReceiveCount` | Filter policy | Dispatched to |
|---|---|---|---|---|---|---|
| `iam.membership.events` | `MembershipRevoked` | `delegation-cascade-q` | `delegation-cascade-q-dlq` | 5 | `EventType IN [MembershipRevoked]` | `CascadeService.EndForUser` (§11.5) |
| `iam.tenant.events` | `TenantOffboarded` | `delegation-cascade-q` | `delegation-cascade-q-dlq` | 5 | `EventType IN [TenantOffboarded]` | `CascadeService.ScrubTenant` (§11.6) |

The queue takes the `lifecycle`-adjacent naming used by the sibling services (`delegation-cascade-q`, not the plain `<topic>-<consumer>-q`, mirroring `iam-group-mapping`'s and User Profile's `tenant-lifecycle-*-q`). Idempotency is via `processed_events` keyed on the envelope `id` (§7.2.3); a redelivery is a no-op. A message whose payload fails schema-decode is routed to the DLQ rather than retried indefinitely.

### 10.2 Serialization format

Event payloads are serialized as **JSON** (UTF-8) — the format registered in the AWS Glue Schema Registry (§10.3.1), referenced in `api/asyncapi.yaml` (§10.3), and carried on the wire in SNS/SQS. A future migration to Protobuf would follow the platform-wide gRPC adoption (out of scope here).

### 10.3 AsyncAPI contract

The JSON envelope carries `id` (UUID v7, consumer dedup key), `type` (PascalCase event name, e.g. `DelegationStarted`), `source` (`iam-delegation`), `specversion` (`"1.0"`), `time` (RFC 3339), `data` (the payload), `tenant_id`, `trace_id`, plus optional `subject`, `actor` (who caused it — audit), `dataschema` (Glue schema-version UUID), and `ip_address`/`user_agent` (system sentinels on cron-origin events). SNS `MessageAttributes`: `EventType` (PascalCase, used by consumer filter policies), `TenantID`, `Source`, `EventID`, `Subject`.

Full `api/asyncapi.yaml` (hand-authored; the CI gate `schema-gov validate --asyncapi api/asyncapi.yaml` runs the pinned `platform-schemagov` image and fails the PR on any drift between this file and the Glue registry):

> Current file: [`api/asyncapi.yaml`](../../api/asyncapi.yaml)

The payload field shapes are byte-identical to the O&M §7.4 definitions except `DelegationEnded.ended_reason` gains `review_expired` (DLG-Q5) and the topic/`source` change. `api/asyncapi.yaml` must be updated in the same PR as any Glue registry version bump — the CI gate enforces consistency.

#### 10.3.1 AWS Glue Schema Registry and `platform-schemagov`

The three subjects register under a new Glue registry `iam-delegation-events` (Core's registry drops them — they no longer originate there):

| Glue registry | Glue schema name | Glue data format | Version |
|---|---|---|---|
| `iam-delegation-events` | `DelegationStarted` | JSON | 1 |
| `iam-delegation-events` | `DelegationEnded` | JSON | 1 |
| `iam-delegation-events` | `DelegationReviewRequested` | JSON | 1 |

Schema-evolution rules (identical discipline to User Profile §7.3.1):

- **Additive** (new optional field, or a new enum value like `review_expired`) → register a new Glue schema version, bump `specversion`/`dataschema`, and update `api/asyncapi.yaml` in the same PR. Backward-compatible; consumers dispatch by event `type`.
- **Breaking** (remove/rename a field, change a type) → new Glue schema name + new AsyncAPI message + major version bump.

Encoding is split between enqueue time (plain JSON into the outbox, validated against the registered schema) and publish time (the Glue codec resolves and stamps the schema-version UUID into `dataschema`), exactly as `platform-events` wires it:

```go
// Enqueue time (eventbus/publisher.go) — plain JSON, no codec, inside the state-change tx:
return pgcommon.RunInTx(ctx, pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
    created, err := repo.Insert(ctx, tx, d)              // the delegation row
    if err != nil { return err }
    payload, _ := json.Marshal(delegationStartedPayload) // self-contained snapshot
    if err := validator.Validate(ctx, "DelegationStarted", payload); err != nil { return err }
    env := events.NewEnvelope("DelegationStarted", "iam-delegation", json.RawMessage(payload),
        events.WithTenantID(rc.TenantID), events.WithTraceID(rc.TraceID), events.WithActor(rc.UserID))
    return outbox.Enqueue(ctx, tx, env)                  // atomic with the INSERT (DLG-EVT-1)
})

// Publish time (cmd/server/main.go) — Glue encoding happens here, not above:
publisher := events.NewSNSPublisher(snsConfig, events.WithCodec(glueCodec)) // registry "iam-delegation-events"
```

AWS Glue Schema Registry is free in all regions including ap-south-1.

### 10.4 Outbound — `iam.delegation.events` via the outbox

A single SNS topic, `events.NewSNSPublisher` (no RoutingPublisher — one topic, three types). Every state change writes to `outbox_events` in the same transaction; the `outbox.Runner` polls at 500 ms (`BatchSize 50`, `MaxAttempts 5`, DLQ on exhaustion). Fan-out to consumer queues (each with a DLQ, `maxReceiveCount=5`):

| Consumer | SQS queue | DLQ | Events consumed | Purpose |
|---|---|---|---|---|
| Workflow Service | `delegation-workflow-q` | `delegation-workflow-q-dlq` | `DelegationStarted`, `DelegationEnded` | Reroute pending activities on start; restore original assignees on end (DEL-4/DEL-5) |
| Notification | `delegation-notification-q` | `delegation-notification-q-dlq` | `DelegationStarted`, `DelegationEnded`, `DelegationReviewRequested` | User-facing notices; review warnings fan out to delegator + delegate + tenant_admins/owners (delegate/owner notify-only) |
| Audit Log | `delegation-audit-q` | `delegation-audit-q-dlq` | all three (no filter) | Immutable audit of every delegation lifecycle transition |

### 10.5 Published events (HLD §9.4)

| Event Type | Emitted when | Payload (key fields) | Consumers |
|---|---|---|---|
| `DelegationStarted` | DLG-2 create, and the create leg of DLG-5 reassign | `delegation_id, tenant_id, delegator_id, delegate_id, scope, scope_id, starts_at, ends_at, actor_id` | **Workflow Service** (reroute), Notification, Audit |
| `DelegationEnded` | DLG-3 cancel, `ends_at` expiry (DLG-I1), review auto-end (DLG-I2), the end leg of DLG-5, and the delegate-removed cascade (§11.5) | `delegation_id, tenant_id, delegator_id, delegate_id, ended_reason, actor_id` — `ended_reason ∈ {expired, cancelled, delegate_removed, review_expired}` | **Workflow Service** (restore), Notification, Audit |
| `DelegationReviewRequested` | `delegation-review` sweep at the 7-day and 3-day marks (DLG-I2) | `delegation_id, tenant_id, delegator_id, delegate_id, days_remaining` — `days_remaining ∈ {7,3}` | Notification, Audit |

Notes: (1) cron-origin events (`DelegationEnded` from expiry/review, `DelegationReviewRequested`) carry system sentinels — `ip_address:"system"`, `user_agent:"iam-delegation/delegation-expiry-cron"` or `".../delegation-review-cron"`, `actor: SystemActorID`. (2) `DelegationStarted`/`DelegationEnded` are the *authoritative* routing signals the Workflow Service acts on — distinct from User Profile's presentation-only `UserAvailabilityChanged` (O&M §2.3). (3) The delegator-side removal end is intentionally silent (no event) — the DEL-7 asymmetry (DLG-EVT-4).

### 10.6 Idempotency and ordering

Publishing is at-least-once (outbox + SNS); every consumer is idempotent via `processed_events` keyed on the envelope `id` (UUID v7). Events for one delegation are **not** strictly ordered across types, so payloads are **self-contained snapshots** (the full current value of the changed fields, not deltas) — a consumer applying them out of order converges to the correct latest state on the next event. A `DelegationEnded` for a `delegation_id` the consumer never saw a `DelegationStarted` for is still safely actionable (Workflow simply has nothing to restore). Consumers that reject a Glue-decode failure route the message to their DLQ rather than retrying indefinitely.

### 10.7 Event invariants

| # | Invariant |
|---|---|
| DLG-EVT-1 | Every event is published through the transactional outbox, atomic with the DB write that caused it — a committed lifecycle change and its event are all-or-nothing. |
| DLG-EVT-2 | Event `type` values and payload `data` shapes are byte-identical to O&M §7.4 (plus the `review_expired` enum value and the topic/`source` change); consumers dispatch by `type` on `iam.delegation.events`. |
| DLG-EVT-3 | `DelegationEnded` fires on **every** end path — cancel (`cancelled`), `ends_at` expiry (`expired`), review auto-end (`review_expired`), delegate-removed cascade (`delegate_removed`) — never path-dependent (DEL-7). |
| DLG-EVT-4 | Delegator-side removal ends the row **silently** (no event), preserving the O&M DEL-7 asymmetry as a deliberate carry-over. |
| DLG-EVT-5 | The `delegation-review` sweep emits `DelegationReviewRequested` at most once per bucket (7 then 3) per cycle, tracked by `review_last_warned_bucket` (DLG-Q6); extend/reassign reset it to NULL, re-arming both. |
| DLG-EVT-6 | Audit consumes all three types on `delegation-audit-q` (no filter); every event and its DB effect are traceable via the shared `trace_id` (§14.3); cron events carry system sentinels. |
| DLG-EVT-7 | Payloads are self-contained snapshots; consumers are idempotent (`processed_events`) and order-insensitive, converging on the latest state (§10.6). |

---

## 11. Key Request Flows

One sequence diagram per meaningful path, numbered to the endpoint/flow it realises (mirroring the User Profile LLD's §8.8 per-endpoint convention). §11.0 is the shared request preamble every public route runs first; §11.1–§11.6 are the lifecycle/cron/cascade flows; §11.7 covers the remaining read/policy endpoints compactly.

### 11.0 Request preamble (shared by every public route)

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

### 11.1 Create / coordination (DLG-2)

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

### 11.2 Cancel (DLG-3) — pointer-clear, fail-open

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

### 11.3 Expiry cron (DLG-I1) — availability-first, self-retrying

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

### 11.4 Review-window cron (DLG-I2) — dual 7 d/3 d warn, then auto-end (DLG-Q6)

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

Fixed-`ends_at` delegations are never considered (the sweep selects `ends_at IS NULL`). The `review_last_warned_bucket` marker makes the 7 d notice fire once and the 3 d notice fire once; extend/reassign reset it to NULL, re-arming both.

### 11.5 User removal — synchronous gate in Core, async row-end here

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

The stranding hazard is resolved synchronously in Core before removal (Workflow gate + P-26). Under Option C, Core has no `delegations` table, so the §8.8.4 dept-scope precision (WFI-11) is a synchronous Core to Delegation call (DLG-I3) on the admin removal path — not a hot path; on a Delegation outage the gate degrades to tenant-wide impact (still correct, less precise). Row-ending is asynchronous and safe (rows inert once the user has no membership, §7.6.5).

### 11.6 Tenant-lifecycle cleanup

On `TenantOffboarded`, the cascade consumer soft-deletes the tenant's `delegations` (and `delegation_tenant_settings`); the monthly `delegation-cleanup` hard-purges rows soft-deleted > 90 days. No live gap (§7.6.5).

### 11.7 Read and policy endpoints (DLG-1 list, DLG-4 extend, DLG-5 reassign, DLG-6/7 settings)

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

---

## 12. Concurrency, Consistency, and Failure Handling

### 12.1 Optimistic concurrency

`record_version` is trigger-managed (TRG-1…3). End/extend/reassign issue `UPDATE ... WHERE id=$1 AND record_version=$2`, mapping zero rows to `409` (with the current version in `details`) or `404` (terminal/absent) via the shipped probe. The cascade `SoftDeleteForUser` is deliberately not optimistic-locked (terminal bulk cascade).

### 12.2 Idempotency

- **Create (DLG-2):** an `Idempotency-Key` (DLG-Q3) makes retries safe; a repeated key returns the original `201`. This also resolves the O&M gap that there is no `UNIQUE(tenant_id, delegator_id)` (a user may legitimately hold multiple concurrent delegations of different scopes).
- **Crons:** idempotent by predicate (`status='active' AND ...`); a mid-batch crash re-selects the still-active row; UP pointer-clear is safe to repeat.
- **Cascade consumer:** idempotent via `processed_events`.

### 12.3 Failure scenarios

| Scenario | Effect on this service | Effect on platform |
|---|---|---|
| Core membership-check down (DLG-2) | `503`, no write (DLG-FAIL-1) | New grants blocked; **I-8 unaffected (and no longer references delegations)** |
| User Profile down — create | `503`, no write | Create blocked, retryable |
| UP down — expiry/review/cascade | defer + retry (DEL-6) | Pointer re-cleared next tick; no split-brain |
| UP down — cancel | fail-open (proceed) | Interactive cancel not blocked |
| **This service down** | DLG-1…7 unavailable; crons paused | **I-8 unaffected**; the removal gate degrades to tenant-wide impact (still correct) |
| Valkey down | DLG-1 to Postgres; idempotency best-effort | Slightly higher latency; correctness intact |

### 12.4 Consistency guarantees

- DB write ↔ event: atomic via outbox (DLG-EVT-1).
- Delegation ↔ availability: availability-first + pointer-clear-only means no split-brain (DEL-6).
- Removal gate ↔ row-end: gate strongly consistent (Core, pre-removal); row-end eventually consistent on inert rows (§11.5).
- **No delegation ↔ I-8 consistency concern exists** — Option C removed the relationship.

### 12.5 Operational invariants

| # | Invariant |
|---|---|
| DLG-FAIL-1 | A Core or UP outage during DLG-2 fails the create cleanly with no partial write. |
| DLG-FAIL-2 | A Delegation Service outage never affects I-8 (I-8 no longer references delegations) and never produces a wrong authorization answer. |
| DLG-FAIL-3 | This service has no involvement in any hot path — every synchronous call it makes is on its own write path; the only coupling toward Core (the removal cascade) is asynchronous, and the dept-scope lookup is on the admin removal path. |
| DLG-FAIL-4 | Every scheduled end path is availability-first and self-retrying (DEL-6); `iam_delegation_*_deferred_total` alarms on sustained deferral. |

---

## 13. Security

### 13.1 Tenant isolation — three layers

RLS (`FORCE`, fail-closed `NULLIF` policy on both tables); `SET LOCAL app.tenant_id` per transaction via `platform-pgcommon` (RLS-6); gateway identity (`x-*` headers), never request-body-derived tenant/actor.

### 13.2 Network isolation

Public behind Envoy; `/internal/*` (DLG-I1…I4) mesh-only mTLS via the internal-route guard. Cross-service calls (Core, User Profile) intra-mesh mTLS.

### 13.3 Input validation

Mirrors the DB CHECKs and DEL invariants: scope in enum; scope↔scope_id; `reason ≤ 500`; `starts_at` within `(now()−skew, now()+1yr]`; `ends_at > starts_at`; span ≤ tenant `max_duration_days`; `extend_days` in `[1,180]`; policy days in `[1,180]`.

### 13.4 Authorization rules

| Decision | Input | Source | New cross-service call? |
|---|---|---|---|
| List (DLG-1) | RLS tenant scope | GUC | No |
| Create (DLG-2) | self = delegator | headers | Membership check ×2 (write path) |
| Cancel/extend/reassign | self or admin | `x-tenant-roles` | No (DLG-5 create leg re-checks membership) |
| Set policy (DLG-7) | admin | `x-tenant-roles` | No (local table) |
| Valid grantee | `{active}` both parties | Core (§7.6.2) | Yes — grant-time only |

`reason` and the delegation record are never an authorization input (DEL-10); this service makes no authz decision.

---

## 14. Observability

### 14.1 SLOs

**Per-endpoint p99** (availability target **99.9%** for the public API; the two crons and the cascade consumer are background and have no user-facing latency SLO, only the freshness/defer targets below):

| Endpoint / scenario | p99 target | Notes |
|---|---|---|
| DLG-1 list | 50 ms | cache hit; Postgres fallback ≤80 ms |
| DLG-2 create | 250 ms | 2 membership checks (≤50 ms each, concurrent) + 1 UP call (≤50 ms) + tx |
| DLG-3 cancel | 150 ms | 1 UP pointer-clear + tx |
| DLG-4 extend / DLG-6 settings-read / DLG-7 settings-write | 120 ms | local tables, no cross-service call |
| DLG-5 reassign | 300 ms | end + create (two flows chained) |
| DLG-I3 dept-delegate (Core removal path) | 50 ms | single indexed lookup, `idx_delegations_delegate` |
| DLG-I4 active-delegations | 50 ms | single indexed lookup; unused today |
| **I-8 (Core, for reference)** | **≤15 ms hit / ≤30 ms miss, improved** | **Faster after this extraction — one fewer join, one fewer sub-object; this service has no part in it (§6.1, DLG-FAIL-2)** |

**Latency source (cache vs. DB) and background freshness:**

| Path | p99 (cache hit) | p99 (miss / cold) | Notes |
|---|---|---|---|
| DLG-1 list | 50 ms (`del:list`) | 80 ms (Postgres) | advisory cache; correctness never depends on it |
| Tenant policy read (DLG-2/4/6) | n/a (local table) | ≤10 ms | in-process `delegation_tenant_settings`; no cross-service read |
| Event propagation (create → Workflow reroute) | — | sub-second to low-seconds | outbox 500 ms poll + SNS/SQS; routing rides the event, not a read |
| Expiry/review auto-end freshness | — | ≤ one cron tick (5 min expiry / 1 h review) + retry on UP-defer | tracked by `iam_delegation_*_deferred_total` |

### 14.2 Metrics

`iam_delegation_created_total{scope}`, `iam_delegation_ended_total{ended_reason}` (now incl. `review_expired`), `iam_delegation_active_gauge{tenant}`, `iam_delegation_expiry_deferred_total`, `iam_delegation_review_deferred_total`, `iam_delegation_review_warned_total{days_remaining}`, `iam_delegation_review_expired_total`, `iam_delegation_membership_check_duration_seconds`, `iam_delegation_membership_check_failures_total`, `iam_delegation_up_availability_failures_total{path}`, `iam_delegation_idempotency_hits_total`, `iam_delegation_cascade_processed_total`, `iam_delegation_cascade_dlq_total`, plus passthrough `http_*`/`events_*`/`sqs_*`. **No projection-lag metric** (Option C removed the projection).

### 14.3 Tracing and logging

W3C `traceparent` propagated on every outbound call; the outbox stamps `trace_id` so create → event → Workflow reroute is one trace. Logs carry `delegation_id`, `tenant_id`, `actor`, DEL-6 defer reasons.

### 14.4 Dashboards and 14.5 Alerts

Grafana "IAM — Delegation": active gauge, create/end by reason, expiry/review defer, membership-check + UP latency/error, cascade throughput/DLQ, idempotency hits. Alerts: `expiry_deferred > 0` sustained 30 m (warning, UP dependency); `cascade_dlq > 0` (warning); membership-check failure rate > 5% for 5 m (warning). **No I-8/projection alert** — this service no longer touches the hot path.

---

## 15. Configuration

```yaml
# values-prod.yaml (excerpt)
delegation:
  database: { logicalName: delegation, appRole: delegation_app }
  userProfile:   { baseUrl: http://iam-user-profile.iam.svc.cluster.local,   timeoutMs: 3000 }
  orgMembership: { baseUrl: http://iam-org-membership.iam.svc.cluster.local, membershipCheckTimeoutMs: 3000 }
  cache:
    keyspace: "del:"
    listTtlSeconds: 60
    idempotencyTtlSeconds: 86400
  policyDefaults: { maxDurationDays: 90, reviewWindowDays: 90 }   # used when a tenant has no delegation_tenant_settings row
  review: { warnEarlyDays: 7, warnLateDays: 3 }                   # dual warning (DLG-Q6)
  events: { topic: iam-delegation-events, source: iam-delegation } # dedicated topic (DLG-Q1)
  crons:
    expirySchedule: "*/5 * * * *"
    reviewSchedule: "0 * * * *"
    cleanupSchedule: "0 4 1 * *"
    batchLimit: 50
```

The O&M `DELEGATION_REVIEW_WINDOW_DAYS` env var is gone entirely — the window is per-tenant in `delegation_tenant_settings`, defaulting to `policyDefaults` only when a tenant has no row. No cross-service config URL or TTL exists any more (DLG-Q2). Secrets via the platform store; base URLs required (fail-fast on empty for the data-bearing clients).

---

## 16. Deployment and Scaling

**16.1 Topology.** EKS `iam`; RDS `delegation` (Multi-AZ, PgBouncer); Valkey `del:`; one Envoy upstream for `/api/v1/delegations*`. A `server` Deployment (2–4 replicas, HPA) serves DLG-1…7 + DLG-I3/I4 + the cascade consumer; a `reconciler` runs the CronJobs.

**16.2 Repo/layout.** `github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation`, Clean Architecture, independent chart/pipeline.

**16.3 Scaling.** HPA on CPU + RPS (writes admin/user-driven, low); HPA on `delegation-cascade-q` depth; raise `batchLimit`/shorten schedule if a cron saturates.

**16.4 Environments.** `golang-migrate` pre-deploy Job (transaction-wrapped). Graceful shutdown drains HTTP and lets the SQS consumer finish its message (at-least-once + `processed_events`).

---

## 17. Testing Strategy

**17.1 Unit.** DEL-1…DEL-14 branches; availability-first ordering; pointer-clear-only; optimistic-lock 404-vs-409; extend range/open-ended; reassign end-then-create; idempotency replay; dual-warning bucket transitions (7 then 3, reset on extend).

**17.2 Integration.** Postgres testcontainer with RLS: repository CRUD, cron sweeps (`idx_delegations_ends_at`, `idx_delegations_review_due`), `review_last_warned_bucket` firing both notices once, soft-delete cascade, `record_version` trigger, `delegation_tenant_settings` upsert/default, `processed_events` dedup. Port the shipped `p19_delegation_coverage_test.go` / `tenant_delegation_happy_test.go`.

**17.3 Contract.** Membership-check mock: two calls, both `tenant_membership_id`s stored, `{active:false}` gives `422`, response must carry `tenant_membership_id` (DLG-D3). **Core-side regression:** assert I-8's response **no longer contains** `active_delegations[]` and its SQL joins four tables (the Option-C removal). DLG-I4 returns the identical six-field shape I-8 used to embed. AsyncAPI: validate the three payloads incl. `review_expired`.

**17.4 E2E.** Create → Workflow reroute event → cancel → restore event; expiry ends past-`ends_at` + re-clears availability; review warns at 7 d then 3 d then auto-ends `review_expired`; user removal → Core gate `409` → P-26 → removal → async cascade ends rows + emits `delegate_removed`.

### 17.5 RLS test cases (canonical)

```sql
-- Case 1: Missing GUC → 0 rows
RESET app.tenant_id;
SELECT count(*) FROM delegations;                          -- expect: 0

-- Case 2: Cross-tenant write rejected
SET LOCAL app.tenant_id = 'aaaaaaaa-...';
INSERT INTO delegations (tenant_id, delegator_id, delegate_id, delegator_membership_id, delegate_membership_id, scope)
VALUES ('bbbbbbbb-...', gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'all');  -- expect: ERROR

-- Case 3: Cross-tenant read isolation
SET LOCAL app.tenant_id = 'aaaaaaaa-...';
SELECT count(*) FROM delegations WHERE tenant_id = 'bbbbbbbb-...';   -- expect: 0

-- Case 4: Cross-tenant soft-delete rejected
SET LOCAL app.tenant_id = 'aaaaaaaa-...';
UPDATE delegations SET deleted_at = now() WHERE tenant_id = 'bbbbbbbb-...';  -- expect: 0 rows affected
```

**Case 5 — no GUC leak across a pooled connection** (`TestRLS_NoGUCLeakageAcrossPooledConnection`, RLS-6): pin to one backend (`MaxConns=1`); tenant A inserts under `SET LOCAL`; tenant B reads and sees only B's rows; tenant B on the same backend with no GUC set reads 0 (fail-closed via `NULLIF`), not A's rows; B cannot update A's row by id. Applies to `delegation_tenant_settings` too, and to the crons/consumer (a missing binding after a UP pointer-clear silently no-ops the row-end). CI greps for the forbidden non-`LOCAL` `SET app.tenant_id`.

---

## 18. GDPR, Data Lifecycle, and Compliance

**18.1 PII.** `delegations` references three user UUIDs (`delegator_id`, `delegate_id`, two `*_membership_id`) and an optional free-text `reason`. No names/emails/credentials.

**18.2 Offboarding and erasure.** `TenantOffboarded` gives cascade soft-delete then monthly hard-purge (§11.6). Per-user erasure (Core/User Profile signal on the same queue) ends the user's rows and scrubs `reason` on hard-delete. Completeness asserted by a reconciliation report.

**18.3 Residency.** Single-region RDS/ElastiCache (HLD §7.1).

**18.4 Retention.** Active until ended/cancelled/expired, then soft-deleted 90 days, then purge; `processed_events` 30 days; delegation events per Audit Log policy.

---

## 19. Open Questions and Sign-off Register

All v1 open questions are **resolved as decisions** for this development-stage build (detail in §23). Retained here for traceability; none blocks implementation.

- **DLG-Q1 — Event topic. RESOLVED:** dedicated `iam.delegation.events` (§10.1).
- **DLG-Q2 — Tenant delegation policy. RESOLVED:** moved into this service as `delegation_tenant_settings` (§7.2.2); Core's `tenants` drops the two columns.
- **DLG-Q3 — Create idempotency. RESOLVED:** mandatory `Idempotency-Key` on DLG-2, 24 h dedup (§9.2).
- **DLG-Q4 — Core removal signal. RESOLVED:** Core emits `MembershipRevoked{tenant_id, user_id, actor_id}` + `TenantOffboarded`; consumed on `delegation-cascade-q` (§10.2). *(Requires the Core team to add the `MembershipRevoked` emission — a development task, tracked with Core.)*
- **DLG-Q5 — `review_expired` end-reason. RESOLVED:** added to the enum, `[expired, cancelled, delegate_removed, review_expired]` (§7.1/§10).
- **DLG-Q6 — Review cadence. RESOLVED:** dual 7 d + 3 d warnings via `review_last_warned_bucket`; `days_remaining` in `{7,3}` (§7.2.1/§11.4).
- **DLG-Q7 — Reassign surface. RESOLVED:** fuller body `{new_delegate_id?, scope?, scope_id?, ends_at?, reason?}` (§8.4).
- **DLG-Q8 — Error taxonomy. RESOLVED:** §20 — keep `scope_id_required`/`not_review_tracked`; add `invalid_scope_id`, `reason_too_long`; drop `delegation_not_open_ended`.
- **DLG-Q9 — Drop the Core projection (Option C). RESOLVED:** yes — `active_delegations[]` removed from I-8, Core drops the `delegations` table (§6.1, ADR-0008 §13.1).
- **DLG-Q10 — Future-dated activation. RESOLVED:** v1 scope is active-at-create (matches the OOO workflow); future-dating is an explicit v2 feature needing a start-scheduler (§22).

The one item still requiring **cross-team coordination** (not a design open question) is DLG-Q4's Core-side `MembershipRevoked` emission, since it is a change in the Core codebase this service depends on.

---

## 20. Appendix — Error Taxonomy

`gincommon.ErrorResponse`: `{ "error": { "code", "message", "request_id", "trace_id", "details" } }`. Canonical, reconciled per DLG-Q8.

| `code` | HTTP | Meaning |
|---|---|---|
| `invalid_delegation_scope` | 400 | `scope` not in `{all, department, tender}` |
| `invalid_delegation_max_duration_days` | 400 | DLG-7 `max_duration_days` outside `[1,180]` |
| `invalid_delegation_review_window_days` | 400 | DLG-7 `review_window_days` outside `[1,180]` |
| `scope_id_required` | 422 | `scope_id` missing for `department`/`tender` |
| `invalid_scope_id` | 422 | `scope_id` present when `scope='all'` |
| `self_delegation` | 422 | `delegator_id == delegate_id` |
| `invalid_delegate` | 422 | Delegate (or delegator) not an active member (§7.6.2, or a UP 4xx race) |
| `delegate_unavailable` | 422 | Delegate is themselves OOO (from User Profile) |
| `reason_too_long` | 422 | `reason` exceeds 500 chars |
| `delegation_window_inverted` | 422 | `ends_at <= starts_at` |
| `delegation_window_too_long` | 422 | Fixed span exceeds tenant `max_duration_days` (DEL-14) |
| `delegation_start_in_past` | 422 | `starts_at` before `now()` (5 s skew) |
| `delegation_start_too_far_future` | 422 | `starts_at` more than 1 year ahead (DEL-14) |
| `not_review_tracked` | 422 | DLG-4 extend on a fixed-`ends_at` delegation |
| `extend_days_out_of_range` | 422 | DLG-4 `extend_days` outside `[1,180]` |
| `delegation_not_found` | 404 | Not resolvable in caller's tenant (or terminal, DEL-3) |
| `optimistic_lock_conflict` | 409 | `record_version` mismatch; `details.record_version` carries the current value |
| `org_membership_unavailable` | 503 | §7.6.2 membership check 5xx/timeout; no write; retryable |
| `user_profile_unavailable` | 503 | DEL-6 availability call 5xx/timeout on create; no write; retryable |

The removal-gate errors (`409 workflow_resolution_required`, `503 workflow_service_unavailable`) remain **Core's** (P-8/I-5/P-26).

---

## 21. Migration Plan

**The IAM services are in development and not deployed — there is no live data or traffic to preserve.** So this is a coordinated set of development-branch changes, not the expand/contract dance the ADR-0007 waves needed. (Any throwaway dev data in `delegations` is a one-time export to seed the new DB; `delegation_tenant_settings` seeds from the soon-dropped `tenants` columns or the 90/90 defaults.)

1. **Stand up the Delegation Service** — `delegation` DB (`delegations` + `delegation_tenant_settings`), DLG-1…7, DLG-I1…I4, crons, the `iam.delegation.events` topic, the `delegation-cascade-q` consumer.
2. **Repoint consumers** — Workflow / Notification / Audit subscribe to `iam.delegation.events`.
3. **Subtract from Core** — remove the `LEFT JOIN delegations` and `active_delegations[]` from I-8; drop the `delegations` table, the P-18/19/20/32/33 handlers and crons, the delegation use of `port.UserProfileClient`, the §15.2.2 in-transaction row-end, and the two `tenants` delegation-policy columns; add the `MembershipRevoked` emission and the `GET /internal/delegations/dept-delegate` client call on the removal path.
4. **Adjust AuthZ Enrichment** — drop `active_delegations[]` from `ae:ctx` and header injection.

**Ordering constraint:** step 3's I-8 field removal and step 4 land together (or step 4 first) so AuthZ never expects a field I-8 stops returning. Everything else is independent.

**Rollback:** trivial while in development — revert the coordinated branch. No production snapshot/replay posture, because there is no production.

---

## 22. Future Options

- **Option-A projection (documented fallback).** If a genuine *synchronous, hot-path* need for a user's outbound delegations ever emerges, reintroduce v1's Core-side event-synced `delegations` projection so I-8 can serve the field again — but only then, and only for the specific field a consumer proves it needs on the hot path. DLG-I4 (`/internal/users/:id/active-delegations`) is the non-hot-path stand-in until such a need is demonstrated.
- **Own event schema evolution.** With a dedicated topic, the delegation event schemas can evolve independently of the membership topic.
- **Future-dated / chained delegation (DLG-Q10).** Holding a created delegation inactive until a later `starts_at` (needs a start-scheduler), and transitive re-delegation when a delegate is themselves OOO, are explicit v2 features.
- **Real FK restoration.** If a future consolidation co-locates delegation with Core, the swappable `MembershipCheckClient` can be deleted and the composite FKs restored.

---

## 23. Decision Register

| # | Decision |
|---|---|
| DLG-D1 | **Option C for I-8** — `active_delegations[]` is removed from I-8 and Core drops the `delegations` table entirely; I-8's join shrinks to four tables and gets faster (§6.1, ADR-0008 §13.1/§14). Chosen once the no-contract-change constraint was lifted (dev-stage). v1's projection (Option A) is retained only as a documented fallback (§22). Read-through (Option B) rejected. |
| DLG-D2 | **Tenant delegation policy moves into this service** as `delegation_tenant_settings`, read in-process at create/extend; Core drops the two `tenants` columns (§7.2.2, DLG-Q2). Eliminates any cross-service config read. |
| DLG-D3 | **The two lost composite membership FKs → two synchronous grant-time checks** against Core's `GET /internal/tenants/:id/members/:user_id/exists`, which must return `tenant_membership_id` when active (populating both `NOT NULL` columns), behind a swappable `MembershipCheckClient` (§7.6.2/§7.6.3). |
| DLG-D4 | **The lost `fk_del_tenant` cascade → an async tenant-offboarding consumer** (§11.6). |
| DLG-D5 | **Delegation events get a dedicated topic `iam.delegation.events`** (`source: iam-delegation`); Core's AsyncAPI drops them (§10, DLG-Q1). |
| DLG-D6 | **`DelegationEnded.ended_reason` gains `review_expired`** (DLG-Q5) — the shipped code's value becomes contract; review auto-ends are distinguishable from `ends_at` expiry. |
| DLG-D7 | **The review sweep warns twice (7 d and 3 d)** via `review_last_warned_bucket`; `days_remaining` in `{7,3}` (§11.4, DLG-Q6). |
| DLG-D8 | **Create takes a mandatory `Idempotency-Key`** (24 h dedup), resolving the duplicate-on-retry gap left by the absence of a `UNIQUE(tenant_id, delegator_id)` (§9.2, DLG-Q3). |
| DLG-D9 | **The synchronous removal gate stays in Core; only the row-end moves here (async).** Core's §8.8.4 dept-scope precision becomes a Core to Delegation call (DLG-I3) on the admin path (§11.5). |
| DLG-D10 | **`port.UserProfileClient` + DEL-6 availability-first / pointer-clear-only move here intact**; cancel is fail-open, scheduled ends defer-and-retry (§11.2/§11.3). |
| DLG-D11 | **Reassign takes the fuller body** (`new_delegate_id?/scope?/scope_id?/ends_at?/reason?`) and the error taxonomy is canonicalised (§8.4/§20, DLG-Q7/Q8). |
| DLG-D12 | **v1 scope is active-at-create** (no future-dating); future-dated activation is a v2 feature needing a start-scheduler (§22, DLG-Q10). |

*End of document. This v2 resolves all ten open questions as decisions for the development-stage build; the only remaining cross-team task is Core adding the `MembershipRevoked` emission this service's cascade consumes (DLG-Q4).*
