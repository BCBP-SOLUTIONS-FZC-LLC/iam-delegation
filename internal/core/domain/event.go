package domain

import (
	"time"

	"github.com/google/uuid"
)

// SystemActorID is the well-known UUID used as actor_id in system-initiated
// events (cron jobs, the cascade consumer). Distinguishable from uuid.Nil.
var SystemActorID = uuid.MustParse("00000000-0000-0000-0000-000000000002")

// DomainEvent is the framework-agnostic event carrier passed from services
// into the outbox layer. The eventbus adapter wraps this in an
// events.Envelope[json.RawMessage] and inserts it into outbox_events within
// the caller's active pgx.Tx, atomic with the state change (DLG-EVT-1).
//
// sits alongside platform-events' own events.Envelope/events.Handler at the
// same call sites (see internal/adapter/outbound/postgres/db.go), and a
// bare domain.Event would read as ambiguously generic against that
// neighboring vocabulary. The stutter is judged more readable here than
// the alternative.
//
//nolint:revive // "DomainEvent" (not "Event") is intentional: this type
type DomainEvent struct {
	Type       string
	TenantID   uuid.UUID
	Subject    string // resource identifier the event is about (the delegation_id)
	Actor      string // user_id of the initiator; "iam-system" for cron/cascade
	OccurredAt time.Time
	Data       any // payload — JSON-marshaled by the publisher

	// Optional audit fields. System/cron-origin events carry the sentinels
	// "system" / "iam-delegation/<job>-cron" per LLD §10.5 note (1).
	IPAddress string
	UserAgent string
}

// Published event type constants — the PascalCase names on
// iam.delegation.events (LLD §10.5, DLG-D5).
const (
	EventDelegationStarted         = "DelegationStarted"
	EventDelegationEnded           = "DelegationEnded"
	EventDelegationReviewRequested = "DelegationReviewRequested"
)

// Consumed event type constants — Core's (iam-org-membership's)
// removal/tenant-scrub signals on delegation-cascade-q (LLD §10.1,
// DLG-Q4/DLG-D14). Not produced here.
//
// EventTenantMembershipsPurged is Core's own tenant-level cascade relay,
// on iam.membership.events (NOT iam.tenant.events) — Core renamed it from
// "TenantOffboarded" specifically because that name collided with Realm
// Provisioner's own, differently-scoped TenantOffboarded event, which Core
// only consumes and never re-emits ("one producer per event name"). This
// service's cascade consumer must match Core's current name, not RP's.
const (
	EventMembershipRevoked       = "MembershipRevoked"
	EventTenantMembershipsPurged = "TenantMembershipsPurged"
)

// EventUserUpdated is User Profile's (iam-user-profile's) consumed
// UserUpdated signal on iam.user.events — a second SNS subscription onto
// the same delegation-cascade-q queue, filtered to EventType = "user.updated"
// (dot-notation: User Profile's own EventType convention, unlike Core's
// PascalCase above). Bug 2 (cross-service): a delegate being disabled is
// NOT a MembershipRevoked/tenant-removal signal — "disabled" is a status
// flip within the tenant, not a departure — so without this second
// subscription an active delegation whose delegate gets disabled was never
// ended, silently stranding it (delegate_disabled below). Only dispatched
// when the decoded payload's status field is "disabled"; changed_fields
// alone cannot say what status changed TO.
const EventUserUpdated = "user.updated"

// Topic is the single dedicated SNS topic this service publishes to
// (DLG-D5) — unlike O&M there is no RoutingPublisher; one topic, three types.
const Topic = "iam.delegation.events"

// Source is the envelope's `source` field (LLD §10.3 EventEnvelope).
const Source = "iam-delegation"
