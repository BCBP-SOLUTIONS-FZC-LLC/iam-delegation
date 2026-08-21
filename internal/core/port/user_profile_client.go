package port

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// UserProfileClient is the outbound HTTP client for iam-user-profile.
// Availability-first coordination (DEL-6, LLD §11.1/§11.2/§11.3/§11.4):
// create waits for a 200 before writing; every end path clears the pointer
// only.
//
// SetAvailability variants:
//   - Create:   {status:"ooo", ooo_from, ooo_until, delegate_id}
//   - End:      {delegate_id: null} — pointer-clear only (DEL-6); NEVER
//     {status:"available"} (UP owns the return-to-available transition).
type UserProfileClient interface {
	SetAvailability(ctx context.Context, req SetAvailabilityRequest) error
}

// SetAvailabilityRequest models the two shapes documented above. Fields
// left zero-value are omitted from the wire payload.
type SetAvailabilityRequest struct {
	TenantID      uuid.UUID
	UserID        uuid.UUID
	Status        *string // "ooo" on create; nil on end (pointer-clear)
	OOOFrom       *time.Time
	OOOUntil      *time.Time
	DelegateID    *uuid.UUID // non-nil → wire "delegate_id": "<uuid>"
	ClearDelegate bool       // true → send delegate_id: null explicitly
	Note          string     // free-text OOO note (≤500 chars, mapped from delegation reason)
}
