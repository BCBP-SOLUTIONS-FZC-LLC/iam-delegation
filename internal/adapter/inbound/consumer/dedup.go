package consumer

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

// skipDuplicate is the cheap processed_events probe every known-type
// handler runs before doing work. A hit increments
// iam_delegation_processed_events_duplicates_total (IDEMP-4) and short-circuits.
// Matching iam-realm-provisioner.
func skipDuplicate(ctx context.Context, dedup idempotencyStore, consumer, eventID string) (bool, error) {
	processed, err := dedup.IsProcessed(ctx, consumer, eventID)
	if err != nil {
		return false, err
	}
	if processed {
		if metrics.Live != nil {
			metrics.Live.RecordProcessedEventsDuplicate(consumer)
		}
		return true, nil
	}
	return false, nil
}

// ackUnknown is the O&M forward-compat path: an event type with no wired
// handler is logged, counted, and recorded in processed_events inside a
// RunInTx so redelivery does not storm the same unknown type.
func ackUnknown(ctx context.Context, tx port.TxRunner, dedup idempotencyStore, log Logger, consumer string, env events.Envelope[json.RawMessage]) error {
	if metrics.Live != nil {
		metrics.Live.RecordUnknownEventAcknowledged(consumer, env.Type)
	}
	log.Info("unknown event type — silently acknowledging", map[string]interface{}{
		"event_type": env.Type, "event_id": env.ID, "consumer": consumer,
	})
	return markProcessedInTx(ctx, tx, dedup, consumer, env.ID)
}

// markProcessedInTx records eventID on the caller's TxRunner so the dedup
// insert joins withPool's ambient transaction (or opens its own when the
// handler has no local write to be atomic with). Nil tx (unit tests) marks
// directly.
func markProcessedInTx(ctx context.Context, tx port.TxRunner, dedup idempotencyStore, consumer, eventID string) error {
	if tx == nil {
		if err := dedup.MarkProcessed(ctx, consumer, eventID); err != nil {
			return fmt.Errorf("mark processed: %w", err)
		}
		return nil
	}
	return tx.RunInTx(ctx, func(txCtx context.Context) error {
		if err := dedup.MarkProcessed(txCtx, consumer, eventID); err != nil {
			return fmt.Errorf("mark processed: %w", err)
		}
		return nil
	})
}
