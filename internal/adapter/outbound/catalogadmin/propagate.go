package catalogadmin

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

// setInternalHeaders authenticates as the reserved iam-system principal.
//
// Catalog Admin (CAT-7) is a public route — "any authenticated caller" — so
// no specific role is required. Using x-tenant-roles: iam-system is
// consistent with how every other mesh-internal caller in this codebase
// identifies itself, and Catalog Admin's IdentityBridgeMiddleware accepts it.
//
// x-tenant-id is required on every Catalog Admin route even though the
// service has no tenant concept (CAT-D12 / Gap 11 fix). uuid.Nil is the
// correct value for calls without a tenant scope — Catalog Admin explicitly
// accepts it (Gap 11 fix: allows uuid.Nil in IdentityBridgeMiddleware).
func setInternalHeaders(req *http.Request) {
	req.Header.Set("x-user-id", "iam-system")
	req.Header.Set("x-tenant-id", uuid.Nil.String())
	req.Header.Set("x-tenant-roles", "iam-system")
	req.Header.Set("x-caller-service", "iam-delegation")
}

// propagate copies the caller's trace context onto the outbound request.
// See orgmembership/propagate.go for the full rationale: gincommon.PropagateHeaders
// needs a concrete *gin.Context (which satisfies context.Context and can be
// threaded through unchanged); anything else falls back to OTel-only trace
// propagation.
func propagate(ctx context.Context, req *http.Request) {
	if gc, ok := ctx.(*gin.Context); ok {
		gincommon.PropagateHeaders(gc, req)
		return
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
}
