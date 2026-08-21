package http

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	pgcommon "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestRouter(docs DocsConfig) (*Router, *fakeDelegationService, *fakeDelegationReader, *fakeSettingsService) {
	svc := &fakeDelegationService{}
	reader := &fakeDelegationReader{}
	settingsSvc := &fakeSettingsService{}
	delegationHandler := NewDelegationHandler(svc, reader)
	settingsHandler := NewSettingsHandler(settingsSvc)
	internalHandler := NewInternalHandler(&fakeExpiryRunner{}, &fakeReviewRunner{}, reader)
	r := NewRouter(delegationHandler, settingsHandler, internalHandler,
		fakePostgresHealth{status: pgcommon.HealthStatus{Healthy: true}}, fakePinger{}, fakePinger{},
		testLogger(), nil, docs, nil)
	return r, svc, reader, settingsSvc
}

// TestRouter_BindTenantGUC verifies tenantGUCMiddleware is wired in and
// invoked with the bridged identity when NewRouter is given a non-nil
// BindTenantGUC (router.go's RLS-6 GUC-bind seam).
func TestRouter_BindTenantGUC(t *testing.T) {
	svc := &fakeDelegationService{}
	reader := &fakeDelegationReader{}
	settingsSvc := &fakeSettingsService{}
	delegationHandler := NewDelegationHandler(svc, reader)
	settingsHandler := NewSettingsHandler(settingsSvc)
	internalHandler := NewInternalHandler(&fakeExpiryRunner{}, &fakeReviewRunner{}, reader)

	tenantID, userID := uuid.New(), uuid.New()
	var gotTenant, gotUser string
	bind := func(ctx context.Context, tenantID, userID string) context.Context {
		gotTenant, gotUser = tenantID, userID
		return ctx
	}

	r := NewRouter(delegationHandler, settingsHandler, internalHandler,
		fakePostgresHealth{status: pgcommon.HealthStatus{Healthy: true}}, fakePinger{}, fakePinger{},
		testLogger(), nil, DocsConfig{Environment: "development"}, bind)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/delegations", nil)
	req.Header.Set("x-user-id", userID.String())
	req.Header.Set("x-tenant-id", tenantID.String())
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, tenantID.String(), gotTenant)
	require.Equal(t, userID.String(), gotUser)
}

// TestTenantGUCMiddleware_NoIdentity covers the "no requestctx bound" branch
// — bind must not be called when ContextMiddleware never ran.
func TestTenantGUCMiddleware_NoIdentity(t *testing.T) {
	called := false
	bind := func(ctx context.Context, tenantID, userID string) context.Context {
		called = true
		return ctx
	}
	mw := tenantGUCMiddleware(bind)
	c, w := newRequestWithIdentity(http.MethodGet, "/x", nil, nil)
	mw(c)
	require.False(t, called)
	require.False(t, c.IsAborted())
	require.Equal(t, http.StatusOK, w.Code)
}

// TestSlogPlatformLogger exercises every method of the small adapter that
// bridges *slog.Logger onto platform-gincommon's/platform-events' Logger
// interface shape.
func TestSlogPlatformLogger(t *testing.T) {
	a := slogPlatformLogger{l: testLogger()}
	fields := map[string]interface{}{"k": "v"}
	a.Debug("debug", fields)
	a.Info("info", fields)
	a.Warn("warn", fields)
	a.Error("error", fields)
}

func TestDocsConfig_Active(t *testing.T) {
	require.True(t, DocsConfig{Environment: "development"}.active())
	require.True(t, DocsConfig{Environment: ""}.active())
	require.False(t, DocsConfig{Environment: "production", Enabled: false}.active())
	require.True(t, DocsConfig{Environment: "production", Enabled: true}.active())
}

func TestRouter_HealthAndReady(t *testing.T) {
	r, _, _, _ := newTestRouter(DocsConfig{Environment: "development"})

	t.Run("healthz", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		w := httptest.NewRecorder()
		r.Handler().ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		require.Equal(t, "ok", body["status"])
	})

	t.Run("readyz", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		w := httptest.NewRecorder()
		r.Handler().ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
	})
}

func TestRouter_PublicRoutes_AuthAndDispatch(t *testing.T) {
	r, svc, _, _ := newTestRouter(DocsConfig{Environment: "development"})

	t.Run("GET /api/v1/delegations without identity -> 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/delegations", nil)
		w := httptest.NewRecorder()
		r.Handler().ServeHTTP(w, req)
		require.Equal(t, http.StatusUnauthorized, w.Code)
	})

	tenantID, userID := uuid.New(), uuid.New()

	t.Run("GET /api/v1/delegations with identity -> 200 via full stack", func(t *testing.T) {
		svc.listFn = func(ctx context.Context, gotTenant, gotDelegator uuid.UUID) ([]domain.Delegation, error) {
			return nil, nil
		}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/delegations", nil)
		req.Header.Set("x-user-id", userID.String())
		req.Header.Set("x-tenant-id", tenantID.String())
		w := httptest.NewRecorder()
		r.Handler().ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("POST /api/v1/delegations without Idempotency-Key -> 400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/delegations", nil)
		req.Header.Set("x-user-id", userID.String())
		req.Header.Set("x-tenant-id", tenantID.String())
		w := httptest.NewRecorder()
		r.Handler().ServeHTTP(w, req)
		require.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("PUT /api/v1/delegations/settings without admin role -> 403", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/delegations/settings", nil)
		req.Header.Set("x-user-id", userID.String())
		req.Header.Set("x-tenant-id", tenantID.String())
		req.Header.Set("x-tenant-roles", "tenant_member")
		w := httptest.NewRecorder()
		r.Handler().ServeHTTP(w, req)
		require.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("GET /api/v1/delegations/settings any tenant member -> 200", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/delegations/settings", nil)
		req.Header.Set("x-user-id", userID.String())
		req.Header.Set("x-tenant-id", tenantID.String())
		w := httptest.NewRecorder()
		r.Handler().ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
	})
}

func TestRouter_InternalRoutes_NoAuthRequired(t *testing.T) {
	r, _, _, _ := newTestRouter(DocsConfig{Environment: "development"})

	req := httptest.NewRequest(http.MethodPost, "/internal/delegations/expire", nil)
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	req = httptest.NewRequest(http.MethodPost, "/internal/delegations/review-sweep", nil)
	w = httptest.NewRecorder()
	r.Handler().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
}

func TestRouter_Docs_NonProduction(t *testing.T) {
	r, _, _, _ := newTestRouter(DocsConfig{Environment: "development"})

	req := httptest.NewRequest(http.MethodGet, "/asyncapi", nil)
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	req = httptest.NewRequest(http.MethodGet, "/asyncapi.yaml", nil)
	w = httptest.NewRecorder()
	r.Handler().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
}

func TestRouter_Docs_ProductionDisabled(t *testing.T) {
	r, _, _, _ := newTestRouter(DocsConfig{Environment: "production", Enabled: false})

	req := httptest.NewRequest(http.MethodGet, "/asyncapi", nil)
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, req)
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestRouter_Docs_ProductionEnabledWithToken(t *testing.T) {
	r, _, _, _ := newTestRouter(DocsConfig{Environment: "production", Enabled: true, AuthToken: "secret-token"})

	t.Run("missing bearer -> 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/asyncapi", nil)
		w := httptest.NewRecorder()
		r.Handler().ServeHTTP(w, req)
		require.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("wrong bearer -> 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/asyncapi", nil)
		req.Header.Set("Authorization", "Bearer wrong")
		w := httptest.NewRecorder()
		r.Handler().ServeHTTP(w, req)
		require.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("correct bearer -> 200", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/asyncapi", nil)
		req.Header.Set("Authorization", "Bearer secret-token")
		w := httptest.NewRecorder()
		r.Handler().ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("swagger also gated", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/swagger/index.html", nil)
		w := httptest.NewRecorder()
		r.Handler().ServeHTTP(w, req)
		require.Equal(t, http.StatusUnauthorized, w.Code)
	})
}
