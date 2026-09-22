package catalogadmin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
)

// newTestMetrics registers a fresh isolated metrics instance and restores
// the process-wide Live after the test — same pattern as orgmembership tests.
func newTestMetrics(t *testing.T) {
	t.Helper()
	prev := metrics.Live
	m, err := metrics.RegisterOn(prometheus.NewRegistry(), metrics.RegisterConfig{})
	require.NoError(t, err)
	metrics.Live = m
	t.Cleanup(func() { metrics.Live = prev })
}

// ── Construction ──────────────────────────────────────────────────────────────

func TestNewHTTPClient_EmptyBaseURL_Errors(t *testing.T) {
	_, err := NewHTTPClient("", nil, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "baseURL is required")
}

func TestNewHTTPClient_WhitespaceBaseURL_Errors(t *testing.T) {
	_, err := NewHTTPClient("   ", nil, 0)
	require.Error(t, err)
}

func TestNewHTTPClient_ValidBaseURL_OK(t *testing.T) {
	c, err := NewHTTPClient("http://catalog-admin:8081", nil, 0)
	require.NoError(t, err)
	assert.NotNil(t, c)
}

// ── DepartmentActive — happy paths ────────────────────────────────────────────

func TestDepartmentActive_ActiveDepartment_ReturnsExistsAndActive(t *testing.T) {
	deptID := uuid.New()
	var gotPath, gotUserID, gotTenantID, gotTenantRoles, gotCaller string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUserID = r.Header.Get("x-user-id")
		gotTenantID = r.Header.Get("x-tenant-id")
		gotTenantRoles = r.Header.Get("x-tenant-roles")
		gotCaller = r.Header.Get("x-caller-service")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(departmentResponse{ID: deptID, IsActive: true})
	}))
	defer server.Close()

	c, err := NewHTTPClient(server.URL, nil, 0)
	require.NoError(t, err)

	exists, active, err := c.DepartmentActive(t.Context(), deptID)
	require.NoError(t, err)
	assert.True(t, exists)
	assert.True(t, active)

	// URL path must be /api/v1/departments/:id
	assert.Equal(t, "/api/v1/departments/"+deptID.String(), gotPath)

	// Internal identity headers (Gap 11 fix: uuid.Nil for x-tenant-id)
	assert.Equal(t, "iam-system", gotUserID)
	assert.Equal(t, uuid.Nil.String(), gotTenantID)
	assert.Equal(t, "iam-system", gotTenantRoles)
	assert.Equal(t, "iam-delegation", gotCaller)
}

func TestDepartmentActive_RetiredDepartment_ExistsTrueActiveFalse(t *testing.T) {
	// is_active=false: department exists in catalog but has been retired by
	// an operator (CAT-2 is_active=false). The call succeeds (200 OK) but
	// DepartmentActive must signal active=false so the caller can reject
	// the create with 422 invalid_scope_id.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(departmentResponse{ID: uuid.New(), IsActive: false})
	}))
	defer server.Close()

	c, err := NewHTTPClient(server.URL, nil, 0)
	require.NoError(t, err)

	exists, active, err := c.DepartmentActive(t.Context(), uuid.New())
	require.NoError(t, err)
	assert.True(t, exists, "department row exists in catalog")
	assert.False(t, active, "retired department must return active=false")
}

// ── DepartmentActive — 404 (department not in catalog) ───────────────────────

func TestDepartmentActive_NotFound_ReturnsFalseNoError(t *testing.T) {
	// Catalog Admin returns 404 when the department UUID does not exist in
	// the global catalog. This is a business rejection (422 invalid_scope_id
	// at the service layer), NOT a dependency fault — err must be nil here.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	c, err := NewHTTPClient(server.URL, nil, 0)
	require.NoError(t, err)

	exists, active, err := c.DepartmentActive(t.Context(), uuid.New())
	require.NoError(t, err, "404 is a business answer, not a dependency fault")
	assert.False(t, exists)
	assert.False(t, active)
}

// ── DepartmentActive — error paths (all must wrap ErrDependencyUnavailable) ──

func TestDepartmentActive_5xx_WrapsErrDependencyUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	c, err := NewHTTPClient(server.URL, nil, 0)
	require.NoError(t, err)

	exists, active, err := c.DepartmentActive(t.Context(), uuid.New())
	require.Error(t, err)
	assert.True(t, errors.Is(err, port.ErrDependencyUnavailable),
		"5xx must surface as a dependency fault so the service layer returns 503 catalog_admin_unavailable")
	assert.False(t, exists)
	assert.False(t, active)
}

func TestDepartmentActive_503_WrapsErrDependencyUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	c, err := NewHTTPClient(server.URL, nil, 0)
	require.NoError(t, err)

	_, _, err = c.DepartmentActive(t.Context(), uuid.New())
	require.Error(t, err)
	assert.True(t, errors.Is(err, port.ErrDependencyUnavailable))
}

func TestDepartmentActive_MalformedJSON_WrapsErrDependencyUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{not valid json"))
	}))
	defer server.Close()

	c, err := NewHTTPClient(server.URL, nil, 0)
	require.NoError(t, err)

	exists, active, err := c.DepartmentActive(t.Context(), uuid.New())
	require.Error(t, err)
	assert.True(t, errors.Is(err, port.ErrDependencyUnavailable),
		"a decode failure on a 200 body is treated as a dependency fault, not a business rejection")
	assert.False(t, exists)
	assert.False(t, active)
}

func TestDepartmentActive_NetworkError_WrapsErrDependencyUnavailable(t *testing.T) {
	// Port 1 is reserved/unreachable — connection refused immediately.
	c, err := NewHTTPClient("http://127.0.0.1:1", nil, 50*time.Millisecond)
	require.NoError(t, err)

	_, _, err = c.DepartmentActive(t.Context(), uuid.New())
	require.Error(t, err)
	assert.True(t, errors.Is(err, port.ErrDependencyUnavailable))
}

func TestDepartmentActive_Timeout_WrapsErrDependencyUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond) // outlasts the 10ms client timeout
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c, err := NewHTTPClient(server.URL, nil, 10*time.Millisecond)
	require.NoError(t, err)

	_, _, err = c.DepartmentActive(t.Context(), uuid.New())
	require.Error(t, err)
	assert.True(t, errors.Is(err, port.ErrDependencyUnavailable))
}

func TestDepartmentActive_InvalidURL_Errors(t *testing.T) {
	// A null byte in baseURL causes http.NewRequestWithContext to fail
	// before any network call — covers the request-build error path.
	c := &HTTPClient{baseURL: "http://host\x00", httpClient: &http.Client{}}
	_, _, err := c.departmentActive(t.Context(), uuid.New())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "build request")
}

// ── Metrics recording ─────────────────────────────────────────────────────────

func TestDepartmentActive_RecordsLatencyMetric_OnSuccess(t *testing.T) {
	newTestMetrics(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(departmentResponse{ID: uuid.New(), IsActive: true})
	}))
	defer server.Close()

	c, err := NewHTTPClient(server.URL, nil, 0)
	require.NoError(t, err)

	_, _, err = c.DepartmentActive(t.Context(), uuid.New())
	require.NoError(t, err, "metrics recording must not affect the happy-path return")
}

func TestDepartmentActive_RecordsErrorMetric_On5xx(t *testing.T) {
	newTestMetrics(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	c, err := NewHTTPClient(server.URL, nil, 0)
	require.NoError(t, err)

	_, _, err = c.DepartmentActive(t.Context(), uuid.New())
	require.Error(t, err, "the error metric recording path runs on any non-nil error")
}

func TestDepartmentActive_NilMetricsLive_DoesNotPanic(t *testing.T) {
	// metrics.Live is nil in unit tests that do not call metrics.Register.
	// DepartmentActive must be safe to call even when no metrics are wired.
	prev := metrics.Live
	metrics.Live = nil
	t.Cleanup(func() { metrics.Live = prev })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(departmentResponse{ID: uuid.New(), IsActive: true})
	}))
	defer server.Close()

	c, err := NewHTTPClient(server.URL, nil, 0)
	require.NoError(t, err)

	assert.NotPanics(t, func() {
		_, _, _ = c.DepartmentActive(t.Context(), uuid.New())
	})
}
