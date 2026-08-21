package port

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/google/uuid"
)

// Cache is the advisory del:list: read-through cache for DLG-1 (LLD §9).
// Correctness never depends on it — a miss or a down cache always falls
// through to Postgres (§9.3). No error returns: implementations log
// internally and degrade to a cache miss.
type Cache interface {
	GetDelegatorList(ctx context.Context, tenantID, delegatorID uuid.UUID) ([]domain.Delegation, bool)
	SetDelegatorList(ctx context.Context, tenantID, delegatorID uuid.UUID, list []domain.Delegation)
	InvalidateDelegatorList(ctx context.Context, tenantID, delegatorID uuid.UUID)
}
