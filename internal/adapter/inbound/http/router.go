package http

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"

	gincommon "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/pkg/requestctx"
)

// BindTenantGUC binds the pgcommon RLS GUC (app.tenant_id/app.user_id) onto
// ctx for the request's lifetime. Satisfied by
// internal/adapter/outbound/postgres.WithTenantGUC, injected here (rather
// than imported directly) so this package never depends on pgcommon —
// cmd/server wires the concrete function. Every public route ultimately
// reaches the postgres adapter either through a TxRunner.RunInTx call
// (Create/Cancel/Extend/Reassign) or a direct read (List/settings/
// requireSelfOrAdmin's FindByID probe); both read the GUC already bound on
// ctx rather than deriving it from a parameter, so this must run before any
// of them — RLS-6's "bind before any UPDATE" applies here exactly as it
// does in the reconciler jobs and cascade consumer (LLD §7.3).
type BindTenantGUC func(ctx context.Context, tenantID, userID string) context.Context

// tenantGUCMiddleware applies BindTenantGUC using the identity
// ContextMiddleware already bridged into requestctx.Context. Must be
// registered after ContextMiddleware() on the same route group.
func tenantGUCMiddleware(bind BindTenantGUC) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rc, ok := requestctx.FromContext(c.Request.Context()); ok {
			ctx := bind(c.Request.Context(), rc.TenantID, rc.UserID)
			c.Request = c.Request.WithContext(ctx)
		}
		c.Next()
	}
}

// settingsSetRoles are the x-tenant-roles allowed to call DLG-7 (LLD §8.2).
var settingsSetRoles = []string{"tenant_admin", "tenant_owner"}

// DocsConfig controls whether — and how — the Swagger/AsyncAPI docs surface
// is exposed. Outside production it's always on; in production it's opt-in
// via Enabled and, if AuthToken is set, gated behind a bearer token.
// Mirrors iam-tender-acl's / iam-org-membership's identical DocsConfig.
type DocsConfig struct {
	Environment string
	Enabled     bool
	AuthToken   string
}

func (d DocsConfig) active() bool {
	return d.Environment != "production" || d.Enabled
}

// Router builds and owns the Gin engine for this service.
type Router struct {
	engine *gin.Engine
}

// Logger is the map[string]interface{}-based logging interface both
// platform-gincommon's and platform-events' Config structs expect
// (identical method shape in both libraries, structurally satisfied by the
// Zap-backed logger platform-gincommon/pkg/logger.NewLogger returns without
// any adapter — cmd/server passes that value straight through here).
type Logger interface {
	Debug(msg string, fields map[string]interface{})
	Info(msg string, fields map[string]interface{})
	Warn(msg string, fields map[string]interface{})
	Error(msg string, fields map[string]interface{})
}

// NewRouter wires every route: the public delegation API (DLG-1…7,
// gateway-fronted, auth via gincommon.ProtectedMiddlewares), the internal
// mesh-only cron/query API (DLG-I1…I4, no RBAC — mTLS trust boundary per
// LLD §8.2/§13.2, TAC-4-equivalent pattern), and health/docs.
func NewRouter(
	delegationHandler *DelegationHandler,
	settingsHandler *SettingsHandler,
	internalHandler *InternalHandler,
	postgres PostgresHealth,
	sysPostgres PostgresHealth,
	cache Pinger,
	outbox Pinger,
	ginCfg gincommon.Config,
	docs DocsConfig,
	bindTenantGUC BindTenantGUC,
) *Router {
	engine := gin.New()
	engine.HandleMethodNotAllowed = true

	if ginCfg.ServiceName == "" {
		ginCfg.ServiceName = "iam-delegation"
	}

	// Middleware order matches platform-gincommon's README and
	// iam-realm-provisioner: 1 MB body cap → TimeoutMiddleware →
	// ObservabilityMiddlewares (recovery/request-id/tracing/metrics/
	// correlation/logging) on every route, including /internal/*. Auth
	// (ProtectedMiddlewares) applies only to the public group below —
	// /internal/* is a mesh-only trust boundary with no RBAC/JWT check
	// (LLD §8.2/§13.2). gin.SetMode is decided in cmd/server from APP_ENV.
	engine.Use(func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
		c.Next()
	})
	engine.Use(gincommon.TimeoutMiddleware(30 * time.Second))
	engine.Use(gincommon.ObservabilityMiddlewares(ginCfg)...)

	public := engine.Group("/api/v1/delegations")
	public.Use(gincommon.ProtectedMiddlewares(ginCfg)...)
	public.Use(ContextMiddleware())
	if bindTenantGUC != nil {
		public.Use(tenantGUCMiddleware(bindTenantGUC))
	}
	public.GET("", delegationHandler.List)
	public.POST("", requireIdempotencyKey(), delegationHandler.Create)
	public.DELETE("/:id", delegationHandler.Cancel)
	public.POST("/:id/extend", delegationHandler.Extend)
	public.POST("/:id/reassign", delegationHandler.Reassign)
	public.GET("/settings", settingsHandler.Get)
	public.PUT("/settings", RequireAnyRole(settingsSetRoles...), settingsHandler.Set)

	// DLG-I1…I4 — mesh-only, mTLS trust boundary: no RBAC, no JWT parsing,
	// no ContextMiddleware (there is no gateway identity to bridge on
	// these routes at all, LLD §13.2).
	internalGroup := engine.Group("/internal")
	internalGroup.POST("/delegations/expire", internalHandler.Expire)
	internalGroup.POST("/delegations/review-sweep", internalHandler.ReviewSweep)
	internalGroup.GET("/delegations/dept-delegate", internalHandler.DeptDelegate)
	internalGroup.GET("/users/:id/active-delegations", internalHandler.ActiveDelegations)

	hc := &healthHandlers{postgres: postgres, sysPostgres: sysPostgres, cache: cache, outbox: outbox}
	// healthz is gincommon.HealthHandler() itself, not a local
	// reimplementation of its {"status":"ok"} body.
	engine.GET("/healthz", gincommon.HealthHandler())
	engine.GET("/readyz", hc.readyz)

	registerDocsRoutes(engine, docs)

	return &Router{engine: engine}
}

// registerDocsRoutes wires the Swagger UI (REST, generated by `make swag`
// from the // @… annotations next to each handler — see
// cmd/server/swagger_info.go for the top-level spec metadata) and the
// AsyncAPI viewer (asyncapi.go, rendering the embedded api/asyncapi.yaml).
// Outside production both are always mounted; in production they're opt-in
// via DocsConfig.Enabled and, if AuthToken is set, gated behind a bearer
// token. Mirrors iam-tender-acl's identical registerDocsRoutes.
func registerDocsRoutes(engine *gin.Engine, docs DocsConfig) {
	if !docs.active() {
		return
	}
	stdSwagger := ginSwagger.WrapHandler(swaggerFiles.Handler)
	group := engine.Group("/swagger")
	if docs.Environment == "production" && docs.AuthToken != "" {
		group.Use(docsAuthMiddleware(docs.AuthToken))
	}
	group.GET("/*any", func(c *gin.Context) {
		switch {
		case strings.HasSuffix(c.Request.URL.Path, "/index.css"):
			SwaggerThemeHandler(c)
		case strings.HasSuffix(c.Request.URL.Path, "/swagger-initializer.js"):
			SwaggerInitializerHandler(c)
		default:
			stdSwagger(c)
		}
	})

	asyncGroup := engine.Group("")
	if docs.Environment == "production" && docs.AuthToken != "" {
		asyncGroup.Use(docsAuthMiddleware(docs.AuthToken))
	}
	asyncGroup.Use(envMiddleware(docs.Environment))
	asyncGroup.GET("/asyncapi", AsyncAPIHandler)
	asyncGroup.GET("/asyncapi.yaml", AsyncAPIYAMLHandler)
}

// docsAuthMiddleware requires an exact `Authorization: Bearer <token>` match
// before letting a request through to the Swagger UI / AsyncAPI viewer.
// Compared in constant time (crypto/subtle) rather than with `!=` — a plain
// string compare short-circuits on the first differing byte, which is a
// timing side-channel an attacker could use to recover the token one byte
// at a time.
func docsAuthMiddleware(token string) gin.HandlerFunc {
	want := []byte("Bearer " + token)
	return func(c *gin.Context) {
		got := []byte(c.GetHeader("Authorization"))
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Next()
	}
}

// Handler returns the http.Handler to serve.
func (r *Router) Handler() http.Handler { return r.engine }
