# Versioning and releases

This repository is a **deployed Go microservice**, not a library other services `go get` — it has no `pkg/` public API surface for another module to import (`pkg/requestctx` is a typed context helper used only inside this repo's own request path), and its `go.mod` module path exists so this repo's own code compiles, not for import by sibling repos (this document is modeled on [`iam-user-profile`](https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-user-profile)'s `VERSIONING.md` — see also [`platform-events`](https://github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events)'s for the library-package version of the same pattern — but differs from both for that reason). What gets versioned here is the **container image** (`ghcr.io/bcbp-solutions-fzc-llc/iam-delegation`), the **Helm chart** (`deploy/helm/iam-delegation/`), and the **runtime contract** — REST API, published/consumed event schemas, and operational surface — that every caller and downstream consumer depends on. Versions are published with **Git tags** and described in [CHANGELOG.md](./CHANGELOG.md).

## Semantic versioning (SemVer)

We use [SemVer 2.0.0](https://semver.org/): `MAJOR.MINOR.PATCH` (e.g. `v1.2.3`).

| Bump | When you change | Examples |
|------|-----------------|----------|
| **MAJOR** | Breaking change in the runtime contract (§ below) | Removing/renaming a REST field or endpoint, narrowing an enum, changing `ended_reason`'s meaning, a breaking event-schema change not shipped as a new `type` (per `api/asyncapi.yaml`'s own `x-version-governance` schema-evolution rules), removing/renaming a required env var, an incompatible `values.yaml` restructure |
| **MINOR** | New backward-compatible capability | A new optional REST field or endpoint, a new optional env var, a new optional Helm value, a new published or consumed event type, a new CronJob, a new Prometheus metric |
| **PATCH** | Backward-compatible fix | Bug fix, performance improvement, dependency bump with no observable behavior change, documentation-only correction |

### What counts as the runtime contract

This service has no Go package for another service to import — its "public API" is the wire/deployment contract every HTTP caller, event consumer, and operator depends on.

| In scope (SemVer applies) | Out of scope (may change without MAJOR) |
|---------------------------|------------------------------------------|
| The public REST endpoints in `internal/adapter/inbound/http/router.go` — DLG-1..7 (`GET/POST/DELETE /api/v1/delegations`, `POST .../:id/extend`, `POST .../:id/reassign`, `GET/PUT .../settings`) and the mesh-only DLG-I1..I4 internal endpoints (`POST /internal/delegations/{expire,review-sweep}`, `GET /internal/delegations/dept-delegate`, `GET /internal/users/:id/active-delegations`), request/response shapes documented in `docs/swagger/` | `internal/*` package structure, exported Go identifiers, file layout — nothing here is imported by another module |
| `api/asyncapi.yaml`'s four published messages (`DelegationStarted`, `DelegationEnded`, `DelegationReviewRequested`, `DelegationEscalationRequested`) and the three consumed messages (`MembershipRevoked`, `TenantMembershipsPurged` from Core; `UserUpdated` from User Profile) — event `type` values, envelope shape, `ended_reason`/`days_remaining` enums, and per-event payload schemas, per the spec's own Backward-compatible/Breaking-change rules | The Glue schema version UUID itself (`dataschema`) — it rotates on every backward-compatible change by design, not a contract break |
| Required env var **names and semantics** (`.env.example`) — notably `SNS_TOPIC_ARN`/`CASCADE_QUEUE_URL` (always required), `USER_PROFILE_BASE_URL`/`ORG_MEMBERSHIP_BASE_URL` (required, client constructors fail fast on empty), `SYSTEM_DATABASE_URL` (required when `ENVIRONMENT=production`) | Env var **defaults** — tunable without a MAJOR bump unless the new default itself breaks a documented invariant |
| `deploy/helm/iam-delegation/values.yaml` top-level key names and shapes consumers actually set (`image.*`, `env.*`, `envFromSecret`, `existingSecret`/`secretValues`, `cronJobs.*` schedules and `batchLimit`, `service.*`, replica/autoscaling/probe fields, `networkPolicy.*`) | Chart internals (`_helpers.tpl`, template structure) not exposed as a `values.yaml` key |
| Prometheus metric **names** in `internal/adapter/outbound/metrics/metrics.go` (`iam_delegation_created_total`, `iam_delegation_ended_total`, `iam_delegation_active_gauge`, `iam_delegation_expiry_deferred_total`, `iam_delegation_review_deferred_total`, `iam_delegation_activation_deferred_total`, `iam_delegation_review_warned_total`, `iam_delegation_review_expired_total`, `iam_delegation_membership_check_duration_seconds`, `iam_delegation_membership_check_failures_total`, `iam_delegation_up_availability_failures_total`, `iam_delegation_idempotency_hits_total`, `iam_delegation_cascade_processed_total`, `iam_delegation_cascade_dlq_total`) — dashboards and alert rules key off these exact names | Metric **label cardinality** beyond what's documented, histogram bucket boundaries |
| The four CronJob **names and schedules' semantics** (`delegation-activation`, `delegation-expiry`, `delegation-review`, `delegation-cleanup`) — an operator's monitoring/alerting keys off these job names | The exact cron expression, as long as the documented cadence class (5-min, hourly, monthly) is preserved |
| `GET /healthz`/`GET /readyz` — existence and meaning (liveness vs. dual-Postgres-pool dependency health) | `GET /asyncapi`/`GET /asyncapi.yaml`/`GET /swagger/*any` content rendering — documentation surfaces, not a contract this service must hold byte-stable |

### Guarantees

- **Pre-`v1.0.0` (current status — no tag has ever been pushed):** per [SemVer §4](https://semver.org/#spec-item-4), anything may change at any time while the major version is `0`. Unlike some sibling IAM services, this repo has not yet cut **any** release — `deploy/helm/iam-delegation/Chart.yaml` carries `version: 0.1.0`/`appVersion: "1.0.0"` as scaffold defaults, not a shipped release (see Supported releases below). The table above still indicates what's *more* disruptive than what within `v0.x` (a MINOR bump is still meant to signal "safer than a MAJOR bump would have been"), but a downstream REST caller, event consumer, or Helm deployment should not yet assume `v0.x` compatibility across a MINOR bump the way it could once `v1.0.0` ships.
- **MAJOR (once `v1` ships):** we avoid breaking changes to the runtime contract within `v1.x`. A breaking change ships as `v2.0.0` with migration notes in the CHANGELOG.
- **MINOR:** safe to redeploy without changing caller code or Helm values, unless you opt into a new capability.
- **PATCH:** drop-in image replacement; upgrade recommended for security fixes (every release is CVE-scanned, see below).

## Supported releases

| Version | Status | Image tag | Notes |
|---------|--------|-----------|-------|
| — | — | — | **No Git tag has been pushed yet.** `.github/workflows/release.yml` exists and is fully wired (build → docker → deploy-gate → publish), but no `vMAJOR.MINOR.PATCH` tag has triggered it. `CHANGELOG.md` has only ever had an `[Unreleased]` section. |

This service has no formal support-window policy yet, since nothing has shipped. Once `v1.0.0` (or an earlier `v0.x`) tags and deploys to a real environment, this section will define how long a superseded major line receives security-only fixes (expect the same platform convention `platform-events` uses: security fixes only, for a period the platform team sets, typically ~6 months after the next major).

`deploy/helm/iam-delegation/Chart.yaml`'s `version`/`appVersion` (`0.1.0`/`"1.0.0"`) were set once at the chart's initial commit and have never been bumped alongside a tag, because no tag has been cut — do not treat them as a release marker of any kind. See the maintainer process below for keeping them in sync once the first tag is cut.

## Consume a release

This service is **not** consumed via `go get` — do not add this module as a dependency of another Go service. It is consumed as a **container image**, deployed via the **Helm chart** in `deploy/helm/iam-delegation/`, exposing a REST API and publishing/consuming events per `api/asyncapi.yaml`.

### Pull and verify the image

```bash
docker pull ghcr.io/bcbp-solutions-fzc-llc/iam-delegation:v0.1.0
```

(`v0.1.0` above is illustrative — substitute the actual tag once one exists; see Supported releases above.)

Every tagged release is signed keylessly via Sigstore/Cosign (no long-lived key) — verify before deploying:

```bash
cosign verify \
  --certificate-identity-regexp "^https://github\.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/.github/workflows/.*@refs/tags/.*$" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/bcbp-solutions-fzc-llc/iam-delegation@<digest>
```

(Untagged `main`-branch builds use a different identity regexp — `ci.yml`'s `push` job signs with `@refs/heads/(main|master)$` — since they're signed there, not by `release.yml`.)

### Image tag scheme

`.github/workflows/release.yml`'s `docker/metadata-action` step produces, per tag push:

| Tag pattern | Produced for | Example (from `v1.2.3`) |
|-------------|--------------|--------------------------|
| `vMAJOR.MINOR.PATCH` | Every release | `v1.2.3` |
| `vMAJOR.MINOR` | Every release | `v1.2` |
| `vMAJOR` | Every release | `v1` |
| `latest` | Stable releases only (no pre-release suffix) | `latest` |

| Pin style | Use when |
|-----------|----------|
| `vMAJOR.MINOR.PATCH` | Production; exact reproducibility (recommended — also enables the digest verification the release workflow's `deploy-gate` performs) |
| `vMAJOR.MINOR` | Accept PATCH updates automatically |
| `vMAJOR` | Accept MINOR/PATCH updates automatically — not recommended before `v1.0.0`, see the pre-`v1` guarantee note above |
| `latest` | Local/dev experiments only; never production |

### Deploy via Helm

```bash
helm upgrade iam-delegation ./deploy/helm/iam-delegation \
  --install \
  --namespace <your-namespace> \
  --set image.tag=v0.1.0 \
  --set existingSecret=iam-delegation-secrets
```

`image.tag` (`deploy/helm/iam-delegation/values.yaml`) defaults to the chart's own `appVersion` (`deploy/helm/iam-delegation/Chart.yaml`) when unset — see `_helpers.tpl`'s image template. Chart `version`/`appVersion` are meant to be bumped manually alongside the Git tag (see the maintainer process below); since neither has ever been bumped and no tag has ever been cut, always set `image.tag` explicitly — do not rely on the chart's default in any environment that matters. `existingSecret` must reference a Kubernetes Secret carrying `DATABASE_URL`, `SYSTEM_DATABASE_URL`, `MIGRATION_DATABASE_URL`, `VALKEY_PASSWORD`, and (if docs are gated) `DOCS_AUTH_TOKEN` — see `values.yaml`'s `envFromSecret` list.

## Maintainer release process

`.github/workflows/release.yml` automates validation, the image build/scan/sign, an optional live health-gated deploy, and GitHub Release creation. Your job is to prepare the commit and push the tag.

1. **Merge** all changes for the release to `main`. `.github/workflows/changelog-check.yml` already blocks any PR that touches `internal/`, `api/`, `deploy/`, or `cmd/` without a `CHANGELOG.md` update, so `[Unreleased]` should already be current.

2. **Update `CHANGELOG.md`:** move `[Unreleased]` entries into a new `## [X.Y.Z] - YYYY-MM-DD` section. The `build` job's `verify-changelog-entry.sh` step fails the release if this section is missing — cut it *before* tagging. Since this repo has never released, the very first tag will move the entire `[Unreleased]` history accumulated so far (DLG-D1 through DLG-D34) into that first version section.

3. **Bump `deploy/helm/iam-delegation/Chart.yaml`'s `version`/`appVersion`** to match `X.Y.Z`. Nothing does this automatically — the release workflow overrides `image.tag` at deploy time via `--set`, but a consumer who deploys the chart without setting `image.tag` explicitly gets whatever `appVersion` was last committed, so a forgotten bump here silently ships a stale (or, for the first release, entirely fictitious) image reference to anyone relying on the chart's own default.

4. **Run `make ci` locally** to confirm everything is green before tagging:
   ```bash
   make ci   # tidy + fmt-check + vet + lint + arch-lint + test-ci (race, coverage ≥95%) + build
   ```

5. **Create and push an annotated tag** — this triggers the release workflow automatically:
   ```bash
   git tag -a v0.1.0 -m "v0.1.0"
   git push origin v0.1.0
   ```

6. **The release workflow** (triggered by the tag) runs:
   - `validate-test`/`validate-quality` — the same reusable gates `ci.yml` uses, re-run at the exact tagged commit.
   - `build` — verifies the tag matches HEAD (`verify-release-tag.sh`), verifies the CHANGELOG entry exists, cross-compiles **both** binaries (`iam-delegation-server`, `iam-delegation-reconciler`) for multiple platforms with per-binary checksums — a convenience for non-Docker runs; this service is deployed exclusively as a container.
   - `docker` — builds and pushes the semver-tagged image carrying both binaries (see tag scheme above), CVE-scans it with Trivy (fails the release on CRITICAL/HIGH), generates a CycloneDX SBOM and SLSA provenance, signs with Cosign and self-verifies.
   - `deploy-gate` (requires the `KUBECONFIG_B64` secret; fails immediately if not configured) — live Helm deploy (also setting `secretValues.DATABASE_URL`/`SYSTEM_DATABASE_URL`/`MIGRATION_DATABASE_URL`/`VALKEY_PASSWORD` from repo secrets), deployed-image-digest verification (guards against a GitOps controller substituting a different image after the Helm upgrade), rollout wait, then a 2-minute error-rate health check (`http_requests_total{error_class="server_error"}`, the label `platform-gincommon` emits) with automatic `helm rollback` on failure.
   - `publish` — creates the GitHub Release with the CHANGELOG section as notes, plus binaries/checksums/SBOM/provenance attached; only runs if `deploy-gate` succeeded, so a skipped/failed gate never produces a Release a consumer could mistake for production-verified.

   Separately, `.github/workflows/schema-registry.yml` and `schema-health-quarterly.yml`/`schema-prune.yml` handle registering `api/asyncapi.yaml`'s event schemas to the AWS Glue registry `iam-delegation-events` and its ongoing governance — see [docs/runbook-schema-registry.md](docs/runbook-schema-registry.md) and `.claude/operations.md`'s Schema Governance section. `make schema-verify` (pre-deploy Glue-vs-spec drift check) is a local/CI safety check, not itself a `release.yml` job.

7. **Notify consumers** — every service that calls this API or subscribes to `iam.delegation.events` (Workflow Service, Notification, Audit Log — see `api/asyncapi.yaml`'s fan-out queue registry) and whoever owns the Helm deployment — with upgrade notes if MINOR or MAJOR.

### Pre-release tags (optional)

| Tag pattern | Meaning |
|-------------|---------|
| `v1.1.0-rc.1` | Release candidate; not for production unless approved |
| `v1.1.0-beta.1` | Early integration testing |

Both match `release.yml`'s tag trigger (`v[0-9]*.[0-9]*.[0-9]*-*`) and produce a signed, scanned image — but never a `latest` tag (see the tag scheme table above).

## Compatibility matrix

| iam-delegation | Go (`go.mod`) | `platform-gincommon` | `platform-events` | `platform-pgcommon` | `platform-schemagov` (`schema-gov` CLI) |
|---|---|---|---|---|---|
| `main` (unreleased — no tag exists yet) | `1.26.6` | `v1.3.0` | `v1.4.0` | `v1.3.0` | `0.4` (`SCHEMA_GOV_IMAGE` in `.env.example`) |

Other key runtime dependencies pinned in `go.mod` at the time of a given release (see `CHANGELOG.md` for exact per-release bumps): AWS SDK v2 (`sns` via `platform-events`, `sqs`, `glue`), `pgx/v5`, `go-redis/v9`, `santhosh-tekuri/jsonschema/v6` (event payload validation). None are re-exported — a consumer of this *service* never needs any of them as a direct dependency.

## Related files

| File | Purpose |
|------|---------|
| [CHANGELOG.md](./CHANGELOG.md) | User-facing history — currently all under `[Unreleased]`, since no version has ever been cut |
| [README.md](./README.md) | Mental model, interface contract, local dev, CI/CD summary |
| [CONTRIBUTING.md](./CONTRIBUTING.md#deployment) | Full deployment/release pipeline description, required secrets |
| [docs/lld/iam-lld-delegation-service.md](./docs/lld/iam-lld-delegation-service.md) | The runtime contract in full — endpoint shapes, event schemas, config, metrics |
| [docs/runbook-schema-registry.md](./docs/runbook-schema-registry.md) | Operator runbook for Glue registry drift, rollback, IAM |
| [ARCHITECTURE.md](./ARCHITECTURE.md) | Layer model, key invariants, Mermaid diagrams |
| [api/asyncapi.yaml](./api/asyncapi.yaml) | Published/consumed event contract — the async half of the runtime contract |
| [docs/swagger/](./docs/swagger/) | Generated REST API spec (`make swag`) — the sync half of the runtime contract |
| [.github/workflows/release.yml](./.github/workflows/release.yml) | Automated release pipeline |
| [deploy/helm/iam-delegation/Chart.yaml](./deploy/helm/iam-delegation/Chart.yaml) | Helm chart version / app version |
| [go.mod](./go.mod) | Module path and minimum Go version — not a consumable package |
