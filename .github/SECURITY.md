# Security Policy

## Supported Versions

| Version | Supported |
|---------|-----------|
| 1.x     | ✅ Active |

Older major versions are not patched. Deploy the latest 1.x image.

## Reporting a Vulnerability

**Do not open a public GitHub issue for security vulnerabilities.**

Email: vijay@bcbpsolutions.com
Subject: `[iam-delegation] Security vulnerability`

Include in your report:
- Description of the vulnerability and the affected component (HTTP handler, RLS policy, event payload, cascade consumer, membership-check client, etc.)
- Steps to reproduce
- Potential impact (RLS bypass, tenant data leakage, privilege escalation, delegation grant forgery, DoS, etc.)
- Suggested fix or patch (if any)

### Response timeline

| Step | Target |
|------|--------|
| Initial acknowledgement | 48 hours |
| Severity assessment | 5 business days |
| Patch release (critical/high) | 14 days |
| Public disclosure | After patch ships |

We follow responsible disclosure. Reporters will be credited in release notes unless anonymity is requested.

## Scope

Areas of particular sensitivity in this service:

- **Row-Level Security (RLS)** — tenant isolation enforced at the Postgres layer on both `delegations` and `delegation_tenant_settings`; a bypass would expose cross-tenant delegation grants and policy.
- **GUC injection** — `app.tenant_id` is bound per-request (public routes via `tenantGUCMiddleware`, internal reads via `gucBoundReader`, crons/cascade via `BindTenantGUC`); an unbound or incorrectly-bound GUC would allow cross-tenant writes or silently no-op an intended update (RLS-6).
- **Grant-time membership checks** — `MembershipCheckClient.Exists` against Core is the sole replacement for the composite FKs the split lost (LLD §7.6); a client that fails open (returns `active:true` on error) would let a delegation be granted to a non-member.
- **Availability-first coordination (DEL-6)** — the ordering guarantee (User Profile call before/around the DB write, pointer-clear-only on every end path) prevents split-brain between `user_availability.delegate_id` and `delegations.status`.
- **Outbox payloads** — event payloads are validated against JSON Schemas before insert; a bypass could propagate malformed data to Workflow/Notification/Audit.
- **Cascade consumer idempotency** — `processed_events` dedup prevents a redelivered `MembershipRevoked`/`TenantOffboarded` from double-processing; a bypass could double-clear availability or emit duplicate `DelegationEnded` events.
