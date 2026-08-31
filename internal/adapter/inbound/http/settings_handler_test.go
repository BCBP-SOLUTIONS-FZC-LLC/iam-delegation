package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/pkg/requestctx"
)

// ── Get (DLG-6) ─────────────────────────────────────────────────────────

func TestSettingsHandler_Get(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()

	t.Run("missing identity -> 401", func(t *testing.T) {
		h := NewSettingsHandler(&fakeSettingsService{})
		c, w := newRequestWithIdentity(http.MethodGet, "/api/v1/delegations/settings", nil, nil)
		h.Get(c)
		require.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("invalid tenant id -> 401", func(t *testing.T) {
		h := NewSettingsHandler(&fakeSettingsService{})
		rc := &requestctx.Context{UserID: userID.String(), TenantID: "not-a-uuid"}
		c, w := newRequestWithIdentity(http.MethodGet, "/api/v1/delegations/settings", nil, rc)
		h.Get(c)
		require.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("service error mapped", func(t *testing.T) {
		svc := &fakeSettingsService{
			getFn: func(ctx context.Context, tenantID uuid.UUID) (domain.DelegationTenantSettings, error) {
				return domain.DelegationTenantSettings{}, domain.NewError(domain.ErrDelegationNotFound, "nope")
			},
		}
		h := NewSettingsHandler(svc)
		c, w := newRequestWithIdentity(http.MethodGet, "/api/v1/delegations/settings", nil, selfRC(userID, tenantID))
		h.Get(c)
		require.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("happy path -> 200 default 90/90", func(t *testing.T) {
		h := NewSettingsHandler(&fakeSettingsService{})
		c, w := newRequestWithIdentity(http.MethodGet, "/api/v1/delegations/settings", nil, selfRC(userID, tenantID))
		h.Get(c)
		require.Equal(t, http.StatusOK, w.Code)
		var resp SettingsResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		require.Equal(t, 90, resp.MaxDurationDays)
		require.Equal(t, 90, resp.ReviewWindowDays)
	})

	// FM-HDR-11: tenant with no settings row → service returns defaults → handler returns 200
	t.Run("no settings row → service returns defaults 90/90 → 200 (FM-HDR-11)", func(t *testing.T) {
		svc := &fakeSettingsService{
			getFn: func(ctx context.Context, gotTenant uuid.UUID) (domain.DelegationTenantSettings, error) {
				// simulate settings_service returning its defaults when repo has no row
				return domain.DelegationTenantSettings{
					TenantID:         gotTenant,
					MaxDurationDays:  90,
					ReviewWindowDays: 90,
				}, nil
			},
		}
		h := NewSettingsHandler(svc)
		c, w := newRequestWithIdentity(http.MethodGet, "/api/v1/delegations/settings", nil, selfRC(userID, tenantID))
		h.Get(c)
		require.Equal(t, http.StatusOK, w.Code)
		var resp SettingsResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		require.Equal(t, 90, resp.MaxDurationDays)
		require.Equal(t, 90, resp.ReviewWindowDays)
	})
}

// ── Set (DLG-7) ─────────────────────────────────────────────────────────

func TestSettingsHandler_Set(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()

	t.Run("missing identity -> 401", func(t *testing.T) {
		h := NewSettingsHandler(&fakeSettingsService{})
		c, w := newRequestWithIdentity(http.MethodPut, "/api/v1/delegations/settings", bytes.NewReader([]byte(`{}`)), nil)
		h.Set(c)
		require.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("malformed JSON body -> 400", func(t *testing.T) {
		h := NewSettingsHandler(&fakeSettingsService{})
		c, w := newRequestWithIdentity(http.MethodPut, "/api/v1/delegations/settings", bytes.NewReader([]byte(`{bad`)), adminRC(userID, tenantID))
		h.Set(c)
		require.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("service validation error mapped (max_duration_days out of range)", func(t *testing.T) {
		svc := &fakeSettingsService{
			setFn: func(ctx context.Context, tenantID uuid.UUID, maxDurationDays, reviewWindowDays int) (domain.DelegationTenantSettings, error) {
				return domain.DelegationTenantSettings{}, domain.NewError(domain.ErrInvalidDelegationMaxDurationDays, "bad")
			},
		}
		h := NewSettingsHandler(svc)
		body, _ := json.Marshal(SettingsSetRequest{MaxDurationDays: 999, ReviewWindowDays: 30})
		c, w := newRequestWithIdentity(http.MethodPut, "/api/v1/delegations/settings", bytes.NewReader(body), adminRC(userID, tenantID))
		h.Set(c)
		require.Equal(t, http.StatusBadRequest, w.Code)
	})

	// FM-HDR-09: missing review_window_days (int defaults to 0) → service rejects → 400
	t.Run("zero review_window_days → 400 invalid_delegation_review_window_days (FM-HDR-09)", func(t *testing.T) {
		svc := &fakeSettingsService{
			setFn: func(ctx context.Context, gotTenant uuid.UUID, maxDurationDays, reviewWindowDays int) (domain.DelegationTenantSettings, error) {
				return domain.DelegationTenantSettings{}, domain.NewError(domain.ErrInvalidDelegationReviewWindow, "review_window_days must be >= 1")
			},
		}
		h := NewSettingsHandler(svc)
		// body only has max_duration_days; review_window_days absent → defaults to 0
		body, _ := json.Marshal(SettingsSetRequest{MaxDurationDays: 60, ReviewWindowDays: 0})
		c, w := newRequestWithIdentity(http.MethodPut, "/api/v1/delegations/settings", bytes.NewReader(body), adminRC(userID, tenantID))
		h.Set(c)
		require.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("happy path -> 200", func(t *testing.T) {
		var gotMax, gotReview int
		svc := &fakeSettingsService{
			setFn: func(ctx context.Context, gotTenant uuid.UUID, maxDurationDays, reviewWindowDays int) (domain.DelegationTenantSettings, error) {
				require.Equal(t, tenantID, gotTenant)
				gotMax, gotReview = maxDurationDays, reviewWindowDays
				return domain.DelegationTenantSettings{TenantID: gotTenant, MaxDurationDays: maxDurationDays, ReviewWindowDays: reviewWindowDays, RecordVersion: 2}, nil
			},
		}
		h := NewSettingsHandler(svc)
		body, _ := json.Marshal(SettingsSetRequest{MaxDurationDays: 60, ReviewWindowDays: 45})
		c, w := newRequestWithIdentity(http.MethodPut, "/api/v1/delegations/settings", bytes.NewReader(body), adminRC(userID, tenantID))
		h.Set(c)
		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, 60, gotMax)
		require.Equal(t, 45, gotReview)

		var resp SettingsResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		require.Equal(t, 60, resp.MaxDurationDays)
		require.Equal(t, 45, resp.ReviewWindowDays)
		require.Equal(t, int64(2), resp.RecordVersion)
	})
}

func TestSettingsHandler_Set_InvalidTenantID_Returns401(t *testing.T) {
	h := NewSettingsHandler(&fakeSettingsService{})
	c, w := newRequestWithIdentity(http.MethodPut, "/api/v1/delegations/settings",
		bytes.NewReader([]byte(`{"max_duration_days":90,"review_window_days":30}`)),
		&requestctx.Context{TenantID: "not-a-uuid", UserID: uuid.New().String(), Roles: []string{"tenant_admin"}})
	h.Set(c)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}
