package http

import (
	"context"
	"log/slog"
	"net/http"

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

// slogPlatformLogger adapts *slog.Logger to the map[string]interface{}-based
// Logger interface both platform-gincommon's and platform-events' Config
// structs expect (identical method shape in both libraries, structurally
// satisfied here without importing either library's internal port package).
type slogPlatformLogger struct{ l *slog.Logger }

func (a slogPlatformLogger) Debug(msg string, fields map[string]interface{}) {
	a.l.Debug(msg, mapToArgs(fields)...)
}
func (a slogPlatformLogger) Info(msg string, fields map[string]interface{}) {
	a.l.Info(msg, mapToArgs(fields)...)
}
func (a slogPlatformLogger) Warn(msg string, fields map[string]interface{}) {
	a.l.Warn(msg, mapToArgs(fields)...)
}
func (a slogPlatformLogger) Error(msg string, fields map[string]interface{}) {
	a.l.Error(msg, mapToArgs(fields)...)
}

func mapToArgs(fields map[string]interface{}) []any {
	args := make([]any, 0, len(fields)*2)
	for k, v := range fields {
		args = append(args, k, v)
	}
	return args
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
	cache Pinger,
	outbox Pinger,
	logger *slog.Logger,
	tracing *gincommon.TracingOptions,
	docs DocsConfig,
	bindTenantGUC BindTenantGUC,
) *Router {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()

	platformLogger := slogPlatformLogger{l: logger}
	cfg := gincommon.Config{Logger: platformLogger, ServiceName: "delegation", Tracing: tracing}

	// Observability (recovery/request-id/tracing/metrics/correlation/
	// logging) applies to every route, including /internal/*. Auth
	// (ProtectedMiddlewares) applies only to the public group below —
	// /internal/* is a mesh-only trust boundary with no RBAC/JWT check
	// (LLD §8.2/§13.2), mirroring iam-tender-acl's TAC-4 registration.
	engine.Use(gincommon.ObservabilityMiddlewares(cfg)...)

	public := engine.Group("/api/v1/delegations")
	public.Use(gincommon.ProtectedMiddlewares(cfg)...)
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

	hc := &healthHandlers{postgres: postgres, cache: cache, outbox: outbox}
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
	group := engine.Group("/swagger")
	if docs.Environment == "production" && docs.AuthToken != "" {
		group.Use(docsAuthMiddleware(docs.AuthToken))
	}
	group.GET("/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

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
func docsAuthMiddleware(token string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetHeader("Authorization") != "Bearer "+token {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Next()
	}
}

// Handler returns the http.Handler to serve.
func (r *Router) Handler() http.Handler { return r.engine }
