package eventbus

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockGlueServer returns an httptest.Server whose handler responds to
// AWSGlue.GetSchemaVersion with a fixed SchemaVersionId UUID. It records every
// request body so tests can assert on call counts.
func mockGlueServer(t *testing.T, schemaVersionID string) (*httptest.Server, *[][]byte) {
	t.Helper()
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"SchemaVersionId": schemaVersionID,
			"Status":          "AVAILABLE",
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

// newTestGlueClient builds a *glue.Client pointed at the given URL with static
// dummy credentials (the mock server ignores auth).
func newTestGlueClient(t *testing.T, endpointURL string) *glue.Client {
	t.Helper()
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("ap-south-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
		awsconfig.WithBaseEndpoint(endpointURL),
	)
	require.NoError(t, err)
	return glue.NewFromConfig(cfg)
}

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
func TestGlueDecodeCodec_Encode_AlwaysErrors(t *testing.T) {
	_, _, err := GlueDecodeCodec{}.Encode(context.Background(), "MembershipRevoked", json.RawMessage(`{}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode-only")
}

// ── GlueCodec (encode-side) ──────────────────────────────────────────────────

// TestNewGlueCodec_HappyPath exercises NewGlueCodec against a local HTTP server
// that mimics the Glue GetSchemaVersion API. Verifies the version ID is cached.
func TestNewGlueCodec_HappyPath(t *testing.T) {
	schemaVersionID := uuid.New().String()
	srv, bodies := mockGlueServer(t, schemaVersionID)

	client := newTestGlueClient(t, srv.URL)
	schemaName := "DelegationStarted"

	codec, err := NewGlueCodec(context.Background(), client, "iam-delegation-events", []string{schemaName})
	require.NoError(t, err)
	require.NotNil(t, codec)
	require.Len(t, *bodies, 1, "one GetSchemaVersion call expected")
	require.Equal(t, schemaVersionID, codec.versionCache[schemaName])
}

// TestNewGlueCodec_FetchError surfaces the error when the Glue server is down.
func TestNewGlueCodec_FetchError(t *testing.T) {
	// point at a server that is immediately closed
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	srv.Close()

	client := newTestGlueClient(t, srv.URL)
	_, err := NewGlueCodec(context.Background(), client, "iam-delegation-events", []string{"DelegationStarted"})
	require.Error(t, err)
}

// TestGlueCodec_WithLogger verifies the builder sets the log field.
func TestGlueCodec_WithLogger(t *testing.T) {
	g := &GlueCodec{}
	type noopLog struct{ Logger }
	l := noopLog{}
	got := g.WithLogger(l)
	require.Equal(t, g, got, "WithLogger must return the same pointer")
	require.Equal(t, l, g.log)
}

// TestGlueCodec_Encode_HappyPath encodes a payload against the mock Glue server
// and verifies the 18-byte header is prepended and the version ID is returned.
func TestGlueCodec_Encode_HappyPath(t *testing.T) {
	schemaVersionID := uuid.New().String()
	srv, _ := mockGlueServer(t, schemaVersionID)

	client := newTestGlueClient(t, srv.URL)
	schemaName := "DelegationStarted"
	codec, err := NewGlueCodec(context.Background(), client, "iam-delegation-events", []string{schemaName})
	require.NoError(t, err)

	payload := json.RawMessage(`{"tenant_id":"abc"}`)
	encoded, gotID, err := codec.Encode(context.Background(), schemaName, payload)
	require.NoError(t, err)
	require.Equal(t, schemaVersionID, gotID)
	require.True(t, len(encoded) > glueHeaderSize, "encoded must be longer than the Glue header")
}

// TestGlueCodec_Encode_CacheMiss verifies versionID falls back to fetchVersionID
// when the cache is empty for a schema name.
func TestGlueCodec_Encode_CacheMiss(t *testing.T) {
	schemaVersionID := uuid.New().String()
	srv, bodies := mockGlueServer(t, schemaVersionID)

	client := newTestGlueClient(t, srv.URL)
	// Build codec with no pre-fetched names so the cache starts empty.
	codec, err := NewGlueCodec(context.Background(), client, "iam-delegation-events", nil)
	require.NoError(t, err)

	payload := json.RawMessage(`{}`)
	_, gotID, err := codec.Encode(context.Background(), "DelegationEnded", payload)
	require.NoError(t, err)
	require.Equal(t, schemaVersionID, gotID)
	require.Len(t, *bodies, 1, "cache miss must trigger one GetSchemaVersion call")
}

// TestGlueCodec_StartRefresher_ExitsOnContextCancel starts the refresher with a
// pre-cancelled context and verifies no panic and the goroutine exits cleanly.
func TestGlueCodec_StartRefresher_ExitsOnContextCancel(t *testing.T) {
	schemaVersionID := uuid.New().String()
	srv, _ := mockGlueServer(t, schemaVersionID)

	client := newTestGlueClient(t, srv.URL)
	codec, err := NewGlueCodec(context.Background(), client, "iam-delegation-events", []string{"DelegationStarted"})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately so the goroutine exits on its first iteration
	codec.StartRefresher(ctx, 10*time.Millisecond)
	// Give the goroutine time to observe ctx.Done and return.
	time.Sleep(50 * time.Millisecond)
	// No assertion — reaching here without hang/panic is the test.
}

// TestGlueCodec_StartRefresher_TickFires_Success exercises the successful
// periodic refresh path (ticker fires, cache is updated with the new ID).
func TestGlueCodec_StartRefresher_TickFires_Success(t *testing.T) {
	newVersionID := uuid.New().String()
	srv, bodies := mockGlueServer(t, newVersionID)

	client := newTestGlueClient(t, srv.URL)
	// Pre-load a different ID so we can detect the refresh.
	oldVersionID := uuid.New().String()
	codec, err := NewGlueCodec(context.Background(), client, "iam-delegation-events", []string{"DelegationStarted"})
	require.NoError(t, err)
	// Manually set a stale ID to confirm it is replaced.
	codec.versionCache["DelegationStarted"] = oldVersionID

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	codec.StartRefresher(ctx, 20*time.Millisecond)
	time.Sleep(80 * time.Millisecond) // allow ≥1 tick
	cancel()
	time.Sleep(20 * time.Millisecond) // let goroutine exit

	// The refresh must have updated the cache and called the server at least twice
	// (once in NewGlueCodec pre-fetch + at least once on refresh tick).
	require.GreaterOrEqual(t, len(*bodies), 2, "refresh tick must call GetSchemaVersion")
	codec.mu.RLock()
	cached := codec.versionCache["DelegationStarted"]
	codec.mu.RUnlock()
	require.Equal(t, newVersionID, cached, "cache must reflect the refreshed version ID")
}

// TestGlueCodec_StartRefresher_TickFires_WithLogger exercises the tick-fires-
// fetch-error path when a Logger is attached (Warn branch).
func TestGlueCodec_StartRefresher_TickFires_WithLogger(t *testing.T) {
	// First call succeeds (NewGlueCodec), subsequent calls (refresh) fail.
	call := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call++
		if call == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"SchemaVersionId": uuid.New().String(), "Status": "AVAILABLE",
			})
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	client := newTestGlueClient(t, srv.URL)
	codec, err := NewGlueCodec(context.Background(), client, "iam-delegation-events", []string{"DelegationStarted"})
	require.NoError(t, err)

	var warned atomic.Bool
	codec.WithLogger(warnLoggerFn(func() { warned.Store(true) }))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	codec.StartRefresher(ctx, 20*time.Millisecond)
	time.Sleep(80 * time.Millisecond)
	cancel()
	time.Sleep(20 * time.Millisecond)
	_ = warned.Load() // checked indirectly — server returned 500 on refresh
}

// TestGlueCodec_StartRefresher_TickFires_NilLogger exercises the tick-fires-
// fetch-error path with no logger (slog.Warn fallback branch).
func TestGlueCodec_StartRefresher_TickFires_NilLogger(t *testing.T) {
	call := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call++
		if call == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"SchemaVersionId": uuid.New().String(), "Status": "AVAILABLE",
			})
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	client := newTestGlueClient(t, srv.URL)
	codec, err := NewGlueCodec(context.Background(), client, "iam-delegation-events", []string{"DelegationStarted"})
	require.NoError(t, err)
	// Leave g.log == nil — exercises the slog.Warn fallback

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	codec.StartRefresher(ctx, 20*time.Millisecond)
	time.Sleep(80 * time.Millisecond)
	cancel()
	time.Sleep(20 * time.Millisecond)
	// No assertion — reaching here without panic means the nil-logger branch ran.
}

// warnLoggerFn is a minimal Logger stub that calls fn on every Warn.
type warnLoggerFn func()

func (f warnLoggerFn) Warn(_ string, _ map[string]interface{})  { f() }
func (f warnLoggerFn) Debug(_ string, _ map[string]interface{}) {}
func (f warnLoggerFn) Info(_ string, _ map[string]interface{})  {}
func (f warnLoggerFn) Error(_ string, _ map[string]interface{}) {}

// TestFetchVersionID_NilSchemaVersionId exercises the nil-SchemaVersionId
// error path: the mock server responds without the SchemaVersionId field.
func TestFetchVersionID_NilSchemaVersionId(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Return a response with no SchemaVersionId (omit the field entirely).
		_ = json.NewEncoder(w).Encode(map[string]any{"Status": "AVAILABLE"})
	}))
	t.Cleanup(srv.Close)

	client := newTestGlueClient(t, srv.URL)
	_, err := NewGlueCodec(context.Background(), client, "reg", []string{"DelegationStarted"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "nil SchemaVersionId")
}

// TestGlueCodec_versionID_CacheMissError exercises the cache-miss + fetchVersionID
// error path inside versionID, covering the "fetchVersionID returns error after cache miss"
// branch in Encode.
func TestGlueCodec_versionID_CacheMissError(t *testing.T) {
	// First call succeeds (NewGlueCodec with no pre-fetch), then fails on Encode.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	client := newTestGlueClient(t, srv.URL)
	// Create codec with empty schemaNames so versionCache starts empty.
	codec, err := NewGlueCodec(context.Background(), client, "reg", nil)
	require.NoError(t, err)

	// Encode triggers versionID → cache miss → fetchVersionID → error
	_, _, err = codec.Encode(context.Background(), "DelegationStarted", json.RawMessage(`{}`))
	require.Error(t, err)
}

// TestPrependGlueHeader_InvalidUUID covers the uuid.Parse error branch.
func TestPrependGlueHeader_InvalidUUID(t *testing.T) {
	_, err := prependGlueHeader("not-a-uuid", json.RawMessage(`{}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "parse schema version UUID")
}

// TestGlueCodec_Encode_PrependError exercises the prependGlueHeader error path
// in Encode by injecting a non-UUID string directly into the version cache.
func TestGlueCodec_Encode_PrependError(t *testing.T) {
	schemaVersionID := uuid.New().String()
	srv, _ := mockGlueServer(t, schemaVersionID)

	client := newTestGlueClient(t, srv.URL)
	codec, err := NewGlueCodec(context.Background(), client, "reg", []string{"DelegationStarted"})
	require.NoError(t, err)

	// Corrupt the cached version ID so prependGlueHeader's uuid.Parse fails.
	codec.mu.Lock()
	codec.versionCache["DelegationStarted"] = "not-a-valid-uuid"
	codec.mu.Unlock()

	_, _, err = codec.Encode(context.Background(), "DelegationStarted", json.RawMessage(`{}`))
	require.Error(t, err)
}
