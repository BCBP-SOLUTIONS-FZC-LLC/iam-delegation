package orgmembership

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

// setInternalHeaders authenticates as the reserved iam-system principal —
// the same internal-call convention iam-tender-acl's membershipcheck client
// and iam-group-mapping's catalogclient use against internal-route callees
// (iam-user-profile's middleware.go: RequireInternalRole checks
// rc.HasRole("iam-system"), populated from X-User-Roles).
func setInternalHeaders(req *http.Request) {
	req.Header.Set("X-User-Id", "iam-system")
	req.Header.Set("X-User-Roles", "iam-system")
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
