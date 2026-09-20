// Package port defines the outbound/inbound interfaces internal/core/service
// depends on — repositories, the outbox-backed event publisher, the
// idempotency store, the cache, the transactional runner, the shared
// Zap-backed Logger (platform-gincommon/pkg/logger.NewLogger), and the
// membership/user-profile clients used to reach Core and iam-user-profile
// (LLD §6 "Architecture", §7 "Domain & Persistence"). Also declares
// TenderScopeClient (LLD §7.6.7, DLG-D13) — no implementing adapter is wired
// into the composition root yet, since Tender has not shipped the provider
// endpoint (IB-4, blocked on EXT-1); see internal/adapter/outbound/tender's
// package doc. Adapters under internal/adapter implement these interfaces;
// internal/core/service depends only on the interfaces here, never on a
// concrete adapter, keeping the hexagonal boundary intact.
package port
