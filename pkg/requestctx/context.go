// Package requestctx carries the gateway-asserted identity (x-tenant-id,
// x-user-id, x-tenant-roles) through a request, mirroring the sibling
// services' pkg/requestctx. Populated by the inbound http ContextMiddleware
// from headers Envoy/ext_authz injects — never from the request body
// (LLD §13.1).
package requestctx

import (
	"context"
	"slices"
)

// Context is the request-scoped identity carried alongside context.Context.
type Context struct {
	UserID    string
	TenantID  string
	Roles     []string
	ClientIP  string
	UserAgent string
}

type ctxKey struct{}

// WithContext binds rc into ctx.
func WithContext(ctx context.Context, rc *Context) context.Context {
	return context.WithValue(ctx, ctxKey{}, rc)
}

// FromContext retrieves the Context bound by WithContext.
func FromContext(ctx context.Context) (*Context, bool) {
	rc, ok := ctx.Value(ctxKey{}).(*Context)
	return rc, ok
}

// HasRole reports whether the caller carries the given tenant role.
func (c *Context) HasRole(role string) bool {
	return slices.Contains(c.Roles, role)
}

// IsAdmin reports whether the caller is a tenant_admin or tenant_owner —
// the role gate for DLG-3/4/5/7 admin-or-self routes (LLD §8.2).
func (c *Context) IsAdmin() bool {
	return c.HasRole("tenant_admin") || c.HasRole("tenant_owner")
}
