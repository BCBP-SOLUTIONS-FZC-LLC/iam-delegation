# Runbook — AWS Glue Schema Registry state

## The contract

The service reads its four event schemas from AWS Glue at startup
(`eventbus.NewGlueCodec` pre-fetches every version ID). Startup fails fast if
any schema is missing — the pod will not accept traffic. This is intentional;
publishing an event whose header points at a non-existent Glue schema
version silently poisons every consumer downstream.

The four expected schema names in the registry (`iam-delegation-events`) are
**identical to the domain event-type constants** — no PascalCase translation
table is needed, unlike `iam-user-profile`'s `domain.GlueSchemaName`:

| Domain event type (`internal/core/domain/event.go`) | Registered Glue schema name |
|---|---|
| `EventDelegationStarted` | `DelegationStarted` |
| `EventDelegationEnded` | `DelegationEnded` |
| `EventDelegationReviewRequested` | `DelegationReviewRequested` |
| `EventDelegationEscalationRequested` | `DelegationEscalationRequested` |

Note: the **JSON filenames** in `internal/eventschema/` (`delegation_started.json`,
etc.) are snake_case to satisfy `schema-gov validate`'s Pass 7 coverage rule.
That is a source-file convention only and does not dictate the registered
Glue schema name — `.github/workflows/schema-registry.yml`'s diff step
translates between the two via an explicit `SCHEMA_NAME_MAP`.

## Pre-deploy checklist

Run this once, in the target AWS account, before every promotion to a new
environment. Fails loudly if any schema is missing.

```bash
export GLUE_REGISTRY_NAME=iam-delegation-events   # or the env-specific name
export AWS_REGION=ap-south-1
make schema-verify
```

Expected output:

```
OK: all four schemas present in registry 'iam-delegation-events'
```

Failure modes and fixes:

- `missing Glue schemas ... DelegationStarted DelegationEnded ...`
  The registry does not yet have the schemas. Register them:

  ```bash
  make schema-register
  make schema-verify   # re-check
  ```

- The IAM role running `aws glue get-schema` returns AccessDenied.
  Ensure the caller can `glue:GetSchemaVersion` (see
  `deploy/iam/policy.json` sid `GlueSchemaRegistryReadOnly`).

## What happens if a schema is missing at pod startup

`NewGlueCodec` returns an error of the form:

```
prefetch glue schema "DelegationStarted" in registry "iam-delegation-events":
<underlying AWS error> — confirm the four expected schemas exist
(DelegationStarted, DelegationEnded, DelegationReviewRequested,
DelegationEscalationRequested)
```

The pod exits before opening the HTTP port. Kubernetes will restart-loop it
(CrashLoopBackOff). Nothing publishes to SNS during this state.

**Mitigation:** roll back the Helm release with `helm rollback` and register
the missing schema, then re-deploy. Do NOT set `GLUE_REGISTRY_NAME=""` in an
attempt to fall through to the `NoopCodec` — that publishes unversioned JSON
and permanently corrupts the audit trail for the duration.

## Local development

`scripts/init-floci.sh` registers the four schemas under the same names into
floci's real Glue Schema Registry — floci includes it in the free tier
(unlike LocalStack Community, which gated it behind Pro), so `docker-compose.yml`'s
default stack always runs the real Glue codec locally; `GLUE_REGISTRY_NAME=""`
(NoopCodec) is only needed if you deliberately want to bypass it.

## Adding a new event type

1. Add the payload struct and event-type constant to
   `internal/core/domain/event.go` (PascalCase — it IS the Glue schema name
   here, no translation switch to extend).
2. Add the JSON schema file to `internal/eventschema/` using the snake_case
   filename convention (e.g. `delegation_transferred.json`).
3. Add the schema to `eventschema.ByEventType` in `internal/eventschema/schemas.go`
   (`ValidatingCodec` compiles this map at enqueue) **and** the
   `schemaNames` slice passed to `eventbus.NewGlueCodec` in
   `cmd/server/main.go` (the Glue pre-fetch list `NewGlueCodec` fails fast
   on at startup) — two separate lists, easy to update one and miss the
   other; DLG-D27 shipped `delegate_disabled` in the Go `EndReason` enum
   without updating the *schema's* enum (step 4 below) and initially missed
   this exact `NewGlueCodec` list too, both silent-failure gaps invisible to
   unit tests (they use a fake `EventPublisher` that skips schema
   validation, and never construct a real `GlueCodec`).
4. If the new event type reuses an existing enum field on an already-shipped
   payload (e.g. adding a value to `ended_reason`), update that field's enum
   in **both** `internal/eventschema/{name}.json` **and**
   `api/asyncapi.yaml`'s matching schema — not just the Go `const` block.
   `make schema-validate` (step 7) catches this locally; without it, a
   payload using the new enum value fails real `SchemaValidator` validation
   at publish time even though every Go-level unit test passes.
5. Extend `SCHEMA_NAME_MAP` in `.github/workflows/schema-registry.yml` (all
   three job blocks) and the `register_schema` call in
   `scripts/init-floci.sh`.
6. Add `x-lifecycle`/`x-owner` annotations to the new message in
   `api/asyncapi.yaml`.
7. Run `make schema-validate` locally.
8. Merge, then run `make schema-register` in every environment before the
   first pod that would publish the new event starts.

## IAM policy — required SIDs

The pod's IAM role must include the following SID (see
`deploy/iam/policy.json`). Missing it produces `AccessDenied` at startup.

**Glue Schema Registry (read):**
- `GlueSchemaRegistryReadOnly` — `glue:GetRegistry`, `glue:GetSchema`,
  `glue:GetSchemaVersion`, `glue:GetSchemaByDefinition`, `glue:ListSchemas`,
  `glue:QuerySchemaVersionMetadata` on `${glue_registry_arn}` +
  `${glue_registry_arn}/*`.

Schema registration (`glue:CreateSchema`, `glue:RegisterSchemaVersion`,
`glue:DeleteSchema`) is CD-only — granted to the CI runner's AWS credentials
configured on the `staging`/`production` GitHub Environments, not to the
pod's own IRSA role.

## Required env vars

Set in `deploy/helm/iam-delegation/values.yaml` (per environment) or `.env`
(dev, via `docker-compose.yml`'s `floci` service — Floci includes Glue
Schema Registry in its free tier, so no separate Pro-tier compose stack is
needed):

| Variable | Purpose | Notes |
|---|---|---|
| `GLUE_REGISTRY_NAME` | Glue registry name | Set to `iam-delegation-events` in production/staging, and by default in local dev too (floci provisions it for free). Leave empty to force NoopCodec. |
| `GLUE_REGISTRY_ARN` | Full registry ARN | For IAM policy scoping; used by `schema-gov register` and `deploy/iam/policy.tf.example`. Not read by the Go runtime. |
| `AWS_REGION` | Primary region | `ap-south-1` everywhere — production, local dev, and CI (see `deploy/helm/iam-delegation/values.yaml`; local dev/CI additionally point `AWS_ENDPOINT_URL` at floci). |

## CI governance pipeline

See `.github/workflows/schema-registry.yml` (validate/diff/register on every
push and release), `schema-prune.yml` (monthly orphan-cleanup), and
`schema-health-quarterly.yml` (read-only quarterly report) — all mirror
`iam-user-profile`'s pipeline. `freeze-watchdog.yml` alerts if
`SCHEMA_FREEZE` is left set too long. Required GitHub Environment
secrets/variables are documented in each workflow's header comment.
