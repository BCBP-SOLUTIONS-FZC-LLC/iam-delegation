# Changelog

All notable changes to this project are documented in this file. Format based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added — initial extraction from `iam-org-membership` (ADR-0008 Wave 4, Option C)

Stood up `iam-delegation` as the fourth and last O&M extraction service, per
`docs/IAM_service/iam-lld-delegation-service.md`. Lifted `internal/core/{domain,port,service}/
delegation*.go`, `delegation_handler.go`, `delegation_repository.go`, and
`cmd/reconciler/jobs/delegation_{expiry,review}.go` near-verbatim out of `iam-org-membership`;
adapters were rehosted onto this repo's own Clean Architecture / Ports-and-Adapters layout
(`internal/core/{domain,port,service}` + `internal/adapter/{inbound,outbound}`, matching
`iam-catalog-admin`/`iam-group-mapping`/`iam-tender-acl`'s convention).

- Owns two tables — `delegations`, `delegation_tenant_settings` (the latter relocated in-process
  from two `tenants` columns Core used to own, DLG-D2) — plus `processed_events` for cascade-consumer
  idempotency.
- Ships the DLG-1..7 public API (list/create/cancel/extend/reassign/get-settings/set-settings) and
  the DLG-I1..I4 mesh-only internal API (expiry sweep, review sweep, department-delegate lookup, and
  the `active_delegations[]` replacement endpoint — DLG-I4, §6.1).
- Two binaries (`cmd/server`, `cmd/reconciler`): the server runs the HTTP API and the
  `delegation-cascade-q` SQS consumer (`MembershipRevoked`/`TenantOffboarded`, §11.5/§11.6) in one
  process; the reconciler hosts the three CronJob entry points
  (`delegation-expiry`/`delegation-review`/`delegation-cleanup`).
- Publishes `DelegationStarted`/`DelegationEnded`/`DelegationReviewRequested` to a dedicated topic,
  `iam.delegation.events` (DLG-D5) — this service is both a producer and a consumer, unlike most of
  its O&M extraction siblings.
- The two lost composite membership foreign keys become two synchronous grant-time checks against
  `iam-org-membership` (DLG-D3); the lost `fk_del_tenant` cascade becomes the async
  tenant-offboarding path on `delegation-cascade-q` (DLG-D4).
- Independent Helm chart (`deploy/helm/iam-delegation`) and CI pipeline
  (`.github/workflows/{ci,validate-quality,validate-test,schema-registry,changelog-check,release}.yml`),
  matching the shared conventions across the IAM service family.

### Added — CI schema governance and documentation parity with `iam-user-profile`

Superseded DLG-D15's CI-governance scope (recorded as DLG-D20 in `IMPLEMENTATION_NOTES.md`): this
service's Go-level event validation (`eventbus.SchemaValidator`, `GlueCodec`) was already at parity
with `iam-user-profile`'s — the only real gap was the CI-time `platform-schemagov` pipeline and its
monitoring, now closed.

- `.github/workflows/schema-registry.yml` rewritten to drive `platform-schemagov` validate/diff/
  register against a dedicated `iam-delegation-events` Glue registry, replacing the earlier
  Python-only structural check; added `schema-prune.yml`, `schema-health-quarterly.yml`, and
  `freeze-watchdog.yml` to match.
- Added `deploy/monitoring/schema-registry-alerts.yml`, `docs/runbook-schema-registry.md`, the
  `schema-*` Makefile targets, `docker-compose.pro.yml`, and `GLUE_REGISTRY_NAME`/`GLUE_REGISTRY_ARN`
  in `deploy/helm/iam-delegation/values.yaml`.
- Added `x-lifecycle`/`x-owner`/`x-forward-compatibility`/`x-semantic-contract`/`x-version-governance`/
  `x-usage-override` governance annotations to `api/asyncapi.yaml`, required by `schema-gov validate`
  and previously absent.
- Fixed a real local-dev bug found along the way: `docker-compose.yml`'s `iam-delegation` service set
  `EVENTS_TOPIC`/`SQS_QUEUE_URL`/`SQS_DLQ_URL`/`CACHE_LIST_TTL_SECONDS`/`CACHE_IDEMPOTENCY_TTL_SECONDS`,
  none of which `cmd/server/config.go` actually reads — the container would have crash-looped on the
  missing required `SNS_TOPIC_ARN`/`CASCADE_QUEUE_URL`. Corrected to the real env var names and added
  the previously-missing `.env.example`.
- Brought `docs/`, `README.md`, `ARCHITECTURE.md`, and `CONTRIBUTING.md` up to the structure
  `iam-user-profile` uses: copied the authoritative LLD in as `docs/lld/iam-lld-delegation-service.md`
  (previously lived outside the repo); added `docs/architecture/` (nine Mermaid diagrams — eight
  extracted verbatim from the LLD, plus a new layer-model, package-dependency graph, and RLS/GUC-path
  diagram authored from the actual code); expanded `ARCHITECTURE.md` with the sections
  `iam-user-profile`'s has (cache strategy, observability stack, RLS/GUC injection, concurrency,
  failure domains, key invariants, consumer conformance, schema lifecycle, trust boundaries,
  developer tools); expanded `README.md` (why this service exists, input validation, CI, Docker,
  integrating with other services, out of scope, license/ownership); added `CONTRIBUTING.md` (this
  repo had none). Every fact in the new content was verified against this repo's actual source rather
  than copied from `iam-user-profile` — where this service is simpler (e.g. no rate limiting, a
  two-key cache, business metrics registered but not yet called per DLG-D19) the new docs say so
  rather than fabricating parity.

### Known gaps

- HPA on `delegation-cascade-q` queue depth (LLD §16.3) is not wired — the chart currently scales on
  CPU/memory (and, optionally, RPS) only; a queue-depth-based scaler (e.g. KEDA) is future work.
- DLG-D19 (`IMPLEMENTATION_NOTES.md`): the `iam_delegation_*` business metrics are registered but no
  service-layer call site invokes them yet — counters will report zero until that wiring lands.
