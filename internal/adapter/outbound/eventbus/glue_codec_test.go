package eventbus

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGlueCodec_Encode_CacheHit_NeverTouchesClient confirms Encode's happy
// path — the schema version ID is already cached (the common case after
// NewGlueCodec's startup resolution) — never calls the Glue client at all,
// so it's testable without any AWS mocking.
func TestGlueCodec_Encode_CacheHit_NeverTouchesClient(t *testing.T) {
	versionID := uuid.New().String()
	g := &GlueCodec{versionCache: map[string]string{"DelegationStarted": versionID}}

	payload := json.RawMessage(`{"delegation_id":"018f4c3e-a1b2-7000-9d3e-4f8c1a2b3d52"}`)
	encoded, gotVersionID, err := g.Encode(context.Background(), "DelegationStarted", payload)
	require.NoError(t, err)
	assert.Equal(t, versionID, gotVersionID)

	decoded, err := stripGlueHeader(encoded)
	require.NoError(t, err)
	assert.JSONEq(t, string(payload), string(decoded))
}

func TestGlueCodec_VersionID_CacheHit(t *testing.T) {
	versionID := uuid.New().String()
	g := &GlueCodec{versionCache: map[string]string{"DelegationEnded": versionID}}

	got, err := g.versionID(context.Background(), "DelegationEnded")
	require.NoError(t, err)
	assert.Equal(t, versionID, got)
}
