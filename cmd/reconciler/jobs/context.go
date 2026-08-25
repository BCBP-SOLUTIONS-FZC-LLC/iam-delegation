// Package jobs implements the three CronJob entry points (LLD §6/§16.1):
// delegation-expiry (DLG-I1, */5 * * * *), delegation-review (DLG-I2,
// hourly, 3-day daily cascade warn then auto-end), and delegation-cleanup
// (monthly hard-purge). Per this build's DLG-D17, these are plain functions
// taking already-constructed dependencies so BOTH cmd/reconciler/main.go
// (the CronJob binary, calling them directly) and cmd/server's DLG-I1/I2
// HTTP handlers (calling them through a thin adapter) share one
// implementation.
//
// Lifted in spirit from iam-org-membership's cmd/reconciler/jobs/
// delegation_{expiry,review}.go (LLD §6 "History"), adapted for the
// review_last_warned_bucket daily-cascade warning design (DLG-D7) and the
// local delegation_tenant_settings policy read.
package jobs

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
	"github.com/google/uuid"
)

// Logger is the minimal structured-logging seam every job uses — satisfied
// by the same slog-backed adapter cmd/server and cmd/reconciler both build
// (mirrors the map[string]interface{}-based Logger shape used across this
// service's adapters).
type Logger interface {
	Info(msg string, fields map[string]interface{})
	Warn(msg string, fields map[string]interface{})
}

// BindTenantGUC binds app.tenant_id (and app.user_id) into ctx before any
// per-tenant UPDATE, per LLD §7.3/RLS-6 — the split-brain trap where a
// missing binding makes the WITH CHECK silently affect zero rows.
// Satisfied by internal/adapter/outbound/postgres.WithTenantGUC, injected
// here so this package stays free of a direct postgres import (cmd/* wires
// the concrete function).
type BindTenantGUC func(ctx context.Context, tenantID uuid.UUID, userID string) context.Context

// ProcessedEventsStore is the minimal interface needed by the cleanup job to
// purge the idempotency ledger. Satisfied by
// *consumer.ProcessedEvents (LLD §18.4, GAP-09).
type ProcessedEventsStore interface {
	CleanupExpired(ctx context.Context, olderThan time.Duration) error
}

// Metrics is the minimal recorder seam the expiry/review jobs need to close
// GAP-27 (DLG-D19 partial closure): the deferred-counter metrics must
// actually be incremented on every UP failure, not just registered.
// Satisfied by *metrics.Metrics. Optional — nil means metrics are skipped,
// so existing callers/tests that don't wire it keep working.
type Metrics interface {
	RecordExpiryDeferred()
	RecordReviewDeferred()
}

// Context carries every dependency a job needs. Delegations must be backed
// by a BYPASSRLS pool (delegation_migrator) for the cross-tenant sweep
// finder methods (ListExpiringBefore, FindDueForDailyWarn, FindDueForAutoEnd,
// HardPurgeSoftDeletedBefore) to see rows across all tenants; TxRunner must
// be backed by the RLS-scoped app pool so BindTenantGUC's per-row binding is
// actually enforced on the write (LLD §7.3 roles table).
type Context struct {
	Delegations     port.DelegationRepository
	UserProfile     port.UserProfileClient
	TxRunner        port.TxRunner
	BindTenantGUC   BindTenantGUC
	Logger          Logger
	BatchLimit      int
	RetentionDays   int                  // delegation-cleanup only (LLD §18.4, default 90)
	ProcessedEvents ProcessedEventsStore // delegation-cleanup only — purges the idempotency ledger (LLD §18.4, GAP-09); nil means skip
	Metrics         Metrics              // expiry/review deferred counters (LLD §11.4, GAP-27); nil means skip
}

// Result aggregates every job's outcome counters. Unused fields stay zero —
// delegation-expiry populates Attempted/Succeeded/Failed; delegation-review
// populates Warned3d/Warned2d/Warned1d/Expired/Deferred; delegation-cleanup
// populates Purged.
type Result struct {
	Attempted int
	Succeeded int
	Failed    int

	Warned3d int // days_remaining=3
	Warned2d int // days_remaining=2
	Warned1d int // days_remaining=1
	Expired  int
	Deferred int

	Purged int
}

const defaultBatchLimit = 50

func batchLimit(jctx *Context) int {
	if jctx.BatchLimit > 0 {
		return jctx.BatchLimit
	}
	return defaultBatchLimit
}

// systemUserID is the GUC user-id sentinel bound for every cron-origin
// write, matching the "iam-system" identity O&M's shipped cron code used.
const systemUserID = "iam-system"
