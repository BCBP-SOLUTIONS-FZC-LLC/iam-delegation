package tender

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
)

func TestNewHTTPClient_EmptyBaseURL_Errors(t *testing.T) {
	_, err := NewHTTPClient("", nil, 0)
	require.Error(t, err)

	_, err = NewHTTPClient("   ", nil, 0)
	require.Error(t, err)
}

func TestHTTPClient_CheckLive_ExistsAndLive(t *testing.T) {
	tenantID, scopeID := uuid.New(), uuid.New()
	var gotPath, gotUserID, gotTenantID, gotTenantRoles string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUserID = r.Header.Get("x-user-id")
		gotTenantID = r.Header.Get("x-tenant-id")
		gotTenantRoles = r.Header.Get("x-tenant-roles")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"exists": true, "live": true})
	}))
	defer server.Close()

	client, err := NewHTTPClient(server.URL, nil, 0)
	require.NoError(t, err)

	exists, live, err := client.CheckLive(t.Context(), tenantID, scopeID)
	require.NoError(t, err)
	assert.True(t, exists)
	assert.True(t, live)
	assert.Equal(t, "/api/v1/internal/tenants/"+tenantID.String()+"/tenders/"+scopeID.String()+"/exists", gotPath)
	assert.Equal(t, "iam-system", gotUserID)
	assert.Equal(t, tenantID.String(), gotTenantID)
	assert.Equal(t, "iam-system", gotTenantRoles)
}

func TestHTTPClient_CheckLive_ExistsButNotLive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"exists": true, "live": false})
	}))
	defer server.Close()

	client, err := NewHTTPClient(server.URL, nil, 0)
	require.NoError(t, err)
	exists, live, err := client.CheckLive(t.Context(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.True(t, exists)
	assert.False(t, live)
}

// TestHTTPClient_CheckLive_404_NormalizedToNotExists covers LLD §7.6.7's
// "{exists: false} | HTTP 404" — a 404 is a business answer, not a
// dependency failure.
func TestHTTPClient_CheckLive_404_NormalizedToNotExists(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := NewHTTPClient(server.URL, nil, 0)
	require.NoError(t, err)
	exists, live, err := client.CheckLive(t.Context(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.False(t, exists)
	assert.False(t, live)
}

func TestHTTPClient_CheckLive_5xx_WrapsErrDependencyUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client, err := NewHTTPClient(server.URL, nil, 0)
	require.NoError(t, err)
	exists, live, err := client.CheckLive(t.Context(), uuid.New(), uuid.New())
	require.Error(t, err)
	assert.False(t, exists)
	assert.False(t, live)
	assert.True(t, errors.Is(err, port.ErrDependencyUnavailable))
}

func TestHTTPClient_CheckLive_Timeout_WrapsErrDependencyUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"exists": true, "live": true})
	}))
	defer server.Close()

	client, err := NewHTTPClient(server.URL, nil, 10*time.Millisecond)
	require.NoError(t, err)
	_, _, err = client.CheckLive(t.Context(), uuid.New(), uuid.New())
	require.Error(t, err)
	assert.True(t, errors.Is(err, port.ErrDependencyUnavailable))
}

func TestHTTPClient_CheckLive_NetworkError_WrapsErrDependencyUnavailable(t *testing.T) {
	client, err := NewHTTPClient("http://127.0.0.1:1", nil, 50*time.Millisecond)
	require.NoError(t, err)
	_, _, err = client.CheckLive(t.Context(), uuid.New(), uuid.New())
	require.Error(t, err)
	assert.True(t, errors.Is(err, port.ErrDependencyUnavailable))
}

// TestHTTPClient_CheckLive_4xx_NonDependencyError verifies a non-404 4xx
// response is returned as a plain error without the ErrDependencyUnavailable
// sentinel.
func TestHTTPClient_CheckLive_4xx_NonDependencyError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	client, err := NewHTTPClient(server.URL, nil, 0)
	require.NoError(t, err)
	exists, live, err := client.CheckLive(t.Context(), uuid.New(), uuid.New())
	require.Error(t, err)
	assert.False(t, exists)
	assert.False(t, live)
	assert.False(t, errors.Is(err, port.ErrDependencyUnavailable), "a non-404 4xx must not be wrapped as dependency unavailable")
	assert.Contains(t, err.Error(), "400")
}

func TestHTTPClient_CheckLive_200_InvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not-json"))
	}))
	defer server.Close()

	client, err := NewHTTPClient(server.URL, nil, 0)
	require.NoError(t, err)
	_, _, err = client.CheckLive(t.Context(), uuid.New(), uuid.New())
	require.Error(t, err)
}

// TestHTTPClient_CheckLive_InvalidURL covers http.NewRequestWithContext
// failing when the constructed URL is invalid (null byte in baseURL).
func TestHTTPClient_CheckLive_InvalidURL_Errors(t *testing.T) {
	client := &HTTPClient{baseURL: "http://host\x00", httpClient: &http.Client{}}
	_, _, err := client.CheckLive(t.Context(), uuid.New(), uuid.New())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "build request")
}
