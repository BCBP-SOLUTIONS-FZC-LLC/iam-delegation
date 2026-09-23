package eventbus

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/eventschema"
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

// byDefinitionRequest is the GetSchemaByDefinition request body.
type byDefinitionRequest struct {
	SchemaID struct {
		SchemaName   string `json:"SchemaName"`
		RegistryName string `json:"RegistryName"`
	} `json:"SchemaId"`
	SchemaDefinition string `json:"SchemaDefinition"`
}

// byDefinitionServer answers GetSchemaByDefinition with versionID/status and
// fails the test on any other Glue action — in particular the retired
// GetSchemaVersion(LatestVersion) lookup. Every request is sent to reqs.
func byDefinitionServer(t *testing.T, versionID, status string, reqs chan<- byDefinitionRequest) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if target := r.Header.Get("X-Amz-Target"); target != "AWSGlue.GetSchemaByDefinition" {
			t.Errorf("unexpected Glue action %q — only GetSchemaByDefinition is allowed", target)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req byDefinitionRequest
		_ = json.Unmarshal(body, &req)
		if reqs != nil {
			reqs <- req
		}
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_ = json.NewEncoder(w).Encode(map[string]string{"SchemaVersionId": versionID, "Status": status})
	}
}

func TestNewGlueCodec_ResolvesEachSchemaByItsEmbeddedDefinition(t *testing.T) {
	versionID := uuid.New().String()
	reqs := make(chan byDefinitionRequest, 8)
	client := newTestGlueClient(t, byDefinitionServer(t, versionID, "AVAILABLE", reqs))

	names := []string{"DelegationStarted", "DelegationEnded", "DelegationReviewRequested", "DelegationEscalationRequested"}
	codec, err := NewGlueCodec(context.Background(), client, "iam-delegation-events", names)
	require.NoError(t, err)
	close(reqs)

	seen := map[string]bool{}
	for req := range reqs {
		seen[req.SchemaID.SchemaName] = true
		assert.Equal(t, "iam-delegation-events", req.SchemaID.RegistryName)
		want, err := registeredDefinition(eventschema.ByEventType[req.SchemaID.SchemaName])
		require.NoError(t, err)
		assert.Equal(t, want, req.SchemaDefinition, "%s: must send the compact, ASCII-escaped embedded schema", req.SchemaID.SchemaName)
	}
	assert.Len(t, seen, len(names))

	got, err := codec.versionID(context.Background(), "DelegationStarted")
	require.NoError(t, err)
	assert.Equal(t, versionID, got)
}

// Resolution happens once at startup — Encode never calls Glue afterwards.
func TestNewGlueCodec_ResolvesOnce_EncodeMakesNoGlueCall(t *testing.T) {
	var calls atomic.Int64
	versionID := uuid.New().String()
	inner := byDefinitionServer(t, versionID, "AVAILABLE", nil)
	client := newTestGlueClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		inner(w, r)
	})

	codec, err := NewGlueCodec(context.Background(), client, "iam-delegation-events", []string{"DelegationStarted"})
	require.NoError(t, err)
	for range 3 {
		_, vid, err := codec.Encode(context.Background(), "DelegationStarted", json.RawMessage(`{}`))
		require.NoError(t, err)
		assert.Equal(t, versionID, vid)
	}
	assert.EqualValues(t, 1, calls.Load())
}

func TestNewGlueCodec_NotAvailableVersion_Errors(t *testing.T) {
	for _, status := range []string{"PENDING", "FAILURE", "DELETING"} {
		t.Run(status, func(t *testing.T) {
			client := newTestGlueClient(t, byDefinitionServer(t, uuid.New().String(), status, nil))
			_, err := NewGlueCodec(context.Background(), client, "iam-delegation-events", []string{"DelegationStarted"})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "not AVAILABLE")
		})
	}
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
	assert.Contains(t, err.Error(), "isn't registered yet")
}

func TestNewGlueCodec_NilSchemaVersionID_Errors(t *testing.T) {
	client := newTestGlueClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_ = json.NewEncoder(w).Encode(map[string]string{"Status": "AVAILABLE"})
	})

	_, err := NewGlueCodec(context.Background(), client, "iam-delegation-events", []string{"DelegationStarted"})
	require.Error(t, err)
}

// An event type with no embedded schema fails before any Glue call.
func TestNewGlueCodec_NoEmbeddedSchema_ErrorsWithoutCallingGlue(t *testing.T) {
	client := newTestGlueClient(t, func(http.ResponseWriter, *http.Request) {
		t.Error("Glue must not be called for an event type with no embedded schema")
	})
	_, err := NewGlueCodec(context.Background(), client, "iam-delegation-events", []string{"NotAnEvent"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no embedded schema")
}

func TestGlueCodec_VersionID_CacheMiss_FetchesFromGlue(t *testing.T) {
	versionID := uuid.New().String()
	reqs := make(chan byDefinitionRequest, 1)
	client := newTestGlueClient(t, byDefinitionServer(t, versionID, "AVAILABLE", reqs))

	g := &GlueCodec{client: client, registryName: "iam-delegation-events", definitions: eventschema.ByEventType, versionCache: map[string]string{}}
	got, err := g.versionID(context.Background(), "DelegationReviewRequested")
	require.NoError(t, err)
	assert.Equal(t, versionID, got)
	assert.Equal(t, "DelegationReviewRequested", (<-reqs).SchemaID.SchemaName)

	// Second call must hit the now-populated cache, not the server again.
	g.client = newTestGlueClient(t, func(http.ResponseWriter, *http.Request) {
		t.Error("Glue client must not be called again once the version is cached")
	})
	got2, err := g.versionID(context.Background(), "DelegationReviewRequested")
	require.NoError(t, err)
	assert.Equal(t, versionID, got2)
}

func TestGlueCodec_Encode_CacheMiss_FetchesThenEncodes(t *testing.T) {
	versionID := uuid.New().String()
	client := newTestGlueClient(t, byDefinitionServer(t, versionID, "AVAILABLE", nil))
	g := &GlueCodec{client: client, registryName: "iam-delegation-events", definitions: eventschema.ByEventType, versionCache: map[string]string{}}

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
	g := &GlueCodec{client: client, registryName: "iam-delegation-events", definitions: eventschema.ByEventType, versionCache: map[string]string{}}

	_, _, err := g.Encode(context.Background(), "DelegationStarted", json.RawMessage(`{}`))
	require.Error(t, err)
}

func TestRegisteredDefinition_InvalidJSON_Errors(t *testing.T) {
	_, err := registeredDefinition([]byte(`{not json`))
	require.Error(t, err)
}

func TestAsciiEscape_MatchesPythonEnsureASCII(t *testing.T) {
	assert.Equal(t, `{"d":"caf\u00e9 \u2014 \ud83d\ude00"}`, asciiEscape([]byte(`{"d":"café — 😀"}`)))
	assert.Equal(t, `{"a":1}`, asciiEscape([]byte(`{"a":1}`)))
}

// TestRegisteredDefinition_MatchesSchemaGov pins registeredDefinition against
// the exact serialization schema-gov register uploads (Python's
// json.dumps(json.load(f), separators=(",", ":"))) for every produced schema
// — these carry non-ASCII ("§", "—"), so ensure_ascii escaping matters.
func TestRegisteredDefinition_MatchesSchemaGov(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	for name, raw := range eventschema.ByEventType {
		t.Run(name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), python, "-c",
				`import json,sys; sys.stdout.write(json.dumps(json.load(sys.stdin), separators=(",", ":")))`)
			stdin, err := cmd.StdinPipe()
			require.NoError(t, err)
			go func() { _, _ = stdin.Write(raw); _ = stdin.Close() }()
			want, err := cmd.Output()
			require.NoError(t, err)
			got, err := registeredDefinition(raw)
			require.NoError(t, err)
			assert.Equal(t, string(want), got)
		})
	}
}
