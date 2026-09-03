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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestGlueClient points a real *glue.Client at a local httptest.Server
// serving the AWS JSON 1.1 protocol Glue actually uses — AnonymousCredentials
// skips SigV4 signing, so the server only needs to return the expected
// response shape, no real AWS account or credentials required.
func newTestGlueClient(t *testing.T, handler http.HandlerFunc) *glue.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return glue.New(glue.Options{
		Region:           "ap-south-1",
		Credentials:      aws.AnonymousCredentials{},
		BaseEndpoint:     aws.String(server.URL),
		RetryMaxAttempts: 1, // fail fast in tests — no exponential-backoff retries
	})
}

func TestNewGlueCodec_PrefetchesAllSchemaVersions(t *testing.T) {
	versionID := uuid.New().String()
	client := newTestGlueClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_ = json.NewEncoder(w).Encode(map[string]string{"SchemaVersionId": versionID})
	})

	codec, err := NewGlueCodec(context.Background(), client, "iam-delegation-events",
		[]string{"DelegationStarted", "DelegationEnded"})
	require.NoError(t, err)
	require.NotNil(t, codec)

	gotStarted, err := codec.versionID(context.Background(), "DelegationStarted")
	require.NoError(t, err)
	assert.Equal(t, versionID, gotStarted)
}

func TestNewGlueCodec_FetchFailure_ReturnsDescriptiveError(t *testing.T) {
	client := newTestGlueClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"__type":  "EntityNotFoundException",
			"Message": "Schema not found",
		})
	})

	_, err := NewGlueCodec(context.Background(), client, "iam-delegation-events", []string{"DelegationStarted"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DelegationStarted")
	assert.Contains(t, err.Error(), "iam-delegation-events")
}

func TestNewGlueCodec_NilSchemaVersionID_Errors(t *testing.T) {
	client := newTestGlueClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_ = json.NewEncoder(w).Encode(map[string]string{})
	})

	_, err := NewGlueCodec(context.Background(), client, "iam-delegation-events", []string{"DelegationStarted"})
	require.Error(t, err)
}

func TestGlueCodec_VersionID_CacheMiss_FetchesFromGlue(t *testing.T) {
	versionID := uuid.New().String()
	var gotSchemaName string
	client := newTestGlueClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			SchemaID struct {
				SchemaName string `json:"SchemaName"`
			} `json:"SchemaId"`
		}
		_ = json.Unmarshal(body, &req)
		gotSchemaName = req.SchemaID.SchemaName
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_ = json.NewEncoder(w).Encode(map[string]string{"SchemaVersionId": versionID})
	})

	g := &GlueCodec{client: client, registryName: "iam-delegation-events", versionCache: map[string]string{}}
	got, err := g.versionID(context.Background(), "DelegationReviewRequested")
	require.NoError(t, err)
	assert.Equal(t, versionID, got)
	assert.Equal(t, "DelegationReviewRequested", gotSchemaName)

	// Second call must hit the now-populated cache, not the server again.
	client2 := newTestGlueClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("Glue client must not be called again once the version is cached")
	})
	g.client = client2
	got2, err := g.versionID(context.Background(), "DelegationReviewRequested")
	require.NoError(t, err)
	assert.Equal(t, versionID, got2)
}

func TestGlueCodec_Encode_CacheMiss_FetchesThenEncodes(t *testing.T) {
	versionID := uuid.New().String()
	client := newTestGlueClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_ = json.NewEncoder(w).Encode(map[string]string{"SchemaVersionId": versionID})
	})
	g := &GlueCodec{client: client, registryName: "iam-delegation-events", versionCache: map[string]string{}}

	payload := json.RawMessage(`{"a":1}`)
	encoded, gotVersionID, err := g.Encode(context.Background(), "DelegationEscalationRequested", payload)
	require.NoError(t, err)
	assert.Equal(t, versionID, gotVersionID)
	decoded, err := stripGlueHeader(encoded)
	require.NoError(t, err)
	assert.JSONEq(t, string(payload), string(decoded))
}

func TestGlueCodec_Encode_FetchFailure_Propagates(t *testing.T) {
	client := newTestGlueClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	g := &GlueCodec{client: client, registryName: "iam-delegation-events", versionCache: map[string]string{}}

	_, _, err := g.Encode(context.Background(), "DelegationStarted", json.RawMessage(`{}`))
	require.Error(t, err)
}

func TestGlueCodec_StartRefresher_RefetchesOnTick(t *testing.T) {
	staleID := uuid.New().String()
	freshID := uuid.New().String()
	var calls atomic.Int64
	client := newTestGlueClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_ = json.NewEncoder(w).Encode(map[string]string{"SchemaVersionId": freshID})
	})
	log := &fakeLogger{}
	g := (&GlueCodec{client: client, registryName: "iam-delegation-events",
		versionCache: map[string]string{"DelegationStarted": staleID}}).WithLogger(log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g.StartRefresher(ctx, 10*time.Millisecond)

	require.Eventually(t, func() bool {
		g.mu.RLock()
		defer g.mu.RUnlock()
		return g.versionCache["DelegationStarted"] == freshID
	}, time.Second, 5*time.Millisecond, "expected the refresher to replace the stale cached version")
	assert.GreaterOrEqual(t, calls.Load(), int64(1))
}

func TestGlueCodec_StartRefresher_FetchFailure_KeepsStaleAndLogs(t *testing.T) {
	staleID := uuid.New().String()
	client := newTestGlueClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	log := &fakeLogger{}
	g := (&GlueCodec{client: client, registryName: "iam-delegation-events",
		versionCache: map[string]string{"DelegationStarted": staleID}}).WithLogger(log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g.StartRefresher(ctx, 10*time.Millisecond)

	require.Eventually(t, func() bool {
		return log.warnCalls.Load() > 0
	}, time.Second, 5*time.Millisecond, "expected a refresh-failure warning to be logged")

	g.mu.RLock()
	defer g.mu.RUnlock()
	assert.Equal(t, staleID, g.versionCache["DelegationStarted"], "a failed refresh must keep the stale cached ID")
}
