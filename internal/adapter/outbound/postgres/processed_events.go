package postgres

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/jackc/pgx/v5"
)

// ProcessedEventsRepository implements the cascade-consumer dedup ledger
// (LLD §7.2.3) against `processed_events`. RLS-exempt — pool must be
// constructed with no GUCProvider (no tenant_id column).
//
// SQL goes through withPool so a caller already inside TxRunner.RunInTx
// joins that transaction (iam-realm-provisioner / iam-org-membership
// IDEMP-2). CascadeConsumer.runCascadeAndMark opens that outer tx so
// MarkProcessed commits atomically with the cascade write.
type ProcessedEventsRepository struct {
	pool *pgcommon.Pool
}

// NewProcessedEventsRepository constructs a ProcessedEventsRepository
// backed by the BYPASSRLS sysPool.
func NewProcessedEventsRepository(pool *pgcommon.Pool) *ProcessedEventsRepository {
	return &ProcessedEventsRepository{pool: pool}
}

// IsProcessed reports whether eventID has already been recorded as
// successfully processed under the given consumer bucket.
func (r *ProcessedEventsRepository) IsProcessed(ctx context.Context, consumer, eventID string) (bool, error) {
	var exists bool
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM processed_events WHERE event_id = $1 AND consumer = $2)`,
			eventID, consumer,
		).Scan(&exists)
	})
	if err != nil {
		return false, err
	}
	return exists, nil
}

// MarkProcessed records eventID as successfully processed under the given
// consumer bucket. Safe to call more than once for the same (eventID,
// consumer) pair.
func (r *ProcessedEventsRepository) MarkProcessed(ctx context.Context, consumer, eventID string) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO processed_events (event_id, consumer, processed_at)
			VALUES ($1, $2, now())
			ON CONFLICT (event_id, consumer) DO NOTHING`,
			eventID, consumer,
		)
		return err
	})
}

// CleanupExpired deletes processed_events rows older than olderThan
// (LLD §18.4). $1 is seconds (float8), not a Go duration string —
// Postgres's interval parser does not understand Go's "720h0m0s".
func (r *ProcessedEventsRepository) CleanupExpired(ctx context.Context, olderThan time.Duration) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`DELETE FROM processed_events WHERE processed_at < now() - ($1::float8 * interval '1 second')`,
			olderThan.Seconds(),
		)
		return err
	})
}

// Prune deletes up to limit processed_events rows older than ttlDays,
// matching iam-realm-provisioner's ProcessedEventsRepository.Prune
// (subquery + LIMIT, then DELETE … WHERE (event_id, consumer) IN …).
func (r *ProcessedEventsRepository) Prune(ctx context.Context, ttlDays, limit int) (int, error) {
	var n int
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			DELETE FROM processed_events
			WHERE (event_id, consumer) IN (
				SELECT event_id, consumer FROM processed_events
				WHERE processed_at < now() - make_interval(days => $1)
				LIMIT $2
			)`, ttlDays, limit)
		if err != nil {
			return err
		}
		n = int(tag.RowsAffected())
		return nil
	})
	return n, err
}
