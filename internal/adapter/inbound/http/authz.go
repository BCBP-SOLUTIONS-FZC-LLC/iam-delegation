package http

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/pkg/requestctx"
)

// DelegationReader is the minimal read-only seam this adapter needs beyond
// what DelegationService exposes:
//
//   - DLG-3/4/5 (Cancel/Extend/Reassign)'s "self or tenant_admin/tenant_owner"
//     authorization rule (LLD §8.2/§13.4) needs the target row's
//     delegator_id BEFORE the mutation is attempted — this repo's
//     DelegationService methods take no actor parameter (they trust the
//     caller to have already authorized), so requireSelfOrAdmin reads the
//     record here first via FindByID. This is an ordinary same-service DB
//     read, not a "new cross-service call" (§13.4's table already says
//     "No" for that column on this row).
//   - DLG-I3/DLG-I4 are pure reads with no business logic of their own
//     (FindActiveDeptDelegateForUser / FindActiveByDelegator).
//
// Satisfied directly by the concrete postgres port.DelegationRepository
// injected from cmd/server — declared structurally here so this package
// never needs to import internal/core/port.
type DelegationReader interface {
	FindByID(ctx context.Context, tenantID, id uuid.UUID) (*domain.Delegation, error)
	FindActiveDeptDelegateForUser(ctx context.Context, tenantID, userID, deptID uuid.UUID) (*domain.Delegation, error)
	FindActiveByDelegator(ctx context.Context, tenantID, delegatorID uuid.UUID) ([]domain.Delegation, error)
}

// requireSelfOrAdmin enforces DLG-3/4/5's "self or admin" rule. On success
// it returns the delegation it read (so the caller doesn't re-fetch it) and
// true; on failure it has already written the error response and the
// caller must return immediately.
func requireSelfOrAdmin(c *gin.Context, reader DelegationReader, tenantID, id uuid.UUID) (*domain.Delegation, bool) {
	rc, ok := requestctx.FromContext(c.Request.Context())
	if !ok {
		HandleError(c, domain.NewError(domain.ErrMissingIdentity, "missing identity headers"))
		return nil, false
	}
	d, err := reader.FindByID(c.Request.Context(), tenantID, id)
	if err != nil {
		HandleError(c, err)
		return nil, false
	}
	if d.DelegatorID.String() != rc.UserID && !rc.IsAdmin() {
		HandleError(c, domain.NewError(domain.ErrInsufficientRole, "caller is neither the delegator nor a tenant admin"))
		return nil, false
	}
	return d, true
}
