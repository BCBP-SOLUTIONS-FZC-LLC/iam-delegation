package orgmembership

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

func TestNewHTTPChecker_EmptyBaseURL_Errors(t *testing.T) {
	_, err := NewHTTPChecker("", nil, 0)
	require.Error(t, err)

	_, err = NewHTTPChecker("   ", nil, 0)
	require.Error(t, err)
}

func TestHTTPChecker_Exists_Active(t *testing.T) {
	tenantID, userID, membershipID := uuid.New(), uuid.New(), uuid.New()
	var gotPath, gotUserID, gotTenantID, gotTenantRoles string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUserID = r.Header.Get("x-user-id")
		gotTenantID = r.Header.Get("x-tenant-id")
		gotTenantRoles = r.Header.Get("x-tenant-roles")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"active": true, "tenant_membership_id": membershipID.String()})
	}))
	defer server.Close()

	checker, err := NewHTTPChecker(server.URL, nil, 0)
	require.NoError(t, err)

	active, gotMembershipID, err := checker.Exists(t.Context(), tenantID, userID)
	require.NoError(t, err)
	assert.True(t, active)
	assert.Equal(t, membershipID, gotMembershipID)
	assert.Equal(t, "/api/v1/internal/tenants/"+tenantID.String()+"/members/"+userID.String()+"/exists", gotPath)
	assert.Equal(t, "iam-system", gotUserID)
	assert.Equal(t, tenantID.String(), gotTenantID)
	assert.Equal(t, "iam-system", gotTenantRoles)
}

func TestHTTPChecker_Exists_NotActive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"active": false})
	}))
	defer server.Close()

	checker, err := NewHTTPChecker(server.URL, nil, 0)
	require.NoError(t, err)
	active, _, err := checker.Exists(t.Context(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.False(t, active)
}

// TestHTTPChecker_Exists_5xx_WrapsErrDependencyUnavailable covers this
// project's convention (unlike iam-tender-acl's precedent): a 5xx response
// must be wrapped with port.ErrDependencyUnavailable so the service layer's
// errors.Is resolves to 503 org_membership_unavailable.
func TestHTTPChecker_Exists_5xx_WrapsErrDependencyUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	checker, err := NewHTTPChecker(server.URL, nil, 0)
	require.NoError(t, err)
	active, _, err := checker.Exists(t.Context(), uuid.New(), uuid.New())
	require.Error(t, err)
	assert.False(t, active)
	assert.True(t, errors.Is(err, port.ErrDependencyUnavailable))
}

func TestHTTPChecker_Exists_Timeout_WrapsErrDependencyUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"active": true})
	}))
	defer server.Close()

	checker, err := NewHTTPChecker(server.URL, nil, 10*time.Millisecond)
	require.NoError(t, err)
	_, _, err = checker.Exists(t.Context(), uuid.New(), uuid.New())
	require.Error(t, err)
	assert.True(t, errors.Is(err, port.ErrDependencyUnavailable))
}

func TestHTTPChecker_Exists_NetworkError_WrapsErrDependencyUnavailable(t *testing.T) {
	checker, err := NewHTTPChecker("http://127.0.0.1:1", nil, 50*time.Millisecond)
	require.NoError(t, err)
	_, _, err = checker.Exists(t.Context(), uuid.New(), uuid.New())
	require.Error(t, err)
	assert.True(t, errors.Is(err, port.ErrDependencyUnavailable))
}

// TestHTTPChecker_Exists_4xx_NonDependencyError verifies a 4xx (non-5xx) response
// is returned as a plain error without the ErrDependencyUnavailable sentinel.
func TestHTTPChecker_Exists_4xx_NonDependencyError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	checker, err := NewHTTPChecker(server.URL, nil, 0)
	require.NoError(t, err)
	active, _, err := checker.Exists(t.Context(), uuid.New(), uuid.New())
	require.Error(t, err)
	assert.False(t, active)
	assert.False(t, errors.Is(err, port.ErrDependencyUnavailable), "4xx must not be wrapped as dependency unavailable")
	assert.Contains(t, err.Error(), "404")
}

// TestHTTPChecker_Exists_200_InvalidJSON covers the decode error path.
func TestHTTPChecker_Exists_200_InvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not-json"))
	}))
	defer server.Close()

	checker, err := NewHTTPChecker(server.URL, nil, 0)
	require.NoError(t, err)
	_, _, err = checker.Exists(t.Context(), uuid.New(), uuid.New())
	require.Error(t, err)
}

// TestHTTPChecker_Exists_ActiveNoMembershipID covers the path where the response
// says active=true but omits tenant_membership_id — membershipID should be uuid.Nil.
func TestHTTPChecker_Exists_ActiveNoMembershipID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Omit tenant_membership_id
		_ = json.NewEncoder(w).Encode(map[string]any{"active": true})
	}))
	defer server.Close()

	checker, err := NewHTTPChecker(server.URL, nil, 0)
	require.NoError(t, err)
	active, membershipID, err := checker.Exists(t.Context(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.True(t, active)
	assert.Equal(t, uuid.Nil, membershipID, "missing tenant_membership_id must yield uuid.Nil")
}
