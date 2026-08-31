package http

import (
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/pkg/requestctx"
	gincommon "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

// maxIdempotencyKeyLen is a sanity cap — UUID v4 is 36 chars; 1 KiB allows
// any reasonable opaque token while rejecting obvious DoS payloads.
const maxIdempotencyKeyLen = 1024

// ContextMiddleware must run after gincommon.ProtectedMiddlewares (which
// populates gincommon's own, unexported-type domain.RequestContext from the
// gateway-injected x-user-id/x-tenant-id/x-tenant-roles headers — see
// platform-gincommon's middleware.ContextMiddleware). It copies that
// identity into this repo's own pkg/requestctx.Context — already the
// ground-truth type internal/core/service depends on (delegation_service.go
// reads it back out via requestctx.FromContext for outbox event
// IP/UserAgent fields) — so the rest of this service, and unit tests, never
// reference gincommon's internal type directly. Mirrors iam-tender-acl's
// ContextBridgeMiddleware, adapted to bridge into the pre-existing
// requestctx package rather than a new local type.
//
// Per LLD §7.3/§13.1, tenant isolation's DB-level enforcement (the
// `SET LOCAL app.tenant_id` GUC bind for RLS) happens entirely inside the
// postgres outbound adapter, one call at a time, built directly from the
// plain tenantID value handlers pass into the service layer — never from a
// context value threaded through this http package. This middleware
// therefore has nothing to do with pgcommon and imports nothing from it
// (confirmed against iam-tender-acl's identical layering: its http package
// only ever touches pgcommon for the unrelated PostgresHealth.Health DTO in
// health.go).
func ContextMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if prc, ok := gincommon.RequestContext(c); ok {
			rc := &requestctx.Context{
				UserID:    prc.UserID,
				TenantID:  prc.TenantID,
				Roles:     prc.Roles,
				ClientIP:  prc.ClientIP,
				UserAgent: c.Request.UserAgent(),
			}
			c.Request = c.Request.WithContext(requestctx.WithContext(c.Request.Context(), rc))
		}
		c.Next()
	}
}

// RequireAnyRole is a route-level gate for routes whose authorization rule
// is a pure role check with no per-resource ownership component (DLG-7 —
// LLD §8.2 "tenant_admin/tenant_owner"). For DLG-3/4/5's "self or admin"
// rule, use requireSelfOrAdmin (authz.go) instead — that rule needs the
// target delegation's delegator_id, which isn't known until the record is
// read, so it can't be expressed as route-level middleware.
func RequireAnyRole(roles ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		rc, ok := requestctx.FromContext(c.Request.Context())
		if !ok {
			HandleError(c, domain.NewError(domain.ErrMissingIdentity, "missing identity headers"))
			return
		}
		for _, role := range roles {
			if rc.HasRole(role) {
				c.Next()
				return
			}
		}
		HandleError(c, domain.NewError(domain.ErrInsufficientRole, "caller lacks a required role"))
	}
}

// requireIdempotencyKey enforces DLG-2's mandatory `Idempotency-Key` header
// (LLD §8.2 "requires Idempotency-Key", §9.2 DLG-Q3). Missing header ->
// 400 validation_error: this is malformed-request territory (a required
// header absent), not a 422 domain-rule violation — §20's taxonomy table
// has no dedicated code for it, and validation_error is the closest fit
// among the existing 400-mapped sentinels.
func requireIdempotencyKey() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.GetHeader("Idempotency-Key")
		if strings.TrimSpace(key) == "" || len(key) > maxIdempotencyKeyLen {
			HandleError(c, domain.NewError(domain.ErrValidation, "Idempotency-Key header is required"))
			return
		}
		c.Next()
	}
}
