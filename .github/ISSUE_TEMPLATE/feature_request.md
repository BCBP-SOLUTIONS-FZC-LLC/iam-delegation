---
name: Feature request
about: Propose a new endpoint, domain field, event type, cache strategy, or service behaviour
title: '[FEAT] '
labels: enhancement
assignees: ''
---

## Problem / motivation
What problem does this solve? Which consumers or use cases are affected?

## Proposed solution
Describe the API or behaviour change you'd like.

```go
// Example: new endpoint, payload struct, or domain type
```

## Affected areas
- [ ] HTTP API (new or changed endpoint)
- [ ] Internal API
- [ ] Domain model (new field or entity)
- [ ] Database schema (new migration required)
- [ ] Event contract (new event type or changed payload — asyncapi.yaml update needed)
- [ ] Cache strategy
- [ ] Reconciler / CronJob behaviour
- [ ] Helm / deployment config

## Event contract impact
If this adds or changes an event type, describe the downstream consumer impact (Workflow Service, Notification, Audit Log, or Core's cascade consumer on delegation-cascade-q).

## Alternatives considered
Other approaches you evaluated and why you ruled them out.

## Acceptance criteria
- [ ]
- [ ]
- [ ]

## Additional context
Links to related issues, LLD sections, ADRs, or prior art.
