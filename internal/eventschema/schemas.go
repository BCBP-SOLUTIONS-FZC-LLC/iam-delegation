// Package eventschema embeds the JSON Schema documents used to validate
// outbound event payloads before they are written to the transactional
// outbox (internal/adapter/outbound/eventbus/validating_codec.go). One
// schema per published event type (LLD §10.3/§10.5) — DelegationStarted,
// DelegationEnded, DelegationReviewRequested, DelegationEscalationRequested
// (Bug 2a). These are payload-only schemas (they describe the shape of the
// envelope's `data` field, not the envelope itself); see api/asyncapi.yaml
// for the full documented contract including the envelope and the consumed
// message types, which are not validated by this package — this service
// only produces the four schemas embedded here.
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
// types (MembershipRevoked, TenantMembershipsPurged, UserUpdated) are
// deliberately absent — missing schema is a pass-through at enqueue.
var ByEventType = map[string][]byte{
	"DelegationStarted":             DelegationStarted,
	"DelegationEnded":               DelegationEnded,
	"DelegationReviewRequested":     DelegationReviewRequested,
	"DelegationEscalationRequested": DelegationEscalationRequested,
}
