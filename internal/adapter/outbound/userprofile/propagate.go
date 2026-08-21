package userprofile

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

// setInternalHeaders authenticates as the reserved iam-system principal —
// iam-user-profile's internal_handler.go/middleware.go require the caller
// to present the "iam-system" role (rc.HasRole("iam-system")), populated
// from X-User-Roles; X-User-Id: iam-system is the documented system
// principal for the identity itself.
func setInternalHeaders(req *http.Request) {
	req.Header.Set("X-User-Id", "iam-system")
	req.Header.Set("X-User-Roles", "iam-system")
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
