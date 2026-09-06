package eventbus

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGlueDecodeCodec_Decode_StripsHeader confirms GlueDecodeCodec correctly
// decodes wire bytes produced by the real Glue wire format — the shape Core
// (iam-org-membership) actually emits when its own GLUE_REGISTRY_MEMBERSHIP_NAME
// is set (LLD §10.1, DLG-D21), regardless of this service's own outbound
// Glue configuration.
func TestGlueDecodeCodec_Decode_StripsHeader(t *testing.T) {
	payload := json.RawMessage(`{"tenant_id":"018f4c3e-a1b2-7000-9d3e-4f8c1a2b3d52"}`)
	schemaVersionID := uuid.New().String()
	encoded, err := prependGlueHeader(schemaVersionID, payload)
	require.NoError(t, err)

	decoded, err := GlueDecodeCodec{}.Decode(context.Background(), schemaVersionID, encoded)
	require.NoError(t, err)
	assert.JSONEq(t, string(payload), string(decoded))
}

// TestGlueDecodeCodec_Decode_MatchesGlueCodec confirms the shared
// stripGlueHeader refactor didn't change GlueCodec's own decode behavior —
// both codecs must agree byte-for-byte on the same wire input.
func TestGlueDecodeCodec_Decode_MatchesGlueCodec(t *testing.T) {
	payload := json.RawMessage(`{"a":1}`)
	encoded, err := prependGlueHeader(uuid.New().String(), payload)
	require.NoError(t, err)

	g := &GlueCodec{}
	fromGlueCodec, err := g.Decode(context.Background(), "", encoded)
	require.NoError(t, err)
	fromDecodeCodec, err := GlueDecodeCodec{}.Decode(context.Background(), "", encoded)
	require.NoError(t, err)

	assert.Equal(t, fromGlueCodec, fromDecodeCodec)
}

func TestGlueDecodeCodec_Decode_TooShort(t *testing.T) {
	_, err := GlueDecodeCodec{}.Decode(context.Background(), "", []byte{0x03, 0x00})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shorter than")
}

func TestGlueDecodeCodec_Decode_WrongHeaderVersion(t *testing.T) {
	encoded, err := prependGlueHeader(uuid.New().String(), json.RawMessage(`{}`))
	require.NoError(t, err)
	encoded[0] = 0x99 // corrupt the header version byte

	_, err = GlueDecodeCodec{}.Decode(context.Background(), "", encoded)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected header version")
}

// TestGlueDecodeCodec_Encode_AlwaysErrors confirms this codec cannot be
// mistakenly wired as a publish-time codec — only GlueCodec may publish.
// TestPrependGlueHeader_InvalidUUID covers lines 275–277: uuid.Parse fails
// when schemaVersionID is not a valid UUID.
func TestPrependGlueHeader_InvalidUUID_Errors(t *testing.T) {
	_, err := prependGlueHeader("not-a-valid-uuid", []byte(`{}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse schema version UUID")
}

// TestGlueCodec_Encode_BadCachedVersionID covers lines 131–133: GlueCodec.Encode
// returns an error when the cached version ID is not a valid UUID (prependGlueHeader fails).
func TestGlueCodec_Encode_BadCachedVersionID_Errors(t *testing.T) {
	g := &GlueCodec{
		versionCache: map[string]string{
			"DelegationStarted": "not-a-valid-uuid",
		},
	}
	_, _, err := g.Encode(context.Background(), "DelegationStarted", json.RawMessage(`{}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse schema version UUID")
}

func TestGlueDecodeCodec_Encode_AlwaysErrors(t *testing.T) {
	_, _, err := GlueDecodeCodec{}.Encode(context.Background(), "MembershipRevoked", json.RawMessage(`{}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode-only")
}
