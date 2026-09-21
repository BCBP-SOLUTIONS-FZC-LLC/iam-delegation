package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	gincommon "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	pgcommon "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

func testGinConfig() gincommon.Config {
	return gincommon.Config{Logger: testLogger(), ServiceName: "iam-delegation"}
}

// fakeLogger is a no-op Logger for tests that need to pass one to NewRouter
// but don't assert on log output.
type fakeLogger struct{}

func (fakeLogger) Debug(string, map[string]interface{}) {}
func (fakeLogger) Info(string, map[string]interface{})  {}
func (fakeLogger) Warn(string, map[string]interface{})  {}
func (fakeLogger) Error(string, map[string]interface{}) {}

func testLogger() fakeLogger {
	return fakeLogger{}
}

func newTestRouter(docs DocsConfig) (*Router, *fakeDelegationService, *fakeDelegationReader, *fakeSettingsService) {
	svc := &fakeDelegationService{}
	reader := &fakeDelegationReader{}
	settingsSvc := &fakeSettingsService{}
	delegationHandler := NewDelegationHandler(svc, reader)
	settingsHandler := NewSettingsHandler(settingsSvc)
	internalHandler := NewInternalHandler(&fakeExpiryRunner{}, &fakeReviewRunner{}, reader)
	r := NewRouter(delegationHandler, settingsHandler, internalHandler,
		fakePostgresHealth{status: pgcommon.HealthStatus{Healthy: true}}, nil, fakePinger{}, fakePinger{},
		testGinConfig(), docs, nil)
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
		fakePostgresHealth{status: pgcommon.HealthStatus{Healthy: true}}, nil, fakePinger{}, fakePinger{},
		testGinConfig(), DocsConfig{Environment: "development"}, bind)

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

func TestRouter_Docs_SwaggerAssetOverrides(t *testing.T) {
	r, _, _, _ := newTestRouter(DocsConfig{Environment: "development"})

	req := httptest.NewRequest(http.MethodGet, "/swagger/index.css", nil)
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Header().Get("Content-Type"), "text/css")

	req = httptest.NewRequest(http.MethodGet, "/swagger/swagger-initializer.js", nil)
	w = httptest.NewRecorder()
	r.Handler().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Header().Get("Content-Type"), "javascript")
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

// TestNewRouter_EmptyServiceName covers lines 98–100: when ginCfg.ServiceName
// is empty, NewRouter defaults it to "iam-delegation" before passing it on.
func TestNewRouter_EmptyServiceName(t *testing.T) {
	svc := &fakeDelegationService{}
	reader := &fakeDelegationReader{}
	settingsSvc := &fakeSettingsService{}
	delegationHandler := NewDelegationHandler(svc, reader)
	settingsHandler := NewSettingsHandler(settingsSvc)
	internalHandler := NewInternalHandler(&fakeExpiryRunner{}, &fakeReviewRunner{}, reader)

	// Pass an empty ServiceName — the router must fill it in and not panic.
	r := NewRouter(delegationHandler, settingsHandler, internalHandler,
		fakePostgresHealth{status: pgcommon.HealthStatus{Healthy: true}}, nil, fakePinger{}, fakePinger{},
		gincommon.Config{Logger: testLogger()}, DocsConfig{}, nil)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
}

// TestRouter_Swagger_DefaultRoute covers lines 172–173: the wildcard swagger
// handler's default branch, reached by any path that isn't .css or
// swagger-initializer.js (e.g. /swagger/index.html).
func TestRouter_Swagger_DefaultRoute(t *testing.T) {
	r, _, _, _ := newTestRouter(DocsConfig{})
	req := httptest.NewRequest(http.MethodGet, "/swagger/index.html", nil)
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
}
