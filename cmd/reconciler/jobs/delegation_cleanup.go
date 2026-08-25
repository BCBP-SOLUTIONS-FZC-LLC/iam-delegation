package jobs

import (
	"context"
	"time"
)

// defaultRetentionDays is the LLD §18.4 soft-delete retention window before
// a hard purge.
const defaultRetentionDays = 90

// processedEventsRetention is the LLD §18.4 retention window for the
// processed_events idempotency ledger (GAP-09).
const processedEventsRetention = 30 * 24 * time.Hour

// Cleanup is the monthly delegation-cleanup job (0 4 1 * *) — hard-purges
// delegations soft-deleted more than RetentionDays ago (LLD §18.4), and
// purges the processed_events idempotency ledger older than 30 days
// (GAP-09). No event emission (purely a storage-retention concern), no GUC
// binding needed (HardPurgeSoftDeletedBefore is a cross-tenant DELETE
// against the BYPASSRLS pool, like the other cron finder queries).
func Cleanup(ctx context.Context, jctx *Context) (Result, error) {
	var res Result
	retention := jctx.RetentionDays
	if retention <= 0 {
		retention = defaultRetentionDays
	}
	before := time.Now().UTC().AddDate(0, 0, -retention)
	purged, err := jctx.Delegations.HardPurgeSoftDeletedBefore(ctx, before, batchLimit(jctx))
	if err != nil {
		return res, err
	}
	res.Purged = purged

	// GAP-09: purge the processed_events idempotency ledger (LLD §18.4).
	if jctx.ProcessedEvents != nil {
		if err := jctx.ProcessedEvents.CleanupExpired(ctx, processedEventsRetention); err != nil {
			jctx.Logger.Warn("delegation-cleanup: processed_events purge failed",
				map[string]interface{}{"error": err.Error()})
			// non-fatal: log and continue — delegation purge already succeeded
		}
	}

	jctx.Logger.Info("delegation-cleanup complete", map[string]interface{}{"purged": purged, "retention_days": retention})
	return res, nil
}
