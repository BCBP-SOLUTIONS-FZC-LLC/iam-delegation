// Package consumer implements this service's single inbound event-driven
// behavior: consuming Core's MembershipRevoked / TenantMembershipsPurged signals
// off delegation-cascade-q and dispatching them to CascadeService (LLD
// §10.1, §11.5/§11.6). There is no outbox/producer wiring here — this
// package is inbound-only; the outbound side (transactional outbox →
// iam.delegation.events) lives elsewhere.
package consumer

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	pgcommon "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

// ProcessedEvents implements the idempotency ledger backing CascadeConsumer
// (LLD §7.2.3). The table's primary key is composite — (event_id, consumer)
// — with `consumer ∈ {cascade, offboarding}` per the LLD's ERD comment:
// this service physically has ONE consumer type (CascadeConsumer) reading
// ONE queue (delegation-cascade-q), but it drives two logically distinct,
// independently-idempotent cascade behaviors —
//
//   - "cascade"     — MembershipRevoked → CascadeService.EndForUser
//   - "offboarding" — TenantMembershipsPurged  → CascadeService.ScrubTenant
//
// Design choice (deliberately different from iam-tender-acl's
// ProcessedEvents, which bakes one fixed consumer name into each instance
// and constructs one instance per consumer): here the consumer name is a
// parameter on every call rather than a constructor argument. That lets
// cmd/server wire a single *pgcommon.Pool-backed ledger for the one
// CascadeConsumer, which itself picks the bucket name from env.Type inside
// Handle. Functionally equivalent to two instances sharing one table; this
// shape just matches "one consumer type, one queue, one constructor call"
// more directly.
//
// Exempt from RLS (operational table, no tenant_id column) — pool must be
// constructed by the caller with no GUCProvider, so no GUC injection is
// ever attempted for this table. Not this package's job to construct that
// pool: it's injected via NewProcessedEvents from cmd/server.
type ProcessedEvents struct {
	pool *pgcommon.Pool
}

// NewProcessedEvents builds a ProcessedEvents ledger from an
// already-constructed, RLS-exempt *pgcommon.Pool (no GUCProvider). Does NOT
// call pgcommon.NewPool itself.
func NewProcessedEvents(pool *pgcommon.Pool) *ProcessedEvents {
	return &ProcessedEvents{pool: pool}
}

// IsProcessed reports whether eventID has already been recorded as
// successfully processed under the given consumer bucket.
func (r *ProcessedEvents) IsProcessed(ctx context.Context, consumer, eventID string) (bool, error) {
	var exists bool
	err := r.pool.WithConn(ctx, func(ctx context.Context, conn *pgxpool.Conn) error {
		return conn.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM processed_events WHERE event_id = $1 AND consumer = $2)`,
			eventID, consumer,
		).Scan(&exists)
	})
	if err != nil {
		return false, fmt.Errorf("check processed_events: %w", err)
	}
	return exists, nil
}

// MarkProcessed records eventID as successfully processed under the given
// consumer bucket. Safe to call more than once for the same (eventID,
// consumer) pair.
func (r *ProcessedEvents) MarkProcessed(ctx context.Context, consumer, eventID string) error {
	err := r.pool.WithConn(ctx, func(ctx context.Context, conn *pgxpool.Conn) error {
		_, err := conn.Exec(ctx, `
			INSERT INTO processed_events (event_id, consumer, processed_at)
			VALUES ($1, $2, now())
			ON CONFLICT (event_id, consumer) DO NOTHING`,
			eventID, consumer,
		)
		return err
	})
	if err != nil {
		return fmt.Errorf("mark processed_events: %w", err)
	}
	return nil
}

// CleanupExpired deletes processed_events rows (across both consumer
// buckets) older than olderThan (LLD §18.4: "processed_events 30 days" —
// callers pass 30*24*time.Hour; not hardcoded here so the retention window
// stays a cmd/server/cron concern, not baked into the adapter).
func (r *ProcessedEvents) CleanupExpired(ctx context.Context, olderThan time.Duration) error {
	err := r.pool.WithConn(ctx, func(ctx context.Context, conn *pgxpool.Conn) error {
		// $1 is seconds (float8), not a Go duration string — Postgres's
		// interval literal parser does not understand Go's "720h0m0s"
		// format, so this multiplies a numeric interval literal instead.
		_, err := conn.Exec(ctx,
			`DELETE FROM processed_events WHERE processed_at < now() - ($1::float8 * interval '1 second')`,
			olderThan.Seconds(),
		)
		return err
	})
	if err != nil {
		return fmt.Errorf("cleanup processed_events: %w", err)
	}
	return nil
}
