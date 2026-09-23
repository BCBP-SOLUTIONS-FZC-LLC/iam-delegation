package eventbus

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/eventschema"
)

func newTestConsumedValidator(t *testing.T) *ConsumedValidator {
	t.Helper()
	v, err := NewConsumedValidator()
	require.NoError(t, err)
	return v
}

// Payloads shaped exactly as the producers emit them must pass — including
// the optional-upstream fields being absent and additive fields a producer
// may add later (OPEN schemas, no changed_fields enum).
func TestConsumedValidator_AcceptsProducerShapedPayloads(t *testing.T) {
	v := newTestConsumedValidator(t)
	id := func() string { return uuid.NewString() }
	cases := map[string]struct {
		eventType string
		payload   string
	}{
		"MembershipRevoked with actor":          {"MembershipRevoked", `{"tenant_id":"` + id() + `","user_id":"` + id() + `","actor_id":"` + id() + `"}`},
		"MembershipRevoked system-driven":       {"MembershipRevoked", `{"tenant_id":"` + id() + `","user_id":"` + id() + `"}`},
		"TenantMembershipsPurged with actor":    {"TenantMembershipsPurged", `{"tenant_id":"` + id() + `","actor_id":"` + id() + `"}`},
		"TenantMembershipsPurged without actor": {"TenantMembershipsPurged", `{"tenant_id":"` + id() + `"}`},
		"UserUpdated disabled":                  {"UserUpdated", `{"user_id":"` + id() + `","changed_fields":["status"],"status":"disabled"}`},
		"UserUpdated unrelated field + email":   {"UserUpdated", `{"user_id":"` + id() + `","changed_fields":["display_name","email"],"email":"a@b.example"}`},
		"UserUpdated future field and property": {"UserUpdated", `{"user_id":"` + id() + `","changed_fields":["some_future_field"],"new_prop":1}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.NoError(t, v.Validate(tc.eventType, json.RawMessage(tc.payload)))
		})
	}
}

func TestConsumedValidator_RejectsViolations(t *testing.T) {
	v := newTestConsumedValidator(t)
	cases := map[string]struct {
		eventType string
		payload   string
	}{
		"MembershipRevoked missing user_id": {"MembershipRevoked", `{"tenant_id":"` + uuid.NewString() + `"}`},
		"MembershipRevoked non-uuid tenant": {"MembershipRevoked", `{"tenant_id":"nope","user_id":"` + uuid.NewString() + `"}`},
		// google/uuid would happily parse the 32-hex form; the contract is canonical.
		"MembershipRevoked non-canonical uuid": {"MembershipRevoked", `{"tenant_id":"` + uuid.NewString() + `","user_id":"` + strings.ReplaceAll(uuid.NewString(), "-", "") + `"}`},
		"TenantMembershipsPurged no tenant_id": {"TenantMembershipsPurged", `{"actor_id":"` + uuid.NewString() + `"}`},
		"UserUpdated empty changed_fields":     {"UserUpdated", `{"user_id":"` + uuid.NewString() + `","changed_fields":[]}`},
		"UserUpdated unknown status":           {"UserUpdated", `{"user_id":"` + uuid.NewString() + `","changed_fields":["status"],"status":"frozen"}`},
		"UserUpdated not JSON":                 {"UserUpdated", `{not json`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := v.Validate(tc.eventType, json.RawMessage(tc.payload))
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrSchemaViolation)
		})
	}
}

// Unknown types pass — the consumer acks them itself; validation must not
// turn forward-compatible unknown events into DLQ traffic.
func TestConsumedValidator_UnknownTypePasses(t *testing.T) {
	assert.NoError(t, newTestConsumedValidator(t).Validate("SomethingNew", json.RawMessage(`{"x":1}`)))
}

// Produced and consumed schemas must never overlap: GlueCodec resolves
// every ByEventType entry in THIS service's registry, and a consumed type
// there would mean registering another service's schema.
func TestEventschema_ProducedAndConsumedAreDisjoint(t *testing.T) {
	assert.Len(t, eventschema.Consumed, 3)
	for name := range eventschema.Consumed {
		_, clash := eventschema.ByEventType[name]
		assert.False(t, clash, "%s is both produced and consumed", name)
	}
}
