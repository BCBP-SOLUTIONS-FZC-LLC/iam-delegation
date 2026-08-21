package port

import (
	"context"

	"github.com/google/uuid"
)

// MembershipCheckClient replaces the two lost composite membership FKs
// (LLD §7.6.2, DLG-D3) with a synchronous grant-time check against Core's
// GET /internal/tenants/:id/members/:user_id/exists.
type MembershipCheckClient interface {
	// Exists reports whether userID has an active membership in tenantID.
	// err is non-nil ONLY when the check itself could not be performed
	// (network/timeout/5xx) — never as a way of expressing "not active".
	// Callers fail closed (503 org_membership_unavailable) on err != nil,
	// and 422 invalid_delegate on active == false.
	Exists(ctx context.Context, tenantID, userID uuid.UUID) (active bool, membershipID uuid.UUID, err error)
}
