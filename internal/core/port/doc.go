// Package port defines the outbound/inbound interfaces internal/core/service
// depends on — repositories, the outbox-backed event publisher, the
// idempotency store, the cache, the transactional runner, and the
// membership/user-profile clients used to reach Core and iam-user-profile
// (LLD §6 "Architecture", §7 "Domain & Persistence"). Adapters under
// internal/adapter implement these interfaces; internal/core/service depends
// only on the interfaces here, never on a concrete adapter, keeping the
// hexagonal boundary intact.
package port
