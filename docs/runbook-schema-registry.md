# Runbook — AWS Glue Schema Registry state

## The contract

The service resolves its four event schemas' version IDs from AWS Glue
**once, at startup, by definition** (`eventbus.NewGlueCodec` calls
`GetSchemaByDefinition` with each schema exactly as this binary embeds it in
`internal/eventschema/`, compact + ASCII-escaped the way `schema-gov
register` uploads it, and requires the matched version to be `AVAILABLE`).
There is no background refresh: the binary always stamps the version that
describes the payloads *it* produces — never merely the registry's latest,
which a registration can move ahead of the running code (register-before-
deploy, rollback) or leave behind it (a deploy racing `schema-registry.yml`).
Startup fails fast if any definition is not registered — the pod will not
accept traffic. This is intentional; publishing an event whose header points
at a wrong or non-existent Glue schema version silently poisons every
consumer downstream.

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
Glue schema name. schema-gov takes a schema's name from its **file stem**, so
every name-bearing CI step — `usage-check`, `diff`, `register` — and
`make schema-register` read a PascalCase, produced-only copy staged by
`.github/scripts/stage-produced-event-schemas.sh` into `.tmp/glue-schemas`
(DLG-D50/D49). Registering `internal/eventschema/` directly would create
`delegation_started` etc. — schemas the codec never looks up — and
usage-classifying it would never match an asyncapi message name, which is
why lifecycle enforcement silently no-op'd before DLG-D51.

The files are **generated**: `api/asyncapi.yaml` is the single source, and
`make extract-schemas` derives all seven JSON files from its `<Name>Payload`
schemas. CI's "Event schema sync check" (`make schema-sync-check`) fails if
they drift, so never hand-edit a JSON file. Besides the four produced
schemas, the directory holds the three **consumed** ones
(`membership_revoked.json`, `tenant_memberships_purged.json`,
`user_updated.json`): they satisfy validate's Pass 7 coverage rule and are
embedded as `eventschema.Consumed` for consumer-side validation. The staging
script skips them, so they are never registered here — their producers own
them. When a producer changes one of those events, update the matching
`<Name>Payload` in `api/asyncapi.yaml` (never stricter than the producer's
schema) and re-extract.

## Pre-deploy checklist

Run this once, in the target AWS account, before every promotion to a new
environment. Runs the exact lookup the pod runs at startup
(`get-schema-by-definition` per schema, needs `aws` + `python3`) and fails
loudly unless each of this checkout's definitions is registered and
`AVAILABLE`.

```bash
export GLUE_REGISTRY_NAME=iam-delegation-events   # or the env-specific name
export AWS_REGION=ap-south-1
make schema-verify
```

Expected output:

```
OK: all four schema definitions registered and AVAILABLE in 'iam-delegation-events'
```

Failure modes and fixes:

- `... not registered+AVAILABLE ...: DelegationStarted(not-registered) ...`
  The registry has no version with this checkout's definition (schema
  missing, or registered from a different commit). Register them:

  ```bash
  make schema-register
  make schema-verify   # re-check
  ```

- `...(PENDING)` / `(FAILURE)` / `(DELETING)` — the version exists but is
  not usable yet (or any more); wait for / fix the registration.

- The IAM role running `aws glue get-schema-by-definition` returns
  AccessDenied. Ensure the caller can `glue:GetSchemaByDefinition` (see
  `deploy/iam/policy.json` sid `GlueSchemaRegistryReadOnly`).

## What happens if a schema is missing at pod startup

`NewGlueCodec` returns an error of the form:

```
resolve glue schema "DelegationStarted" in registry "iam-delegation-events" by definition:
<underlying AWS error, or "... is PENDING, not AVAILABLE"> — this binary's schema
version isn't registered yet: wait for schema-registry.yml to register it,
or run `make schema-verify`
```

The pod exits before opening the HTTP port. Kubernetes will restart-loop it
(CrashLoopBackOff). Nothing publishes to SNS during this state; the outbox
keeps accumulating, so no event is lost.

**Mitigation:** if `schema-registry.yml` is still running for this commit,
just wait — the CrashLoop self-heals on the first restart after registration
lands. Otherwise register the schema (`make schema-register`) or roll back
the Helm release with `helm rollback`. The same applies to a rollback onto
a binary whose definition was never registered. Do NOT set `GLUE_REGISTRY_NAME=""` in an
attempt to fall through to the `NoopCodec` — that publishes unversioned JSON
and permanently corrupts the audit trail for the duration.

## Local development

`scripts/init-floci.sh` registers the four schemas under the same PascalCase names into
floci's real Glue Schema Registry — floci includes it in the free tier
(unlike LocalStack Community, which gated it behind Pro), so `docker-compose.yml`'s
default stack always runs the real Glue codec locally; `GLUE_REGISTRY_NAME=""`
(NoopCodec) is only needed if you deliberately want to bypass it.

## Adding a new event type

1. Add the payload struct and event-type constant to
   `internal/core/domain/event.go` (PascalCase — it IS the Glue schema name
   here, no translation switch to extend).
2. Add the message to `api/asyncapi.yaml`: a `components/messages` entry
   with `x-lifecycle: {status: active}`/`x-owner` annotations, a
   `<Name>Envelope` schema (`EventEnvelopeBase` + `data`), and a flat
   `<Name>Payload` schema with `additionalProperties: true`.
3. Run `make extract-schemas` — it writes
   `internal/eventschema/<snake_name>.json` (e.g.
   `delegation_transferred.json`). Never hand-write or hand-edit it.
4. Add the file to `internal/eventschema/schemas.go` (`//go:embed` +
   `ByEventType`) — `ValidatingCodec` compiles this map at enqueue — **and**
   to the `schemaNames` slice passed to `eventbus.NewGlueCodec` in
   `cmd/server/main.go` (the list `NewGlueCodec` resolves by definition and
   fails fast on at startup; a name missing from `ByEventType` fails with
   "no embedded schema"). Two separate lists, easy to update one and miss
   the other: DLG-D27 shipped `delegate_disabled` in the Go `EndReason` enum
   without updating the schema's enum and initially missed this exact
   `NewGlueCodec` list too — both silent-failure gaps invisible to unit
   tests.
5. If the change widens an enum on an already-shipped payload (e.g. a new
   `ended_reason`), change it in `api/asyncapi.yaml`'s `<Name>Payload` and
   re-extract — not just the Go `const` block. Without it, a payload using
   the new value fails `ValidatingCodec` at enqueue even though every
   Go-level unit test passes.
6. Add the file stem → PascalCase name to `name_for` in
   `.github/scripts/stage-produced-event-schemas.sh` (it fails loudly on an
   unmapped file) and a `register_schema` call in `scripts/init-floci.sh`.
7. Run `make schema-validate` and `make schema-sync-check` locally.
8. Merge, then let `schema-registry.yml` register it (or run
   `make schema-register`) before the first pod that would publish the new
   event starts — a pod that starts first CrashLoops until it lands. The
   same holds for **any** change to an existing produced schema (even a
   description): the new binary resolves the new definition.

A new **consumed** event type follows steps 2–3 (payload mirroring the
producer's required fields and types, never stricter), adds the file to
`eventschema.Consumed` instead of `ByEventType`, and adds its stem to the
staging script's consumed list, plus the dispatch case in
`internal/adapter/inbound/consumer/cascade_consumer.go`.

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
