// Package catalogadmin is the HTTP adapter implementing
// port.CatalogAdminClient against Catalog Admin's public
// GET /api/v1/departments/:id endpoint (CAT-7, LLD §7.6.7 / GAP-020).
//
// Used during DLG-2 Create to validate that a scope="department" scope_id
// references a department that exists in the global catalog and is still
// active (is_active=true). Retired departments (is_active=false) must be
// rejected at grant time so no delegation is ever created against a scope
// that Catalog Admin has marked as no longer valid.
//
// Fail-closed posture: any 5xx or transport error becomes
// port.ErrDependencyUnavailable (→ 503 catalog_admin_unavailable); a clean
// 404 or is_active=false is a business rejection (422 invalid_scope_id),
// not a dependency failure.
package catalogadmin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/httpx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
)

// defaultTimeout matches the other outbound clients' ceiling (3 s) per LLD §15.
const defaultTimeout = 3 * time.Second

// HTTPClient implements port.CatalogAdminClient against Catalog Admin's
// department-by-id endpoint.
type HTTPClient struct {
	baseURL    string
	httpClient *http.Client
}

var _ port.CatalogAdminClient = (*HTTPClient)(nil)

// NewHTTPClient builds an HTTPClient. baseURL is required — an empty baseURL
// is a construction-time error, matching orgmembership.NewHTTPChecker. If
// httpClient is nil, a client with timeout (or defaultTimeout, if timeout
// <= 0) is used.
func NewHTTPClient(baseURL string, httpClient *http.Client, timeout time.Duration) (*HTTPClient, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return nil, fmt.Errorf("catalogadmin: baseURL is required")
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	httpClient = httpx.Instrument(httpClient, timeout)
	return &HTTPClient{baseURL: strings.TrimRight(baseURL, "/"), httpClient: httpClient}, nil
}

// departmentResponse is the subset of CAT-7's response body we care about.
type departmentResponse struct {
	ID       uuid.UUID `json:"id"`
	IsActive bool      `json:"is_active"`
}

// DepartmentActive calls Catalog Admin's CAT-7 endpoint
// (GET /api/v1/departments/:id). Returns exists=true, active=true only
// when the HTTP response is 200 and is_active=true. A 404 maps to
// exists=false, active=false, err=nil (business rejection, not a fault).
// Any 5xx or transport error wraps port.ErrDependencyUnavailable.
func (c *HTTPClient) DepartmentActive(ctx context.Context, departmentID uuid.UUID) (exists, active bool, err error) {
	url := fmt.Sprintf("%s/api/v1/departments/%s", c.baseURL, departmentID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return false, false, fmt.Errorf("catalogadmin: build request: %w", err)
	}
	setInternalHeaders(req)
	propagate(ctx, req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, false, fmt.Errorf("catalogadmin: request failed: %w: %w", err, port.ErrDependencyUnavailable)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close

	if resp.StatusCode == http.StatusNotFound {
		return false, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode >= http.StatusInternalServerError {
			return false, false, fmt.Errorf("catalogadmin: unexpected status %d: %w", resp.StatusCode, port.ErrDependencyUnavailable)
		}
		return false, false, fmt.Errorf("catalogadmin: unexpected status %d: %w", resp.StatusCode, port.ErrDependencyUnavailable)
	}

	var out departmentResponse
	if err := json.NewDecoder(httpx.LimitBody(resp.Body)).Decode(&out); err != nil {
		return false, false, fmt.Errorf("catalogadmin: decode response: %w: %w", err, port.ErrDependencyUnavailable)
	}
	return true, out.IsActive, nil
}
