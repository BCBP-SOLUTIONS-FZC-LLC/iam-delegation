
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
