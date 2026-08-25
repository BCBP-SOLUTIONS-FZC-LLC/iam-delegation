// Package eventschema embeds the JSON Schema documents used to validate
// outbound event payloads before they are written to the transactional
// outbox (internal/adapter/outbound/eventbus/validator.go). One schema per
// published event type (LLD §10.3/§10.5) — DelegationStarted, DelegationEnded,
// DelegationReviewRequested. These are payload-only schemas (they describe
// the shape of the envelope's `data` field, not the envelope itself); see
// api/asyncapi.yaml for the full documented contract including the envelope
// and the two consumed message types (MembershipRevoked, TenantMembershipsPurged),
// which are not validated by this package — this service only produces the
// three schemas embedded here.
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
