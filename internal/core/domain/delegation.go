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

// The four delegation_status values: scheduled and active are the two
// non-terminal ones. scheduled is DLG-D25 (cross-service future-OOO bug
// fix) — a delegation created with a future starts_at is created in this
// state, not active, and never calls User Profile or emits
// DelegationStarted until the delegation-activation reconciler job (LLD
// §11.1a) flips it to active at starts_at. A scheduled delegation can still
// be cancelled (End's WHERE clause and probeVersionConflict's terminal
// check both accept scheduled alongside active) and is cascaded on
// MembershipRevoked exactly like an active one (EndForUser), since a
// departed member's still-scheduled delegation must not be left to activate
// later against membership that no longer exists.
const (
	DelegationScheduled DelegationStatus = "scheduled"
	DelegationActive    DelegationStatus = "active"
	DelegationEnded     DelegationStatus = "ended"
	DelegationCancelled DelegationStatus = "cancelled"
)

// EndReason is the payload field emitted with DelegationEnded (DEL-7).
// Not persisted — event-payload only. review_expired is DLG-D6/DLG-Q5,
// added over the pre-extraction O&M enum. delegate_disabled is Bug 2.
type EndReason string

// The five ended_reason values (DEL-7, DLG-D6/DLG-Q5, Bug 2).
const (
	EndReasonExpired         EndReason = "expired"
	EndReasonCancelled       EndReason = "cancelled"
	EndReasonDelegateRemoved EndReason = "delegate_removed"
	EndReasonReviewExpired   EndReason = "review_expired"
	// EndReasonDelegateDisabled (Bug 2): the delegate's account was disabled
	// (User Profile UserUpdated{status: disabled}) — distinct from
	// EndReasonDelegateRemoved, which fires on MembershipRevoked (the
	// delegate leaving the tenant entirely). "Disabled ≠ removed": a
	// disabled user is still a tenant member, so this reason exists so
	// downstream consumers (Notification, Audit) can distinguish the two.
	EndReasonDelegateDisabled EndReason = "delegate_disabled"
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
	ReviewLastWarnedBucket *int       // 3 | 2 | 1 | nil — last days_remaining value notified in the 3-day daily cascade (DLG-Q6)
	ReviewWindowDays       *int       // per-delegation override of the tenant/global default (DEL-14)
}
