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

### Known gaps

- `cmd/server` and `cmd/reconciler` (including `cmd/reconciler/jobs/`) are being wired concurrently
  with this Helm/CI scaffolding — until that lands, `make build`/`docker-build` will not yet produce
  runnable binaries.
- HPA on `delegation-cascade-q` queue depth (LLD §16.3) is not wired — the chart currently scales on
  CPU/memory (and, optionally, RPS) only; a queue-depth-based scaler (e.g. KEDA) is future work.
