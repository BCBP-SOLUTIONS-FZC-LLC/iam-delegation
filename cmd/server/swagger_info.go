// Package main global Swagger annotations. `swag init` reads this file
// (via `-g swagger_info.go`) to build the top-level OpenAPI/Swagger
// specification — title, version, security schemes, and tag descriptions.
// Per-handler `// @…` annotations live next to each handler function under
// internal/adapter/inbound/http/.
//
// Regenerate the spec with:
//
//	make swag
//
// The generated files under docs/swagger/ are checked into the repo;
// CI's swagger-staleness check fails a PR whose annotations diverge from
// them.
//
// No @BasePath is set: /api/v1/delegations* and /internal/* are both
// mesh/gateway-fronted with different prefixes, and /healthz, /readyz,
// /asyncapi[.yaml] have none at all — every @Router annotation below is a
// full, absolute path (mirrors iam-tender-acl's identical rationale).
//
// @title           Delegation Service API
// @version         1.0
// @description     The system of record for out-of-office (OOO) delegations — time-bounded grants that drive workflow rerouting. Extracted from iam-org-membership per ADR-0008 (Option C), the fourth and last O&M decomposition wave.
// @description
// @description     **Route prefixes.**  `/api/v1/delegations*` — tenant callers (self or `tenant_admin`/`tenant_owner` via `x-tenant-roles`, gateway-injected identity).  `/internal/*` — in-mesh service-to-service, mTLS trust boundary only, no RBAC/JWT check at all (LLD §8.2/§13.2).
// @description
// @description     DLG-2 (create) requires an `Idempotency-Key` header; a repeated key within 24h returns the original 201 rather than creating a second delegation (DLG-D8).
//
// @contact.name   BCBP Solutions
//
// @license.name   Proprietary
//
// @securityDefinitions.apikey UserID
// @in                         header
// @name                       x-user-id
// @description                Authenticated user UUID injected by the API gateway. Required on /api/v1/* routes only — /internal/* is mesh-only and carries no identity headers.
//
// @securityDefinitions.apikey TenantID
// @in                         header
// @name                       x-tenant-id
// @description                Tenant UUID injected by the API gateway.
//
// @securityDefinitions.apikey TenantRoles
// @in                         header
// @name                       x-tenant-roles
// @description                Comma-separated tenant-role list injected by the API gateway. DLG-3/4/5 accept self or tenant_admin/tenant_owner; DLG-7 requires tenant_admin/tenant_owner.
//
// @tag.name         delegations
// @tag.description  Lifecycle + policy API (DLG-1..7)
//
// @tag.name         internal
// @tag.description  Mesh-only cron entry points and reads (DLG-I1..I4) — no RBAC
//
// @tag.name         infra
// @tag.description  Health checks (unauthenticated)
package main
