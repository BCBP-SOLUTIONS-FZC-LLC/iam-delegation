// Package domain holds the core entity types, value objects, and error
// catalog for the Delegation Service. It imports nothing outside the
// standard library and third-party value libraries — no framework, no
// adapter, no infrastructure (LLD §6.2).
package domain

import (
	"time"

	"github.com/google/uuid"
)

// DelegationScope mirrors the delegation_scope enum (LLD §7.1).
type DelegationScope string

// The three delegation_scope values (DEL-2).
const (
	ScopeAll        DelegationScope = "all"
	ScopeDepartment DelegationScope = "department"
	ScopeTender     DelegationScope = "tender"
)

// DelegationStatus mirrors the delegation_status enum (LLD §7.1).
type DelegationStatus string

// The three delegation_status values (DEL-3): active is the only non-terminal one.
const (
	DelegationActive    DelegationStatus = "active"
	DelegationEnded     DelegationStatus = "ended"
	DelegationCancelled DelegationStatus = "cancelled"
)

// EndReason is the payload field emitted with DelegationEnded (DEL-7).
// Not persisted — event-payload only. review_expired is DLG-D6/DLG-Q5,
// added over the pre-extraction O&M enum.
type EndReason string

// The four ended_reason values (DEL-7, DLG-D6/DLG-Q5).
const (
	EndReasonExpired         EndReason = "expired"
	EndReasonCancelled       EndReason = "cancelled"
	EndReasonDelegateRemoved EndReason = "delegate_removed"
	EndReasonReviewExpired   EndReason = "review_expired"
)

// Delegation is the authoritative OOO grant that drives workflow reroute via
// DelegationStarted / DelegationEnded events on iam.delegation.events. User
// Profile keeps a presentation-only user_availability row (soft pointer).
// See LLD §11.1/§11.2 for the availability-first coordination flow.
//
// DelegatorMembershipID / DelegateMembershipID are populated from Core's
// grant-time membership-existence check (LLD §7.6.2, DLG-D3) rather than a
// local FK lookup — the composite FKs the pre-extraction schema had cannot
// cross databases and are not recreated here (LLD §7.6.1).
type Delegation struct {
	ID                    uuid.UUID
	TenantID              uuid.UUID
	DelegatorID           uuid.UUID
	DelegateID            uuid.UUID
	DelegatorMembershipID uuid.UUID
	DelegateMembershipID  uuid.UUID
	Scope                 DelegationScope
	ScopeID               *uuid.UUID // required when Scope != all (DEL-2)
	Reason                string     // audit-only, capped 500 chars service-side (DEL-10)
	StartsAt              time.Time
	EndsAt                *time.Time // open-ended allowed (DEL-8)
	Status                DelegationStatus
	RecordVersion         int64
	CreatedAt             time.Time
	UpdatedAt             time.Time
	DeletedAt             *time.Time

	// DEL-13/DLG-D7: review window fields — only set when EndsAt is nil.
	ReviewDueAt            *time.Time // set at creation to StartsAt + review window days
	ReviewLastWarnedBucket *int       // 7 | 3 | nil — which review notice fired this cycle (DLG-Q6)
	ReviewWindowDays       *int       // per-delegation override of the tenant/global default (DEL-14)
}
