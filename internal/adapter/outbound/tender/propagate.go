package tender

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
// Tender's provider endpoint (LLD §7.6.7) is documented as mesh-only with
// the same reserved-system-principal posture as Core's and User Profile's
// internal routes; tenantID must be the real tenant the delegation belongs
// to, not a placeholder.
func setInternalHeaders(req *http.Request, tenantID uuid.UUID) {
	req.Header.Set("x-user-id", "iam-system")
	req.Header.Set("x-tenant-id", tenantID.String())
	req.Header.Set("x-tenant-roles", "iam-system")
}

// propagate copies the caller's trace context onto the outbound request.
// See orgmembership/propagate.go for the full rationale: port.TenderScopeClient
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
