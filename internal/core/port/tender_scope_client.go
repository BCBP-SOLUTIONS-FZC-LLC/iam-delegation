package port

import (
	"context"

	"github.com/google/uuid"
)

// TenderScopeClient validates a scope="tender" delegation's scope_id against
// Tender's tender-existence/liveness endpoint (LLD §7.6.7, DLG-D13) — the
// same shape as MembershipCheckClient, so a future co-location can delete
// it. Tender owns the endpoint as a cross-team task; until it ships (tracked
// as IB-4, blocked on EXT-1), no adapter implementing this interface is
// wired into the composition root, and tender-scoped delegations fall back
// to the presence-only chk_scope_id check in
// delegation_service.go's validateCreateInput — documented, not silent.
type TenderScopeClient interface {
	// CheckLive reports whether scopeID names a tender in tenantID and, if
	// so, whether it is still live (open/active — excludes
	// archived/closed/deleted). err is non-nil ONLY when the check itself
	// could not be performed (network/timeout/5xx) — never as a way of
	// expressing "not live" — wrapped with ErrDependencyUnavailable so
	// callers can fail closed (503 tender_unavailable) via errors.Is.
	// exists==false or live==false with err==nil is a business rejection
	// (422 tender_scope_not_found), not a dependency failure.
	CheckLive(ctx context.Context, tenantID, scopeID uuid.UUID) (exists bool, live bool, err error)
}
