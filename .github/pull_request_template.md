
## Description
Provide a clear description of the changes.

---

## Type of Change
- [ ] Bug fix
- [ ] New feature
- [ ] Refactor
- [ ] Documentation
- [ ] Test
- [ ] Breaking change
- [ ] New migration
- [ ] Event contract change (asyncapi.yaml or eventschema/)

---

## Testing
- [ ] Unit tests added/updated (`make test-unit`)
- [ ] Postgres / RLS integration tests added/updated (`make test-rls`) — every new/changed policy or query against `delegations`/`delegation_tenant_settings` covered by the §17.5 RLS matrix
- [ ] Integration tests added/updated (`make test-integration`)
- [ ] All tests passing with race detector (`make race`)
- [ ] Manual testing performed (if required)

---

## Checklist

### Code Quality
- [ ] Code is properly formatted (`make fmt-check`)
- [ ] Linting passed (`make lint`)
- [ ] Vet passed (`make vet`)
- [ ] Architecture layering passed (`make arch-lint`)
- [ ] No debug logs / commented-out code
- [ ] No secrets or DSNs hardcoded

### API Contract
- [ ] Swagger docs regenerated if handler annotations changed (`make swag` — all three files in `docs/swagger/` committed)
- [ ] AsyncAPI spec updated if a new event type or payload field was added (`api/asyncapi.yaml`)
- [ ] Event schema JSON files updated to match (`internal/eventschema/*.json`)
- [ ] `"additionalProperties": true` is set on every modified or new schema in `internal/eventschema/` (mirrors the shipped schemas' open-schema convention)

### Consumer Forward-Compatibility
*Complete only when `internal/eventschema/` changed.*

Backward-compatible changes (new fields, widened enums) do not require a consumer migration cycle,
but consumers must be configured for lenient deserialization or they will crash on the new fields.

- [ ] **Consumer lenient-parsing confirmed** — All known consumers of this event type are configured
      to ignore unknown fields. Required per-language settings:
      - **Go (encoding/json):** do NOT call `json.Decoder.DisallowUnknownFields()` — silently ignored by default.
      - **Go (sonic):** use `sonic.ConfigDefault` or `sonic.ConfigFastest`, NOT `sonic.ConfigStrict`.
- [ ] **Deploy order followed for non-breaking additions** — Consumers deployed first, then producer.
      New fields in the payload reach consumers before the producer starts sending them. Consumers
      that haven't been updated yet will receive the new field as an ignored unknown — no crash.
      See `api/asyncapi.yaml § x-forward-compatibility` for the canonical order.

### Event Schema Semantic Evolution
*Complete only when `api/asyncapi.yaml` or `internal/eventschema/` changed.*

Structural schema diff and lifecycle checks run in CI automatically
(`.github/workflows/schema-registry.yml`). This section covers semantic drift
that is **invisible to tooling**: a field's valid range, units, encoding, or
business meaning can change without any JSON Schema difference.

- [ ] **No semantic drift** — Confirmed that no existing field changed its valid range,
      units, encoding, or business meaning without a structural schema change.
      *Example of a semantic-only breaking change:*
      `days_remaining` meant business days, now means calendar days.
      The JSON Schema type stays `integer` — structural diff cannot catch this.
- [ ] **Semantic change acknowledged → `.v2` created** — If a field's meaning changed
      without a structural change, a new versioned event type has been created
      (e.g. `DelegationEnded.v2`) and the old type is marked deprecated in `asyncapi.yaml`
      with `x-lifecycle: {status: deprecated, deprecated-by: DelegationEnded.v2, retire-after: YYYY-MM-DD}`.
- [ ] **Description-only change confirmed** — Any `description` field edits in
      `asyncapi.yaml` are wording clarifications only (no change to valid range,
      units, nullability, or consumer-observable behaviour).

### RLS / Tenant Isolation
*Complete only when a new query, table, or GUC-binding path was added.*
- [ ] Every new query against a tenant-scoped table runs inside a transaction with `app.tenant_id` bound via `pgcommon`'s `SET LOCAL` semantics — never a session-scoped `SET app.tenant_id` (CI's forbidden-GUC grep enforces this, but double-check any new call path: public routes go through `tenantGUCMiddleware`, internal reads through `gucBoundReader`, crons/cascade through their injected `BindTenantGUC`)
- [ ] New repository methods that intentionally read/write cross-tenant (the reconciler sweep finders, `HardPurgeSoftDeletedBefore`) are documented as requiring the BYPASSRLS pool, not the RLS-scoped one
- [ ] §17.5 RLS matrix (Cases 1–5) still passes on any table this PR touches

### Database / Migrations
- [ ] New migrations have matching `.up.sql` and `.down.sql`
- [ ] Down migration correctly reverses the up migration
- [ ] RLS policies tested with `FORCE ROW LEVEL SECURITY` (`make test-rls`)
- [ ] Migration tested against PgBouncer simple-protocol mode (`PG_BOUNCER_MODE=true`)

### Security
- [ ] No secrets or DSNs hardcoded
- [ ] GUC values are not logged (no credential leakage in slow-query output)
- [ ] New config fields documented in README's env-vars table

### Documentation
- [ ] README updated (if public API, env vars, or config changed)
- [ ] ARCHITECTURE.md updated (if layering, flows, or key invariants changed)
- [ ] IMPLEMENTATION_NOTES.md updated (if an LLD-section mapping or a new decision-register entry applies)

---

## Related Issue
Closes #<issue-id>

---

## Deployment Notes
Mention anything important for operators upgrading (migration steps, new required env vars, config changes, breaking event schema changes).
