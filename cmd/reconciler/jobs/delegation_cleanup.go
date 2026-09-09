package jobs

import (
	"context"
	"time"
)

// defaultRetentionDays is the LLD §18.4 soft-delete retention window before
// a hard purge.
const defaultRetentionDays = 90

// processedEventsTTLDays is the LLD §18.4 default retention window for the
// processed_events idempotency ledger (GAP-09), used when
// Context.ProcessedEventsTTLDays is unset. Override via
// PROCESSED_EVENTS_TTL_DAYS (iam-realm-provisioner uses 8 with a dedicated
// CronJob; this service keeps the LLD's 30-day window and bundles prune
// into monthly delegation-cleanup).
const defaultProcessedEventsTTLDays = 30

// Cleanup is the monthly delegation-cleanup job (0 4 1 * *) — hard-purges
// delegations soft-deleted more than RetentionDays ago (LLD §18.4), and
// purges the processed_events idempotency ledger older than
// ProcessedEventsTTLDays (default 30, GAP-09) via bounded Prune
// (iam-realm-provisioner). No event emission
// (purely a storage-retention concern), no GUC binding needed
// (HardPurgeSoftDeletedBefore is a cross-tenant DELETE against the
// BYPASSRLS pool, like the other cron finder queries).
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

	// GAP-09: prune the processed_events idempotency ledger (LLD §18.4)
	// with a LIMIT so a large backlog cannot lock the table unbounded.
	if jctx.ProcessedEvents != nil {
		ttl := jctx.ProcessedEventsTTLDays
		if ttl <= 0 {
			ttl = defaultProcessedEventsTTLDays
		}
		if _, err := jctx.ProcessedEvents.Prune(ctx, ttl, batchLimit(jctx)); err != nil {
			jctx.Logger.Warn("delegation-cleanup: processed_events prune failed",
				map[string]interface{}{"error": err.Error()})
			// non-fatal: log and continue — delegation purge already succeeded
		}
	}

	jctx.Logger.Info("delegation-cleanup complete", map[string]interface{}{"purged": purged, "retention_days": retention})
	return res, nil
}
