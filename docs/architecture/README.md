# Architecture diagrams

Standalone Mermaid source files for `ARCHITECTURE.md`. Each `.mmd` file is embedded as a fenced code block in the parent document with a `> Source:` back-reference. The eight request/data-model diagrams (everything but `layer-model.mmd`, `package-dependencies.mmd`, and `rls-guc-flow.mmd`) are extracted verbatim from the full LLD, `docs/lld/iam-lld-delegation-service.md` §7/§11 — not re-derived, so they stay in sync by re-copying from the LLD rather than hand-editing independently. `layer-model.mmd`, `package-dependencies.mmd`, and `rls-guc-flow.mmd` are authored directly against the current source (Go import blocks, `router.go`, `cmd/server/adapters.go`, `cmd/reconciler/jobs`) rather than derived from the LLD.

| File | Diagram | Embedded in | LLD source |
|------|---------|------------|------------|
| [`layer-model.mmd`](mermaid/layer-model.mmd) | Clean Architecture layer graph, `cmd`→adapters→core | ARCHITECTURE.md §Layer model | — (authored for this doc, not in the LLD) |
| [`package-dependencies.mmd`](mermaid/package-dependencies.mmd) | Go import graph — module-internal edges only | ARCHITECTURE.md §Package dependency graph | — (authored from actual import blocks) |
| [`data-model.mmd`](mermaid/data-model.mmd) | `delegations` / `delegation_tenant_settings` / `processed_events` ER diagram | ARCHITECTURE.md §Data model | LLD §7 |
| [`request-preamble-flow.mmd`](mermaid/request-preamble-flow.mmd) | Gateway → AuthZ Enrichment → service preamble every public route runs | ARCHITECTURE.md §Key request flows | LLD §11.0 |
| [`create-flow.mmd`](mermaid/create-flow.mmd) | DLG-2 create — idempotency, dual membership checks, availability-first, outbox | ARCHITECTURE.md §Key request flows | LLD §11.1 |
| [`cancel-flow.mmd`](mermaid/cancel-flow.mmd) | DLG-3 cancel — fail-open pointer-clear, optimistic lock | ARCHITECTURE.md §Key request flows | LLD §11.2 |
| [`expiry-cron-flow.mmd`](mermaid/expiry-cron-flow.mmd) | DLG-I1 expiry CronJob — availability-first, self-retrying | ARCHITECTURE.md §Key request flows | LLD §11.3 |
| [`review-cron-flow.mmd`](mermaid/review-cron-flow.mmd) | DLG-I2 review-window CronJob — dual 7d/3d warn, then auto-end | ARCHITECTURE.md §Key request flows | LLD §11.4 |
| [`cascade-removal-flow.mmd`](mermaid/cascade-removal-flow.mmd) | Core `MembershipRevoked` → async row-end cascade | ARCHITECTURE.md §Key request flows | LLD §11.5 |
| [`read-policy-flow.mmd`](mermaid/read-policy-flow.mmd) | DLG-1 list (cached), DLG-4 extend, DLG-5 reassign, DLG-6/7 settings | ARCHITECTURE.md §Key request flows | LLD §11.7 |
| [`rls-guc-flow.mmd`](mermaid/rls-guc-flow.mmd) | Three tenant-GUC binding paths (public middleware, mesh-only per-call, injected job/consumer binder) converging on `postgres.WithTenantGUC` | ARCHITECTURE.md §Row-Level Security (RLS) and GUC injection | — (authored from `router.go`, `cmd/server/adapters.go`, `cmd/reconciler/jobs`; cross-references LLD §7.3/§13.1) |

`request-preamble-flow.mmd` through `read-policy-flow.mmd` and `rls-guc-flow.mmd` are sequence/graph diagrams; `data-model.mmd` is an ER diagram; `layer-model.mmd` and `package-dependencies.mmd` are dependency graphs.

No diagram exists for LLD §11.6 (tenant-lifecycle cleanup) — that flow is one sentence of prose in the LLD (cascade consumer soft-deletes on `TenantOffboarded`; monthly `delegation-cleanup` hard-purges after 90 days), not a sequence worth its own diagram.

To render locally, open any `.mmd` file in a Mermaid-aware IDE (VS Code + Mermaid Preview, IntelliJ + Mermaid plugin) or paste into [mermaid.live](https://mermaid.live).
