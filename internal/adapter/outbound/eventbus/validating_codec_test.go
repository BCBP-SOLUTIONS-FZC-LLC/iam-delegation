package eventbus

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/eventschema"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

func TestNewValidatingCodec_CompilesAllEmbeddedSchemas(t *testing.T) {
	c, err := NewValidatingCodec(events.NoopCodec{})
	require.NoError(t, err)
	assert.Len(t, c.schemas, len(eventschema.ByEventType))
}

func TestValidatingCodec_Encode_ValidPayloadPasses(t *testing.T) {
	c, err := NewValidatingCodec(events.NoopCodec{})
	require.NoError(t, err)

	payload := []byte(`{
		"delegation_id": "11111111-1111-1111-1111-111111111111",
		"delegator_id":  "22222222-2222-2222-2222-222222222222",
		"delegate_id":   "33333333-3333-3333-3333-333333333333",
		"scope":         "all",
		"starts_at":     "2026-01-01T00:00:00Z"
	}`)
	out, _, err := c.Encode(context.Background(), domain.EventDelegationStarted, payload)
	require.NoError(t, err)
	assert.Equal(t, payload, out)
}

func TestValidatingCodec_Encode_MissingRequiredFieldFailsClosed(t *testing.T) {
	c, err := NewValidatingCodec(events.NoopCodec{})
	require.NoError(t, err)

	payload := []byte(`{"delegation_id":"11111111-1111-1111-1111-111111111111"}`)
	_, _, err = c.Encode(context.Background(), domain.EventDelegationStarted, payload)
	require.Error(t, err)
	assert.Contains(t, err.Error(), domain.EventDelegationStarted)
}

func TestValidatingCodec_Encode_MalformedJSONFailsClosed(t *testing.T) {
	c, err := NewValidatingCodec(events.NoopCodec{})
	require.NoError(t, err)

	_, _, err = c.Encode(context.Background(), domain.EventDelegationStarted, []byte(`not json`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not JSON")
}

// TestValidatingCodec_Encode_UnregisteredEventTypePassesThrough confirms a
// consumed-only event type (no entry in eventschema.ByEventType) is treated
// as a no-op pass-through rather than rejected — matching iam-realm-provisioner.
func TestValidatingCodec_Encode_UnregisteredEventTypePassesThrough(t *testing.T) {
	c, err := NewValidatingCodec(events.NoopCodec{})
	require.NoError(t, err)

	payload := []byte(`{"anything":"goes"}`)
	out, _, err := c.Encode(context.Background(), domain.EventMembershipRevoked, payload)
	require.NoError(t, err)
	assert.Equal(t, payload, out)
}

func TestNewValidatingCodec_NilInnerUsesEventsNoopCodec(t *testing.T) {
	c, err := NewValidatingCodec(nil)
	require.NoError(t, err)
	payload := []byte(`{"anything":"goes"}`)
	out, _, err := c.Encode(context.Background(), domain.EventMembershipRevoked, payload)
	require.NoError(t, err)
	assert.Equal(t, payload, out)
}

func TestValidatingCodec_Decode_DelegatesToInner(t *testing.T) {
	c, err := NewValidatingCodec(events.NoopCodec{})
	require.NoError(t, err)
	raw := []byte(`{"k":"v"}`)
	got, err := c.Decode(context.Background(), "", raw)
	require.NoError(t, err)
	assert.Equal(t, json.RawMessage(raw), got)
}
