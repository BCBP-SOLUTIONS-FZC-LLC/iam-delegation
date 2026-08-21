package http

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
)

// TestRequireSelfOrAdmin_MissingIdentity covers the branch unreachable via
// the public handlers (they all call identityOrError first, which already
// gates on missing identity before requireSelfOrAdmin runs) — a direct call
// exercises authz.go's own defensive check.
func TestRequireSelfOrAdmin_MissingIdentity(t *testing.T) {
	c, w := newRequestWithIdentity(http.MethodDelete, "/x", nil, nil)
	d, ok := requireSelfOrAdmin(c, &fakeDelegationReader{}, uuid.New(), uuid.New())
	require.False(t, ok)
	require.Nil(t, d)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRequireSelfOrAdmin_ReaderErrorMapped(t *testing.T) {
	reader := &fakeDelegationReader{
		findByIDFn: func(ctx context.Context, tenantID, id uuid.UUID) (*domain.Delegation, error) {
			return nil, domain.NewError(domain.ErrDelegationNotFound, "gone")
		},
	}
	c, w := newRequestWithIdentity(http.MethodDelete, "/x", nil, selfRC(uuid.New(), uuid.New()))
	_, ok := requireSelfOrAdmin(c, reader, uuid.New(), uuid.New())
	require.False(t, ok)
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestRequireSelfOrAdmin_Self(t *testing.T) {
	tenantID, delegatorID, id := uuid.New(), uuid.New(), uuid.New()
	reader := &fakeDelegationReader{
		findByIDFn: func(ctx context.Context, gotTenant, gotID uuid.UUID) (*domain.Delegation, error) {
			return &domain.Delegation{ID: gotID, TenantID: gotTenant, DelegatorID: delegatorID}, nil
		},
	}
	c, w := newRequestWithIdentity(http.MethodDelete, "/x", nil, selfRC(delegatorID, tenantID))
	d, ok := requireSelfOrAdmin(c, reader, tenantID, id)
	require.True(t, ok)
	require.NotNil(t, d)
	require.Equal(t, http.StatusOK, w.Code) // untouched — no error written
}

func TestRequireSelfOrAdmin_Admin(t *testing.T) {
	tenantID, delegatorID, id, adminID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	reader := &fakeDelegationReader{
		findByIDFn: func(ctx context.Context, gotTenant, gotID uuid.UUID) (*domain.Delegation, error) {
			return &domain.Delegation{ID: gotID, TenantID: gotTenant, DelegatorID: delegatorID}, nil
		},
	}
	c, w := newRequestWithIdentity(http.MethodDelete, "/x", nil, adminRC(adminID, tenantID))
	d, ok := requireSelfOrAdmin(c, reader, tenantID, id)
	require.True(t, ok)
	require.NotNil(t, d)
	require.Equal(t, http.StatusOK, w.Code)
}

func TestRequireSelfOrAdmin_NeitherSelfNorAdmin(t *testing.T) {
	tenantID, delegatorID, id, otherID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	reader := &fakeDelegationReader{
		findByIDFn: func(ctx context.Context, gotTenant, gotID uuid.UUID) (*domain.Delegation, error) {
			return &domain.Delegation{ID: gotID, TenantID: gotTenant, DelegatorID: delegatorID}, nil
		},
	}
	c, w := newRequestWithIdentity(http.MethodDelete, "/x", nil, selfRC(otherID, tenantID))
	d, ok := requireSelfOrAdmin(c, reader, tenantID, id)
	require.False(t, ok)
	require.Nil(t, d)
	require.Equal(t, http.StatusForbidden, w.Code)
}
