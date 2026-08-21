package userprofile

import (
	"encoding/json"
	"errors"
	"io"
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
}

func TestBuildBody_DelegateIDThreeStates(t *testing.T) {
	delegateID := uuid.New()

	// Omitted entirely.
	body := buildBody(port.SetAvailabilityRequest{})
	_, ok := body["delegate_id"]
	assert.False(t, ok)

	// Explicit null.
	body = buildBody(port.SetAvailabilityRequest{ClearDelegate: true})
	val, ok := body["delegate_id"]
	require.True(t, ok)
	assert.Nil(t, val)

	// Explicit value.
	body = buildBody(port.SetAvailabilityRequest{DelegateID: &delegateID})
	assert.Equal(t, delegateID.String(), body["delegate_id"])
}

func TestHTTPClient_SetAvailability_Create(t *testing.T) {
	tenantID, userID, delegateID := uuid.New(), uuid.New(), uuid.New()
	var gotPath, gotMethod, gotUserID, gotUserRoles string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotUserID = r.Header.Get("X-User-Id")
		gotUserRoles = r.Header.Get("X-User-Roles")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c, err := NewHTTPClient(server.URL, nil, 0)
	require.NoError(t, err)

	status := "ooo"
	from := time.Now().UTC()
	err = c.SetAvailability(t.Context(), port.SetAvailabilityRequest{
		TenantID: tenantID, UserID: userID,
		Status: &status, OOOFrom: &from, DelegateID: &delegateID, Note: "on leave",
	})
	require.NoError(t, err)

	assert.Equal(t, http.MethodPut, gotMethod)
	assert.Equal(t, "/internal/users/"+userID.String()+"/availability", gotPath)
	assert.Equal(t, "iam-system", gotUserID)
	assert.Equal(t, "iam-system", gotUserRoles)
	assert.Equal(t, "ooo", gotBody["status"])
	assert.Equal(t, delegateID.String(), gotBody["delegate_id"])
	assert.Equal(t, "on leave", gotBody["note"])
	_, hasUntil := gotBody["ooo_until"]
	assert.False(t, hasUntil)
}

// TestHTTPClient_SetAvailability_End covers DEL-6's pointer-clear-only end
// path: {delegate_id: null}, no status field at all.
func TestHTTPClient_SetAvailability_End(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c, err := NewHTTPClient(server.URL, nil, 0)
	require.NoError(t, err)

	err = c.SetAvailability(t.Context(), port.SetAvailabilityRequest{
		TenantID: uuid.New(), UserID: uuid.New(), ClearDelegate: true,
	})
	require.NoError(t, err)

	val, ok := gotBody["delegate_id"]
	require.True(t, ok, "delegate_id must be present (explicit null)")
	assert.Nil(t, val)
	_, hasStatus := gotBody["status"]
	assert.False(t, hasStatus, "end must never send status:\"available\" — UP owns that transition")
}

// TestHTTPClient_SetAvailability_5xx_WrapsErrDependencyUnavailable covers
// the ground-truth rule: 5xx/timeout wrapped with port.ErrDependencyUnavailable.
func TestHTTPClient_SetAvailability_5xx_WrapsErrDependencyUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	c, err := NewHTTPClient(server.URL, nil, 0)
	require.NoError(t, err)
	err = c.SetAvailability(t.Context(), port.SetAvailabilityRequest{TenantID: uuid.New(), UserID: uuid.New(), ClearDelegate: true})
	require.Error(t, err)
	assert.True(t, errors.Is(err, port.ErrDependencyUnavailable))
}

// TestHTTPClient_SetAvailability_422_CarriesErrorCode covers the ground-truth
// rule: a 4xx business rejection must NOT be wrapped with
// port.ErrDependencyUnavailable, and its .Error() text must contain the
// callee's error code so delegation_service.go's
// strings.Contains(err.Error(), "delegate_unavailable") check still works.
func TestHTTPClient_SetAvailability_422_CarriesErrorCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "delegate_unavailable", "status": 422, "trace_id": "t1", "request_id": "r1",
		})
	}))
	defer server.Close()

	c, err := NewHTTPClient(server.URL, nil, 0)
	require.NoError(t, err)
	err = c.SetAvailability(t.Context(), port.SetAvailabilityRequest{TenantID: uuid.New(), UserID: uuid.New(), ClearDelegate: true})
	require.Error(t, err)
	assert.False(t, errors.Is(err, port.ErrDependencyUnavailable), "4xx business rejection must not be wrapped")
	assert.Contains(t, err.Error(), "delegate_unavailable")
}

func TestHTTPClient_SetAvailability_NetworkError_WrapsErrDependencyUnavailable(t *testing.T) {
	c, err := NewHTTPClient("http://127.0.0.1:1", nil, 50*time.Millisecond)
	require.NoError(t, err)
	err = c.SetAvailability(t.Context(), port.SetAvailabilityRequest{TenantID: uuid.New(), UserID: uuid.New(), ClearDelegate: true})
	require.Error(t, err)
	assert.True(t, errors.Is(err, port.ErrDependencyUnavailable))
}
