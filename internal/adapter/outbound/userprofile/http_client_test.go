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

func TestBuildBody_DelegateIDTwoStates(t *testing.T) {
	delegateID := uuid.New()

	// Omitted entirely.
	body := buildBody(port.SetAvailabilityRequest{})
	_, ok := body["delegate_id"]
	assert.False(t, ok, "delegate_id must be absent when DelegateID is nil")

	// Explicit value.
	body = buildBody(port.SetAvailabilityRequest{DelegateID: &delegateID})
	assert.Equal(t, delegateID.String(), body["delegate_id"])
}

func TestHTTPClient_SetAvailability_Create(t *testing.T) {
	tenantID, userID, delegateID := uuid.New(), uuid.New(), uuid.New()
	var gotPath, gotMethod, gotUserID, gotTenantID, gotTenantRoles string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotUserID = r.Header.Get("x-user-id")
		gotTenantID = r.Header.Get("x-tenant-id")
		gotTenantRoles = r.Header.Get("x-tenant-roles")
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
	assert.Equal(t, "/api/v1/internal/users/"+userID.String()+"/availability", gotPath)
	assert.Equal(t, "iam-system", gotUserID)
	assert.Equal(t, tenantID.String(), gotTenantID)
	assert.Equal(t, "iam-system", gotTenantRoles)
	assert.Equal(t, "ooo", gotBody["status"])
	assert.Equal(t, delegateID.String(), gotBody["delegate_id"])
	assert.Equal(t, "on leave", gotBody["note"])
	_, hasUntil := gotBody["ooo_until"]
	assert.False(t, hasUntil)
}

// TestHTTPClient_ClearDelegatePointer covers DEL-6's pointer-clear end path.
// The dedicated DELETE endpoint is called with no body; UP clears delegate_id
// without touching the user's status (Gap 3 Option B).
func TestHTTPClient_ClearDelegatePointer(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	var gotPath, gotMethod, gotTenantID, gotUserID, gotTenantRoles string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotTenantID = r.Header.Get("x-tenant-id")
		gotUserID = r.Header.Get("x-user-id")
		gotTenantRoles = r.Header.Get("x-tenant-roles")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	c, err := NewHTTPClient(server.URL, nil, 0)
	require.NoError(t, err)

	err = c.ClearDelegatePointer(t.Context(), tenantID, userID)
	require.NoError(t, err)

	assert.Equal(t, http.MethodDelete, gotMethod)
	assert.Equal(t, "/api/v1/internal/users/"+userID.String()+"/availability/delegate", gotPath)
	assert.Equal(t, tenantID.String(), gotTenantID)
	assert.Equal(t, "iam-system", gotUserID)
	assert.Equal(t, "iam-system", gotTenantRoles)
}

// TestHTTPClient_ClearDelegatePointer_5xx_WrapsErrDependencyUnavailable checks
// that 5xx from UP is wrapped with port.ErrDependencyUnavailable.
func TestHTTPClient_ClearDelegatePointer_5xx_WrapsErrDependencyUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	c, err := NewHTTPClient(server.URL, nil, 0)
	require.NoError(t, err)
	err = c.ClearDelegatePointer(t.Context(), uuid.New(), uuid.New())
	require.Error(t, err)
	assert.True(t, errors.Is(err, port.ErrDependencyUnavailable))
}

// TestHTTPClient_ClearDelegatePointer_NetworkError_WrapsErrDependencyUnavailable
// checks that network errors are wrapped with port.ErrDependencyUnavailable.
func TestHTTPClient_ClearDelegatePointer_NetworkError_WrapsErrDependencyUnavailable(t *testing.T) {
	c, err := NewHTTPClient("http://127.0.0.1:1", nil, 50*time.Millisecond)
	require.NoError(t, err)
	err = c.ClearDelegatePointer(t.Context(), uuid.New(), uuid.New())
	require.Error(t, err)
	assert.True(t, errors.Is(err, port.ErrDependencyUnavailable))
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
	err = c.SetAvailability(t.Context(), port.SetAvailabilityRequest{TenantID: uuid.New(), UserID: uuid.New()})
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
	err = c.SetAvailability(t.Context(), port.SetAvailabilityRequest{TenantID: uuid.New(), UserID: uuid.New()})
	require.Error(t, err)
	assert.False(t, errors.Is(err, port.ErrDependencyUnavailable), "4xx business rejection must not be wrapped")
	assert.Contains(t, err.Error(), "delegate_unavailable")
}

func TestHTTPClient_SetAvailability_NetworkError_WrapsErrDependencyUnavailable(t *testing.T) {
	c, err := NewHTTPClient("http://127.0.0.1:1", nil, 50*time.Millisecond)
	require.NoError(t, err)
	err = c.SetAvailability(t.Context(), port.SetAvailabilityRequest{TenantID: uuid.New(), UserID: uuid.New()})
	require.Error(t, err)
	assert.True(t, errors.Is(err, port.ErrDependencyUnavailable))
}

// TestHTTPClient_SetAvailability_4xx_NonJSONBody covers the fallback status-error
// path when the response body is not valid JSON (no `error` field to decode).
func TestHTTPClient_SetAvailability_4xx_NonJSONBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("plain text error"))
	}))
	defer server.Close()

	c, err := NewHTTPClient(server.URL, nil, 0)
	require.NoError(t, err)
	err = c.SetAvailability(t.Context(), port.SetAvailabilityRequest{TenantID: uuid.New(), UserID: uuid.New()})
	require.Error(t, err)
	assert.False(t, errors.Is(err, port.ErrDependencyUnavailable))
	assert.Contains(t, err.Error(), "400")
}

// TestUPContract_HybridErrorEnvelope_ExtraFieldsIgnored verifies that
// iam-user-profile's hybrid error envelope (which includes extra fields
// code/message/details beyond gincommon.ErrorResponse) is parsed correctly.
// encoding/json must silently ignore the extra fields — if DisallowUnknownFields
// were ever added, this test would catch the breakage.
func TestUPContract_HybridErrorEnvelope_ExtraFieldsIgnored(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		// Full hybrid envelope as produced by iam-user-profile's newErrorResponse.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":      "delegate_unavailable",
			"status":     422,
			"trace_id":   "abc123",
			"request_id": "req456",
			"code":       "delegate_unavailable",
			"message":    "delegate is currently out of office",
			"details":    []any{},
		})
	}))
	defer server.Close()

	c, err := NewHTTPClient(server.URL, nil, 0)
	require.NoError(t, err)
	err = c.SetAvailability(t.Context(), port.SetAvailabilityRequest{
		TenantID: uuid.New(), UserID: uuid.New(),
	})
	require.Error(t, err)
	assert.False(t, errors.Is(err, port.ErrDependencyUnavailable), "4xx must not be wrapped as dependency unavailable")
	assert.Contains(t, err.Error(), "delegate_unavailable", "error code from UP's hybrid envelope must be surfaced")
}

// TestUPContract_NoExpectedVersionSent verifies that DEL-6 calls never include
// an expected_version field. Delegation has no prior GET so it always uses
// last-writer-wins semantics — sending a stale version would cause spurious
// 409 conflicts on every availability write.
func TestUPContract_NoExpectedVersionSent(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c, err := NewHTTPClient(server.URL, nil, 0)
	require.NoError(t, err)

	status := "ooo"
	_ = c.SetAvailability(t.Context(), port.SetAvailabilityRequest{
		TenantID: uuid.New(), UserID: uuid.New(), Status: &status,
	})
	_, hasVersion := gotBody["expected_version"]
	assert.False(t, hasVersion, "DEL-6 must never send expected_version — delegation uses last-writer-wins")
}

// TestHTTPClient_SetAvailability_InvalidURL covers lines 116–118:
// http.NewRequestWithContext fails when the URL is invalid (null byte).
func TestHTTPClient_SetAvailability_InvalidURL_Errors(t *testing.T) {
	c := &HTTPClient{baseURL: "http://host\x00", httpClient: &http.Client{}}
	err := c.SetAvailability(t.Context(), port.SetAvailabilityRequest{
		TenantID: uuid.New(), UserID: uuid.New(),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "build request")
}

// TestBuildBody_WithOOOUntil covers the req.OOOUntil != nil branch.
func TestBuildBody_WithOOOUntil(t *testing.T) {
	var oooTime = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	req := port.SetAvailabilityRequest{
		TenantID: uuid.New(),
		UserID:   uuid.New(),
		OOOUntil: &oooTime,
	}
	body := buildBody(req)
	require.NotNil(t, body["ooo_until"])
	assert.Equal(t, oooTime.UTC().Format(time.RFC3339), body["ooo_until"])
}
