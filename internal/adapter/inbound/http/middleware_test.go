package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/pkg/requestctx"
	gincommon "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

// ── RequireAnyRole ──────────────────────────────────────────────────────

func TestRequireAnyRole(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()

	t.Run("missing identity -> 401", func(t *testing.T) {
		mw := RequireAnyRole("tenant_admin", "tenant_owner")
		c, w := newRequestWithIdentity(http.MethodPut, "/x", nil, nil)
		mw(c)
		require.Equal(t, http.StatusUnauthorized, w.Code)
		require.True(t, c.IsAborted())
	})

	t.Run("caller lacks required role -> 403 (DLG-7 has no self concept)", func(t *testing.T) {
		mw := RequireAnyRole("tenant_admin", "tenant_owner")
		rc := &requestctx.Context{UserID: userID.String(), TenantID: tenantID.String(), Roles: []string{"tenant_member"}}
		c, w := newRequestWithIdentity(http.MethodPut, "/x", nil, rc)
		mw(c)
		require.Equal(t, http.StatusForbidden, w.Code)
		require.True(t, c.IsAborted())
	})

	t.Run("tenant_admin passes", func(t *testing.T) {
		mw := RequireAnyRole("tenant_admin", "tenant_owner")
		c, w := newRequestWithIdentity(http.MethodPut, "/x", nil, adminRC(userID, tenantID))
		mw(c)
		require.False(t, c.IsAborted())
		require.Equal(t, http.StatusOK, w.Code) // untouched recorder default
	})

	t.Run("tenant_owner passes", func(t *testing.T) {
		mw := RequireAnyRole("tenant_admin", "tenant_owner")
		rc := &requestctx.Context{UserID: userID.String(), TenantID: tenantID.String(), Roles: []string{"tenant_owner"}}
		c, w := newRequestWithIdentity(http.MethodPut, "/x", nil, rc)
		mw(c)
		require.False(t, c.IsAborted())
		require.Equal(t, http.StatusOK, w.Code)
	})
}

// ── requireIdempotencyKey ───────────────────────────────────────────────

func TestRequireIdempotencyKey(t *testing.T) {
	t.Run("missing header -> 400", func(t *testing.T) {
		mw := requireIdempotencyKey()
		c, w := newRequestWithIdentity(http.MethodPost, "/api/v1/delegations", nil, nil)
		mw(c)
		require.Equal(t, http.StatusBadRequest, w.Code)
		require.True(t, c.IsAborted())
	})

	t.Run("present header -> passes", func(t *testing.T) {
		mw := requireIdempotencyKey()
		c, w := newRequestWithIdentity(http.MethodPost, "/api/v1/delegations", nil, nil)
		c.Request.Header.Set("Idempotency-Key", "abc")
		mw(c)
		require.False(t, c.IsAborted())
		require.Equal(t, http.StatusOK, w.Code)
	})
}

// ── ContextMiddleware (end-to-end through real gincommon middleware) ────

func TestContextMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	buildEngine := func() *gin.Engine {
		engine := gin.New()
		cfg := gincommon.Config{Logger: nil, ServiceName: "delegation-test"}
		engine.Use(gincommon.ProtectedMiddlewares(cfg)...)
		engine.Use(ContextMiddleware())
		engine.GET("/x", func(c *gin.Context) {
			rc, ok := requestctx.FromContext(c.Request.Context())
			if !ok {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "no requestctx bound"})
				return
			}
			c.JSON(http.StatusOK, gin.H{
				"tenant_id":  rc.TenantID,
				"user_id":    rc.UserID,
				"roles":      rc.Roles,
				"client_ip":  rc.ClientIP,
				"user_agent": rc.UserAgent,
			})
		})
		return engine
	}

	t.Run("missing identity headers -> 401, requestctx never bound", func(t *testing.T) {
		engine := buildEngine()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		require.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("malformed tenant header (control char) -> 401", func(t *testing.T) {
		engine := buildEngine()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("x-user-id", "user-1")
		req.Header.Set("x-tenant-id", "tenant\x00bad")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		require.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("valid identity headers bridge into requestctx.Context", func(t *testing.T) {
		engine := buildEngine()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("x-user-id", "user-42")
		req.Header.Set("x-tenant-id", "tenant-7")
		req.Header.Set("x-tenant-roles", "tenant_admin,tenant_member")
		req.Header.Set("User-Agent", "test-agent/1.0")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)

		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		require.Equal(t, "tenant-7", body["tenant_id"])
		require.Equal(t, "user-42", body["user_id"])
		require.Equal(t, []any{"tenant_admin", "tenant_member"}, body["roles"])
		require.Equal(t, "test-agent/1.0", body["user_agent"])
		require.NotEmpty(t, body["client_ip"])
	})
}
