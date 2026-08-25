---
name: Bug report
about: Report a defect in the iam-delegation service (HTTP API, event publishing, cascade consumer, or database layer)
title: '[BUG] '
labels: bug
assignees: ''
---

## Description
A clear description of the bug.

## Service version
`github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation` — tag / commit SHA:

## Go version
`go version goX.Y.Z ...`

## Environment
- [ ] Local dev (`make run`)
- [ ] Docker Compose (`make compose-up`)
- [ ] Staging
- [ ] Production

## Affected area
- [ ] HTTP API (endpoint: `METHOD /api/v1/delegations...`)
- [ ] Internal API (endpoint: `METHOD /internal/...`)
- [ ] Event publishing (event type: `Delegation*`)
- [ ] Cascade consumer (`delegation-cascade-q` — `MembershipRevoked`/`TenantMembershipsPurged`)
- [ ] Reconciler (`delegation-expiry` / `delegation-review` / `delegation-cleanup`)
- [ ] Cache (Valkey)
- [ ] Database / migrations / RLS
- [ ] Outbox relay

## Steps to reproduce
1.
2.
3.

## Expected behaviour
What you expected to happen.

## Actual behaviour
What actually happened. Include error messages, HTTP status codes, log output, or stack traces.

```
// paste relevant log output or error here
```

## Minimal reproduction
```go
// paste the smallest snippet or curl command that triggers the bug
```

## Additional context
Any other relevant context (Postgres version, PgBouncer mode, GLUE_REGISTRY_NAME set, related issues).
