package orgmembership

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
// Core (iam-org-membership) is built on platform-gincommon's
// ProtectedMiddlewares, which requires a well-formed UUID in x-tenant-id
// (hard 401 otherwise) and reads roles from x-tenant-roles, not
// X-User-Roles — confirmed directly against
// platform-gincommon@v1.3.0/internal/core/domain/transport.go and Core's
// own outbound clients (e.g. internal/adapter/outbound/delegationcheck),
// which all send x-user-id/x-tenant-id/x-tenant-roles, lowercase. This
// endpoint is tenant-scoped (unlike iam-catalog-admin's tenant-agnostic
// internal routes), so tenantID must be the real tenant being checked, not
// a placeholder.
func setInternalHeaders(req *http.Request, tenantID uuid.UUID) {
	req.Header.Set("x-user-id", "iam-system")
	req.Header.Set("x-tenant-id", tenantID.String())
	req.Header.Set("x-tenant-roles", "iam-system")
}

// propagate copies the caller's trace context onto the outbound request.
//
// port.MembershipCheckClient.Exists takes a plain context.Context, but
// gincommon.PropagateHeaders — which this project's shared-library-first
// rule requires using instead of a hand-rolled traceparent helper (unlike
// iam-tender-acl's membershipcheck/traceparent.go precedent) — needs a
// concrete *gin.Context to read the inbound identity/request-ID state Gin's
// middleware stashed via c.Set. Since *gin.Context itself satisfies
// context.Context (Deadline/Done/Err/Value), an inbound HTTP handler can
// pass c straight through the service layer as the ctx argument; when it
// does, this type-asserts it back out and gets the full
// trace+request-ID+identity propagation. Callers that only have a plain
// context.Context (background jobs, the cascade consumer, tests) fall back
// to OTel-only trace propagation via the same propagator gincommon uses
// internally, which is still correct, just without the request-ID/identity
// headers.
func propagate(ctx context.Context, req *http.Request) {
	if gc, ok := ctx.(*gin.Context); ok {
		gincommon.PropagateHeaders(gc, req)
		return
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
}
