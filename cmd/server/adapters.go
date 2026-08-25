package main

import (
	"context"
	"errors"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/cmd/reconciler/jobs"
	httpadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/inbound/http"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/outbox"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// gucBoundReader wraps *postgres.DelegationRepository for DLG-I3/I4
// (mesh-only internal routes with no gateway identity to bridge, so no
// per-request middleware binds a tenant GUC the way the public group's
// tenantGUCMiddleware does). Each call binds app.tenant_id itself from the
// tenantID argument the caller already has, immediately before the query —
// correct here because these are read-only, single-statement calls, not a
// multi-statement transaction opened earlier by a TxRunner (LLD §7.3/RLS-6).
type gucBoundReader struct {
	repo *pgadapter.DelegationRepository
}

func (r gucBoundReader) FindByID(ctx context.Context, tenantID, id uuid.UUID) (*domain.Delegation, error) {
	return r.repo.FindByID(pgadapter.WithTenantGUC(ctx, tenantID, "iam-system"), tenantID, id)
}

func (r gucBoundReader) FindActiveDeptDelegateForUser(ctx context.Context, tenantID, userID, deptID uuid.UUID) (*domain.Delegation, error) {
	return r.repo.FindActiveDeptDelegateForUser(pgadapter.WithTenantGUC(ctx, tenantID, "iam-system"), tenantID, userID, deptID)
}

func (r gucBoundReader) FindActiveByDelegator(ctx context.Context, tenantID, delegatorID uuid.UUID) ([]domain.Delegation, error) {
	return r.repo.FindActiveByDelegator(pgadapter.WithTenantGUC(ctx, tenantID, "iam-system"), tenantID, delegatorID)
}

// reconcilerRunner adapts cmd/reconciler/jobs' Expiry/ReviewSweep functions
// to the httpadapter.ExpiryRunner/ReviewRunner interfaces so DLG-I1/I2's
// HTTP entry points and the CronJob binary share one implementation
// (DLG-D17) — see cmd/reconciler/jobs's package doc.
type reconcilerRunner struct {
	jctx *jobs.Context
}

func (r reconcilerRunner) RunExpiry(ctx context.Context) (attempted, succeeded, failed int, err error) {
	res, err := jobs.Expiry(ctx, r.jctx)
	return res.Attempted, res.Succeeded, res.Failed, err
}

func (r reconcilerRunner) RunReviewSweep(ctx context.Context) (httpadapter.ReviewSweepResult, error) {
	res, err := jobs.ReviewSweep(ctx, r.jctx)
	return httpadapter.ReviewSweepResult{
		Warned3d: res.Warned3d, Warned2d: res.Warned2d, Warned1d: res.Warned1d,
		Expired: res.Expired, Deferred: res.Deferred, Failed: res.Failed,
	}, err
}

// redisPinger adapts *redis.Client to the httpadapter.Pinger shape
// (Ping(ctx) error) for /readyz's Valkey check.
type redisPinger struct{ client *redis.Client }

func (p redisPinger) Ping(ctx context.Context) error {
	return p.client.Ping(ctx).Err()
}

// outboxPinger adapts *outbox.Runner to httpadapter.Pinger — Ready() closes
// once the runner has completed its first successful poll cycle;
// non-blocking select reports "not ready yet" without blocking /readyz.
type outboxPinger struct{ runner *outbox.Runner }

func (p outboxPinger) Ping(_ context.Context) error {
	select {
	case <-p.runner.Ready():
		return nil
	default:
		return errors.New("outbox runner not ready")
	}
}
