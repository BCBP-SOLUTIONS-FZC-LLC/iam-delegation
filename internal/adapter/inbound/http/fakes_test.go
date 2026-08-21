package http

import (
	"context"
	"io"
	"net/http/httptest"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/pkg/requestctx"
)

// newRequestWithIdentity builds a gin test context whose request already
// carries rc as its bound requestctx.Context (bypassing gincommon's real
// ContextMiddleware entirely) so handler-level tests can drive
// identityOrError/requireSelfOrAdmin/RequireAnyRole against a specific
// identity without standing up the full gincommon middleware stack. rc==nil
// leaves the context unbound, exercising the "missing identity" branch.
func newRequestWithIdentity(method, path string, body io.Reader, rc *requestctx.Context) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(method, path, body)
	if rc != nil {
		req = req.WithContext(requestctx.WithContext(req.Context(), rc))
	}
	c.Request = req
	return c, w
}

// setPathParam sets a single gin route param — needed because tests here
// call handlers directly rather than through gin's router, so path params
// like :id are never populated by URL matching.
func setPathParam(c *gin.Context, name, value string) {
	c.Params = append(c.Params, gin.Param{Key: name, Value: value})
}

// fakeDelegationService is a hand-written fake satisfying the local
// DelegationService interface — no dependency on the real service's port
// implementations (repository, membership-check/User Profile clients,
// idempotency store, cache, tx runner).
type fakeDelegationService struct {
	listFn     func(ctx context.Context, tenantID, delegatorID uuid.UUID) ([]domain.Delegation, error)
	createFn   func(ctx context.Context, tenantID, delegatorID uuid.UUID, idempotencyKey string, req service.CreateInput) (*domain.Delegation, error)
	cancelFn   func(ctx context.Context, tenantID, id uuid.UUID, expectedVersion int64) (*domain.Delegation, error)
	extendFn   func(ctx context.Context, tenantID, id uuid.UUID, extendDays *int, expectedVersion int64) (*domain.Delegation, error)
	reassignFn func(ctx context.Context, tenantID, id uuid.UUID, expectedVersion int64, req service.ReassignInput) (*domain.Delegation, error)
}

func (f *fakeDelegationService) List(ctx context.Context, tenantID, delegatorID uuid.UUID) ([]domain.Delegation, error) {
	if f.listFn != nil {
		return f.listFn(ctx, tenantID, delegatorID)
	}
	return nil, nil
}

func (f *fakeDelegationService) Create(ctx context.Context, tenantID, delegatorID uuid.UUID, idempotencyKey string, req service.CreateInput) (*domain.Delegation, error) {
	if f.createFn != nil {
		return f.createFn(ctx, tenantID, delegatorID, idempotencyKey, req)
	}
	return &domain.Delegation{}, nil
}

func (f *fakeDelegationService) Cancel(ctx context.Context, tenantID, id uuid.UUID, expectedVersion int64) (*domain.Delegation, error) {
	if f.cancelFn != nil {
		return f.cancelFn(ctx, tenantID, id, expectedVersion)
	}
	return &domain.Delegation{}, nil
}

func (f *fakeDelegationService) Extend(ctx context.Context, tenantID, id uuid.UUID, extendDays *int, expectedVersion int64) (*domain.Delegation, error) {
	if f.extendFn != nil {
		return f.extendFn(ctx, tenantID, id, extendDays, expectedVersion)
	}
	return &domain.Delegation{}, nil
}

func (f *fakeDelegationService) Reassign(ctx context.Context, tenantID, id uuid.UUID, expectedVersion int64, req service.ReassignInput) (*domain.Delegation, error) {
	if f.reassignFn != nil {
		return f.reassignFn(ctx, tenantID, id, expectedVersion, req)
	}
	return &domain.Delegation{}, nil
}

// fakeDelegationReader satisfies DelegationReader (authz.go).
type fakeDelegationReader struct {
	findByIDFn               func(ctx context.Context, tenantID, id uuid.UUID) (*domain.Delegation, error)
	findActiveDeptDelegateFn func(ctx context.Context, tenantID, userID, deptID uuid.UUID) (*domain.Delegation, error)
	findActiveByDelegatorFn  func(ctx context.Context, tenantID, delegatorID uuid.UUID) ([]domain.Delegation, error)
}

func (f *fakeDelegationReader) FindByID(ctx context.Context, tenantID, id uuid.UUID) (*domain.Delegation, error) {
	if f.findByIDFn != nil {
		return f.findByIDFn(ctx, tenantID, id)
	}
	return &domain.Delegation{ID: id, TenantID: tenantID}, nil
}

func (f *fakeDelegationReader) FindActiveDeptDelegateForUser(ctx context.Context, tenantID, userID, deptID uuid.UUID) (*domain.Delegation, error) {
	if f.findActiveDeptDelegateFn != nil {
		return f.findActiveDeptDelegateFn(ctx, tenantID, userID, deptID)
	}
	return nil, nil
}

func (f *fakeDelegationReader) FindActiveByDelegator(ctx context.Context, tenantID, delegatorID uuid.UUID) ([]domain.Delegation, error) {
	if f.findActiveByDelegatorFn != nil {
		return f.findActiveByDelegatorFn(ctx, tenantID, delegatorID)
	}
	return nil, nil
}

// fakeSettingsService satisfies SettingsService.
type fakeSettingsService struct {
	getFn func(ctx context.Context, tenantID uuid.UUID) (domain.DelegationTenantSettings, error)
	setFn func(ctx context.Context, tenantID uuid.UUID, maxDurationDays, reviewWindowDays int) (domain.DelegationTenantSettings, error)
}

func (f *fakeSettingsService) Get(ctx context.Context, tenantID uuid.UUID) (domain.DelegationTenantSettings, error) {
	if f.getFn != nil {
		return f.getFn(ctx, tenantID)
	}
	return domain.DefaultDelegationTenantSettings(tenantID), nil
}

func (f *fakeSettingsService) Set(ctx context.Context, tenantID uuid.UUID, maxDurationDays, reviewWindowDays int) (domain.DelegationTenantSettings, error) {
	if f.setFn != nil {
		return f.setFn(ctx, tenantID, maxDurationDays, reviewWindowDays)
	}
	return domain.DelegationTenantSettings{TenantID: tenantID, MaxDurationDays: maxDurationDays, ReviewWindowDays: reviewWindowDays}, nil
}

// fakeExpiryRunner / fakeReviewRunner satisfy ExpiryRunner / ReviewRunner.
type fakeExpiryRunner struct {
	fn func(ctx context.Context) (attempted, succeeded, failed int, err error)
}

func (f *fakeExpiryRunner) RunExpiry(ctx context.Context) (int, int, int, error) {
	if f.fn != nil {
		return f.fn(ctx)
	}
	return 0, 0, 0, nil
}

type fakeReviewRunner struct {
	fn func(ctx context.Context) (warned7d, warned3d, expired, deferred int, err error)
}

func (f *fakeReviewRunner) RunReviewSweep(ctx context.Context) (int, int, int, int, error) {
	if f.fn != nil {
		return f.fn(ctx)
	}
	return 0, 0, 0, 0, nil
}
