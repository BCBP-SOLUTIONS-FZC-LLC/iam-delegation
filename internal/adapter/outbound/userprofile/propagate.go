package userprofile

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
// iam-user-profile is built on platform-gincommon's ProtectedMiddlewares +
// GUCBridgeMiddleware (internal/adapter/inbound/http/middleware.go), which
// requires a well-formed UUID in x-tenant-id (hard 401 otherwise, per
// GUCBridgeMiddleware's uuid.Parse(platformRc.TenantID) check) and reads
// roles from x-tenant-roles via rc.HasRole — there is no X-User-Roles
// special-casing anywhere in that service's middleware. tenantID must be
// the real tenant the delegation belongs to.
func setInternalHeaders(req *http.Request, tenantID uuid.UUID) {
	req.Header.Set("x-user-id", "iam-system")
	req.Header.Set("x-tenant-id", tenantID.String())
	req.Header.Set("x-tenant-roles", "iam-system")
}

// propagate copies the caller's trace context onto the outbound request.
// See orgmembership/propagate.go for the full rationale: port.UserProfileClient
// takes a plain context.Context, but gincommon.PropagateHeaders needs a
// concrete *gin.Context (which satisfies context.Context and so can be
// threaded through unchanged by a handler that wants full propagation);
// anything else falls back to OTel-only trace propagation.
func propagate(ctx context.Context, req *http.Request) {
	if gc, ok := ctx.(*gin.Context); ok {
		gincommon.PropagateHeaders(gc, req)
		return
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
}
