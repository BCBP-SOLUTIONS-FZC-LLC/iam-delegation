package domain

import (
	"time"

	"github.com/google/uuid"
)

// Event payload structs — one per outbound event (LLD §10.3/§10.5). These
// become the `data` field of the event envelope. JSON tags match the
// AsyncAPI schema in api/asyncapi.yaml. Payloads are self-contained
// snapshots (LLD §10.6/DLG-EVT-7) — TenantID/ActorID are carried in the
// payload itself as well as at the envelope level, matching the shipped
// O&M shape this was lifted from.

// DelegationStartedPayload is DelegationStarted's data (LLD §10.3
// DelegationStartedPayload). Emitted by DLG-2 create and the create leg of
// DLG-5 reassign.
type DelegationStartedPayload struct {
	DelegationID uuid.UUID       `json:"delegation_id"`
	TenantID     uuid.UUID       `json:"tenant_id"`
	DelegatorID  uuid.UUID       `json:"delegator_id"`
	DelegateID   uuid.UUID       `json:"delegate_id"`
	Scope        DelegationScope `json:"scope"`
	ScopeID      *uuid.UUID      `json:"scope_id,omitempty"`
	StartsAt     time.Time       `json:"starts_at"`
	EndsAt       *time.Time      `json:"ends_at,omitempty"`
	ActorID      uuid.UUID       `json:"actor_id"`
}

// DelegationEndedPayload is DelegationEnded's data (LLD §10.3
// DelegationEndedPayload). Fires on every end path — cancel, ends_at
// expiry, review auto-end, delegate-removed cascade (DLG-EVT-3) — never
// path-dependent. Delegator-side removal ends silently, no event
// (DEL-7/DLG-EVT-4).
type DelegationEndedPayload struct {
	DelegationID uuid.UUID       `json:"delegation_id"`
	TenantID     uuid.UUID       `json:"tenant_id"`
	DelegatorID  uuid.UUID       `json:"delegator_id"`
	DelegateID   uuid.UUID       `json:"delegate_id"`
	Scope        DelegationScope `json:"scope"`
	ScopeID      *uuid.UUID      `json:"scope_id,omitempty"`
	EndedReason  EndReason       `json:"ended_reason"` // expired | cancelled | delegate_removed | review_expired
	ActorID      uuid.UUID       `json:"actor_id"`
}

// DelegationReviewRequestedPayload is DelegationReviewRequested's data (LLD
// §10.3 DelegationReviewRequestedPayload). Emitted by the delegation-review
// sweep once per calendar day for each of the 3 days before review_due_at
// (DLG-I2, DLG-D7). Unlike the pre-extraction O&M shape, this carries
// days_remaining rather than review_due_at (DLG-Q6's 3-day daily cascade).
type DelegationReviewRequestedPayload struct {
	DelegationID  uuid.UUID       `json:"delegation_id"`
	TenantID      uuid.UUID       `json:"tenant_id"`
	DelegatorID   uuid.UUID       `json:"delegator_id"`
	DelegateID    uuid.UUID       `json:"delegate_id"`
	Scope         DelegationScope `json:"scope"`
	ScopeID       *uuid.UUID      `json:"scope_id,omitempty"`
	DaysRemaining int             `json:"days_remaining"` // 3 | 2 | 1
	ActorID       uuid.UUID       `json:"actor_id"`
}

// ── Consumed payloads (Core → this service, delegation-cascade-q) ─────────

// MembershipRevokedPayload is the data of Core's consumed MembershipRevoked
// event (LLD §10.1, DLG-Q4/DLG-D14).
type MembershipRevokedPayload struct {
	TenantID uuid.UUID `json:"tenant_id"`
	UserID   uuid.UUID `json:"user_id"`
	ActorID  uuid.UUID `json:"actor_id"`
}

// TenantMembershipsPurgedPayload is the data of Core's (iam-org-membership's)
// consumed TenantMembershipsPurged event on iam.membership.events — Core's
// tenant-scrub cascade relay, renamed from "TenantOffboarded" to avoid
// colliding with Realm Provisioner's own event of that name (LLD §10.1,
// DLG-Q4/DLG-D14). Field shape is unchanged from the old TenantOffboarded
// payload this service used to expect.
type TenantMembershipsPurgedPayload struct {
	TenantID uuid.UUID `json:"tenant_id"`
	ActorID  uuid.UUID `json:"actor_id"`
}

// UserUpdatedPayload is the data of User Profile's (iam-user-profile's)
// consumed UserUpdated event on iam.user.events (Bug 2). Mirrors the
// subset of iam-user-profile's UserUpdatedPayload this service actually
// needs — Email and other changed-field-specific values are intentionally
// omitted, not decoded. Status is a pointer because it is only populated
// upstream when "status" is present in ChangedFields.
type UserUpdatedPayload struct {
	UserID        uuid.UUID `json:"user_id"`
	ChangedFields []string  `json:"changed_fields"`
	Status        *string   `json:"status,omitempty"`
}
