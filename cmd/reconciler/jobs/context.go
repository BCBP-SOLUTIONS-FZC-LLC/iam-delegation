// Package jobs implements the three CronJob entry points (LLD §6/§16.1):
// delegation-expiry (DLG-I1, */5 * * * *), delegation-review (DLG-I2,
// hourly, dual 7d/3d warn then auto-end), and delegation-cleanup (monthly
// hard-purge). Per this build's DLG-D17, these are plain functions taking
// already-constructed dependencies so BOTH cmd/reconciler/main.go (the
// CronJob binary, calling them directly) and cmd/server's DLG-I1/I2 HTTP
// handlers (calling them through a thin adapter) share one implementation.
//
// Lifted in spirit from iam-org-membership's cmd/reconciler/jobs/
// delegation_{expiry,review}.go (LLD §6 "History"), adapted for the dual
// review_last_warned_bucket warning design (DLG-D7) and the local
// delegation_tenant_settings policy read.
package jobs

import (
	"context"

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

// Context carries every dependency a job needs. Delegations must be backed
// by a BYPASSRLS pool (delegation_migrator) for the cross-tenant sweep
// finder methods (ListExpiringBefore, FindDueForWarning7d/3d, FindDueForAutoEnd,
// HardPurgeSoftDeletedBefore) to see rows across all tenants; TxRunner must
// be backed by the RLS-scoped app pool so BindTenantGUC's per-row binding is
// actually enforced on the write (LLD §7.3 roles table).
type Context struct {
	Delegations   port.DelegationRepository
	UserProfile   port.UserProfileClient
	TxRunner      port.TxRunner
	BindTenantGUC BindTenantGUC
	Logger        Logger
	BatchLimit    int
	RetentionDays int // delegation-cleanup only (LLD §18.4, default 90)
}

// Result aggregates every job's outcome counters. Unused fields stay zero —
// delegation-expiry populates Attempted/Succeeded/Failed; delegation-review
// populates Warned7d/Warned3d/Expired/Deferred; delegation-cleanup
// populates Purged.
type Result struct {
	Attempted int
	Succeeded int
	Failed    int

	Warned7d int
	Warned3d int
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
