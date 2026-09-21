# Contributing

This is an internal IAM microservice for the XpertPMS platform. This guide covers development setup, how to extend the service, test requirements, and the PR process.

## Prerequisites

- Go 1.26.6 (pinned exactly — matches `go.mod`'s `go 1.26.6`; DLG-D16 — intentionally newer than the LLD's stated "Go 1.23", which is stale relative to the platform's real toolchain)
- Docker (required for the Postgres/Valkey/Floci-backed integration and RLS tests via `testcontainers-go`)
- `GOPRIVATE=github.com/BCBP-SOLUTIONS-FZC-LLC/*` (private module access)

## Development setup

```bash
git clone https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation
cd iam-delegation
make setup      # copies .env.example → .env
make tidy       # go mod tidy
make docker-up  # start Postgres + Valkey + Floci
make lint       # verify linter passes
make test       # unit + integration + rls, in parallel (requires Docker)
make run        # start cmd/server on :8080
```

To run only unit tests (no Docker required):

```bash
make test-unit
```

`make setup` also installs `.githooks/pre-commit` (tidy + fmt-check + lint + Swagger staleness) into `.git/hooks/`. Re-run `make install-hooks` any time `.githooks/pre-commit` changes.

## Project layout

```
cmd/
  server/               ← Composition root — HTTP API + delegation-cascade-q SQS consumer, one process
  reconciler/
    jobs/                ← delegation_expiry.go / delegation_review.go / delegation_cleanup.go / delegation_activation.go — the four CronJob entry points
internal/
  core/domain/           ← Entities, value objects, event payloads, domain errors (no external deps)
  core/port/             ← Interfaces: DelegationRepository, SettingsRepository, UserProfileClient, MembershipCheckClient, IdempotencyStore, EventPublisher, Cache, TxRunner
  core/service/          ← DelegationService (DLG-1..5) · SettingsService (DLG-6/7) · CascadeService (cascade-consumer logic)
  adapter/inbound/
    http/                ← Gin handlers, router, middleware
    consumer/            ← delegation-cascade-q SQS consumer
  adapter/outbound/
    postgres/            ← Repository implementations + golang-migrate migrations
    userprofile/         ← UserProfileClient HTTP impl (DEL-6)
    orgmembership/       ← MembershipCheckClient HTTP impl (DLG-D3)
    eventbus/            ← SchemaValidator (jsonschema/v6) + GlueCodec/NoopCodec
    valkey/               ← del: cache + idempotency store
    metrics/              ← Prometheus instruments
internal/eventschema/    ← Hand-maintained JSON Schemas for the four published events
pkg/requestctx/          ← Gateway-identity / tenant-actor extraction helpers
api/                     ← asyncapi.yaml (hand-maintained) + embed.go
docs/                    ← lld/, architecture/ (mermaid diagrams), runbook-schema-registry.md, swagger/
```

Tests are colocated with the code they cover (`*_test.go` next to the source file), not under a separate `test/` tree — see Testing requirements below. For the full layout (including `deploy/helm/`, `.github/`) see [ARCHITECTURE.md](ARCHITECTURE.md).

**Dependency rule:** `internal/core/*` imports no adapter, Gin, pgx, or AWS SDK; adapters depend inward; `cmd/*` wires concretes. Enforced in CI by `make arch-lint` (`.go-arch-lint.yml`).

## Extending the service

### Adding a new domain entity or value object

1. Add the type to `internal/core/domain/` — no external imports allowed.
2. Add unit tests alongside it (`internal/core/domain/*_test.go`).

### Adding a new use case (service method)

1. Add the method signature to the relevant interface in `internal/core/port/` if it introduces a new repository or outbound capability.
2. Implement the method in `internal/core/service/` (`DelegationService`, `SettingsService`, or `CascadeService`).
3. Add the handler in `internal/adapter/inbound/http/` — wire the route in `router.go`.
4. Add unit tests next to the service method (`internal/core/service/*_test.go`) and, if it introduces a new query pattern against a tenant-scoped table, an RLS case (`-tags=rls`).

### Adding a new HTTP endpoint

1. Add the Swaggo annotation comment above the handler function (see existing handlers for the pattern).
2. Register the route in `internal/adapter/inbound/http/router.go`.
3. Run `make swag` and commit the updated `docs/swagger/` output.
4. Update the API overview table in `README.md`.

### Adding a new repository migration

1. This repo currently ships a single migration pair, `000001_schema.{up,down}.sql`, under `internal/adapter/outbound/postgres/migrations/` — the next one is `000002_description.{up,down}.sql`. Numbering is monotonic; never reuse or re-order. **Exception while this service remains undeployed:** a schema fix gets folded back into `000001` rather than layered as a new migration (there's no live data or applied history to preserve yet). That exception ends the moment this service is deployed anywhere — after that, every schema change is a new forward migration, full stop.
2. The embedded FS is recompiled on next build — no code changes needed.
3. Verify: `make docker-up && make run` (the server self-migrates at startup, LLD §7.4/§16.4).
4. Add or update RLS policy tests (`-tags=rls`) if the migration touches `delegations` or `delegation_tenant_settings`.
5. **New indexes on tenant tables should use `CREATE INDEX CONCURRENTLY`** for rolling-deploy safety on large tables — this repo's existing single migration predates having a table large enough for this to matter, so there's no in-repo precedent yet, but it's the same convention the sibling IAM services follow. `CONCURRENTLY` requires the statement to run outside a transaction (don't wrap it in `BEGIN`/`COMMIT`) and cannot be used inside `CREATE TABLE`.

### Adding a new domain event type

This repo's event contract is entirely hand-maintained — unlike `iam-user-profile`, there is **no** `make extract-schemas` step deriving `internal/eventschema/*.json` from `api/asyncapi.yaml`; both are authored and kept in sync by hand (DLG-D20).

1. Add the payload struct and event-type constant to `internal/core/domain/event.go` — the constant value IS the Glue schema name (PascalCase, no translation table).
2. Add the JSON schema file to `internal/eventschema/` using the snake_case filename convention (e.g. `delegation_transferred.json`), with `"additionalProperties": true`.
3. Add the schema to `eventschema.ByEventType` in `internal/eventschema/schemas.go` (ValidatingCodec compiles this map at startup; SchemaValidator uses the same map).
4. Extend `SCHEMA_NAME_MAP` in `.github/workflows/schema-registry.yml` (all three job blocks: `staging`, `production`, `pr-check`) and add a `register_schema` call in `scripts/init-floci.sh`.
5. Add the message to `api/asyncapi.yaml` with `x-lifecycle: {status: active}` and `x-owner` annotations.
6. Run `make schema-validate` locally, then `make schema-register` in every environment before the first pod that would publish the new event starts.
7. Update the event table in `README.md`.

The full checklist (with rationale) lives in `docs/runbook-schema-registry.md` § "Adding a new event type" — this is the condensed version.

### Adding a new environment variable

1. Add it to `.env.example` with an inline comment explaining purpose and accepted values.
2. If it's required at startup, add a check in `cmd/server/config.go`'s `loadConfig()` (see the existing `SNS_TOPIC_ARN`/`CASCADE_QUEUE_URL` fail-fast checks for the pattern).
3. Update the Environment variables table in `README.md`, and `deploy/helm/iam-delegation/values.yaml` if it needs a production default.

## Testing requirements

Tests are differentiated by Go build tags, not directory, and all run against `./...`:

| Command | Build tag | Docker | Notes |
|---------|-----------|--------|-------|
| `make test-unit` | *(none)* | No | Fully isolated; mock all dependencies |
| `make test-integration` | `integration` | Yes | Postgres/Valkey/SQS-compatible via testcontainers-go |
| `make test-rls` | `rls` | Yes | Row-Level Security policy enforcement — the canonical §17.5 matrix |
| `make test-e2e` | `e2e` | Yes | Full HTTP-stack request flows |
| `make test` | all three above | Yes | Runs unit + integration + rls in parallel (`-j3`) |
| `make race` | all four | Yes | Same suites, `-race`, in parallel (`-j4`) |
| `make test-ci` | unit + integration + rls | Yes | `-race` + per-suite coverage profiles merged into `coverage.out` — what CI runs |

Run the race detector before submitting a PR:

```bash
make race
```

### Coverage gate

CI enforces a single global statement-coverage gate of **≥ 95%** on the merged `coverage.out` (`.github/scripts/coverage-gate.sh`) — bumped from the original 70% floor (which matched `iam-tender-acl`'s baseline) during the DLG-D34 production-readiness sweep, once actual coverage was pushed to 95.2%. This is a floor, not an aspiration; ratchet it up over time, never lower it to pass a failing PR.

```bash
make cover-func   # per-function summary in terminal
make cover        # HTML report
```

Global coverage was 83.9% before DLG-D34 and 95.2% immediately after it — the largest single gain came from direct AWS Glue Schema Registry API mocking via a real `httptest.Server` (`GlueCodec`'s construction, version-cache refresh, and encode paths had zero coverage before, since `*glue.Client` is a concrete SDK type with no test seam); the rest closed handler auth-branch, cascade-consumer error/metrics, schema-validator error, and repository edge-case gaps. Per-package numbers shift as the code does — run `make cover-func` for current figures rather than trusting a table here to stay accurate.

Coverage is measured over `./internal/...` and `./pkg/...` (`COVER_PKG_LIST` in the Makefile).

## Linting

```bash
make lint
```

The lint config (`.golangci.yml`) enforces the repo's linter rule set. Add a package doc comment to every new package.

## Documentation update checklist

When your change touches a public contract or internal data flow, update the following:

| What changed | Documents to update |
|---|---|
| New HTTP endpoint | README.md API overview table · run `make swag` |
| New domain event type | `api/asyncapi.yaml` · `internal/eventschema/` · `SCHEMA_NAME_MAP` in `schema-registry.yml` · `docs/runbook-schema-registry.md` · README.md event table |
| Changed request/cron/cascade flow | `ARCHITECTURE.md` § Key request flows + the matching `docs/architecture/mermaid/*.mmd` source file · `docs/lld/iam-lld-delegation-service.md` if it's a design-level change |
| New or renamed package | `ARCHITECTURE.md` § Layer model + `docs/architecture/mermaid/layer-model.mmd` |
| New environment variable | `.env.example` (inline comment) · README.md Environment variables table · `deploy/helm/iam-delegation/values.yaml` |
| New repository migration | `CHANGELOG.md` `[Unreleased]` with a short schema-change description |

## Branch naming

Feature branches follow the same convention as this org's other IAM services:

- `feat/<short-topic>` — new capability
- `fix/<short-topic>` — bug fix
- `docs/<short-topic>` — documentation-only change
- `chore/<short-topic>` — dependencies, tooling, CI, refactors with no behavioural change

Base new branches off the latest `main`.

## Pull request checklist

Mirrors `.github/pull_request_template.md` — the PR template is the source of truth; this is a summary:

- [ ] `make fmt-check` + `make vet` + `make lint` + `make arch-lint` all pass
- [ ] `make race` passes with zero data races detected
- [ ] Unit tests added/updated (`make test-unit`); RLS tests added/updated (`make test-rls`) for any new/changed query against `delegations`/`delegation_tenant_settings`; integration tests added/updated (`make test-integration`) where applicable
- [ ] `make swag` run and `docs/swagger/` committed if any handler annotation changed
- [ ] `api/asyncapi.yaml` and `internal/eventschema/*.json` updated together if the event contract changed, with `"additionalProperties": true` preserved
- [ ] `CHANGELOG.md` `[Unreleased]` section updated
- [ ] README.md / ARCHITECTURE.md updated per the checklist above, where applicable (a new session-specific decision goes in `ARCHITECTURE.md`'s "Session-specific decisions" section)
- [ ] No secrets or DSNs hardcoded

## Commit style

Use a short imperative subject line (≤ 72 chars). Reference the area:

```
fix(cancel): return 404 instead of 500 on already-terminal delegation
feat(review-cron): add second warning bucket at 3 days
feat(asyncapi): add x-lifecycle annotations to published messages
docs(runbook): document the SCHEMA_NAME_MAP translation table
test(rls): add cross-tenant settings upsert isolation case
fix(cascade): dedupe MembershipRevoked via processed_events on redelivery
```

## Deployment

This service ships **two** binaries (`cmd/server`, `cmd/reconciler`) built into **one** image from the repo-root `Dockerfile` — not yet deployed to any live environment, and no Git tag has ever been pushed (see `VERSIONING.md` for the full release process, SemVer policy, and runtime-contract scope). Releases are handled via the CI/CD pipeline:

**On push to `main`:** `validate-test` and `validate-quality` run in parallel with `build-image`. `build-image` lints the Dockerfile (hadolint), builds a single-platform (`linux/amd64`) image, runs a Trivy CRITICAL/HIGH/UNKNOWN CVE scan (fails the build; a second permissive scan uploads SARIF to the GitHub Security tab), and runs smoke tests against both the `server` and `reconciler` entrypoints from the same built image. On success it's pushed to GHCR (`ghcr.io/bcbp-solutions-fzc-llc/iam-delegation`, tagged by branch + short SHA) with build provenance and SBOM attached, then signed with Cosign keyless signing and the signature is verified in the same job.

**On a `v*` tag (release):** builds the same single image with `provenance: mode=max` and a CycloneDX SBOM, signs it with Cosign, generates a SLSA provenance attestation, and creates a GitHub Release (environment: `production`) with the SBOM and provenance as downloadable artifacts alongside checksums. `schema-registry.yml`'s `production` job also fires on `release: published` to register the event schemas to the production Glue registry.

The Helm chart lives at `deploy/helm/iam-delegation/`, applied by the platform CD pipeline.
