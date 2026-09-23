// Package eventschema embeds the JSON Schema documents for this service's
// event payloads. They are payload-only schemas (the envelope's `data`
// field, not the envelope itself), DERIVED from api/asyncapi.yaml's
// <Name>Payload schemas by `make extract-schemas` (schema-gov extract) —
// never edit them by hand; CI's `extract --check` fails on drift (DLG-D51).
//
//   - ByEventType: the four PUBLISHED types (LLD §10.3/§10.5) —
//     DelegationStarted, DelegationEnded, DelegationReviewRequested,
//     DelegationEscalationRequested (Bug 2a). Validated at enqueue
//     (eventbus.ValidatingCodec), registered in Glue, and resolved by
//     definition at publish time (eventbus.GlueCodec).
//   - Consumed: the three types delegation-cascade-q consumes —
//     MembershipRevoked, TenantMembershipsPurged (iam-org-membership) and
//     UserUpdated (iam-user-profile). Validated before dispatch
//     (eventbus.ConsumedValidator); never registered in this service's Glue
//     registry — the producers own and register them.
package eventschema

import _ "embed"

// DelegationStarted is the DelegationStarted payload JSON Schema.
//
//go:embed delegation_started.json
var DelegationStarted []byte

// DelegationEnded is the DelegationEnded payload JSON Schema.
//
//go:embed delegation_ended.json
var DelegationEnded []byte

// DelegationReviewRequested is the DelegationReviewRequested payload JSON Schema.
//
//go:embed delegation_review_requested.json
var DelegationReviewRequested []byte

// DelegationEscalationRequested is the DelegationEscalationRequested
// payload JSON Schema (Bug 2a).
//
//go:embed delegation_escalation_requested.json
var DelegationEscalationRequested []byte

// ByEventType maps the frozen PascalCase event type name (LLD §10.5) to its
// embedded schema, for ValidatingCodec to compile at startup. Consumed-only
// types are deliberately absent (see Consumed) — missing schema is a
// pass-through at enqueue, and GlueCodec must never resolve them.
var ByEventType = map[string][]byte{
	"DelegationStarted":             DelegationStarted,
	"DelegationEnded":               DelegationEnded,
	"DelegationReviewRequested":     DelegationReviewRequested,
	"DelegationEscalationRequested": DelegationEscalationRequested,
}

// MembershipRevoked is iam-org-membership's MembershipRevoked payload JSON
// Schema, as this service consumes it.
//
//go:embed membership_revoked.json
var MembershipRevoked []byte

// TenantMembershipsPurged is iam-org-membership's TenantMembershipsPurged
// payload JSON Schema, as this service consumes it.
//
//go:embed tenant_memberships_purged.json
var TenantMembershipsPurged []byte

// UserUpdated is the subset of iam-user-profile's UserUpdated payload JSON
// Schema this service reads.
//
//go:embed user_updated.json
var UserUpdated []byte

// Consumed maps each event type delegation-cascade-q consumes to its
// embedded consumer-side schema, for eventbus.ConsumedValidator. These
// match the producers' required fields and types but are never stricter
// (see api/asyncapi.yaml's consumed-payload comment).
var Consumed = map[string][]byte{
	"MembershipRevoked":       MembershipRevoked,
	"TenantMembershipsPurged": TenantMembershipsPurged,
	"UserUpdated":             UserUpdated,
}
