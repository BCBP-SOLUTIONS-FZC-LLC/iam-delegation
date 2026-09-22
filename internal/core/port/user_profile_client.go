package port

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// AvailabilitySnapshot is the result of a delegate OOO pre-flight check
// (GAP-DEL-2). Status mirrors iam-user-profile's availability status field.
// OOOUntil is non-nil when status is "ooo" and a bounded end is set by UP.
type AvailabilitySnapshot struct {
	Status   string
	OOOUntil *time.Time
}

// UserProfileClient is the outbound HTTP client for iam-user-profile.
// Availability-first coordination (DEL-6, LLD §11.1/§11.2/§11.3/§11.4):
// create waits for a 200 before writing; every end path clears the pointer
// via ClearDelegatePointer (Gap 3 Option B).
//
// GetAvailability is used as a delegate OOO pre-flight on the create path
// (GAP-DEL-2): DEL checks the delegate's status before writing, so that
// starts_at-relative OOO overlap is evaluated here rather than by UP.
//
// SetAvailability is used only for the create path:
//   - Create: {status:"ooo", ooo_from, ooo_until, delegate_id}
//
// ClearDelegatePointer is used for every end path (cancel/expiry/cascade/review):
//   - DELETE /internal/users/:id/availability/delegate — clears delegate_id
//     without touching the user's status (UP owns the return-to-available
//     transition and the user's pre-delegation status is preserved).
type UserProfileClient interface {
	GetAvailability(ctx context.Context, tenantID, userID uuid.UUID) (*AvailabilitySnapshot, error)
	SetAvailability(ctx context.Context, req SetAvailabilityRequest) error
	ClearDelegatePointer(ctx context.Context, tenantID, userID uuid.UUID) error
}

// SetAvailabilityRequest models the create path. Fields left zero-value are
// omitted from the wire payload.
type SetAvailabilityRequest struct {
	TenantID   uuid.UUID
	UserID     uuid.UUID
	Status     *string // "ooo" on create
	OOOFrom    *time.Time
	OOOUntil   *time.Time
	DelegateID *uuid.UUID // non-nil → wire "delegate_id": "<uuid>"
	Note       string     // free-text OOO note (≤500 chars, mapped from delegation reason)
}
