// Package tender is the HTTP adapter implementing port.TenderScopeClient
// against Tender's internal GET /api/v1/internal/tenants/{tenantID}/tenders/{scopeID}/exists
// endpoint (LLD §7.6.7, DLG-D13). Mirrors
// internal/adapter/outbound/orgmembership's HTTPChecker in shape and
// fail-closed posture.
//
// Not yet wired into cmd/server: Tender has not shipped this provider
// endpoint (cross-team task DLG-D13, tracked as IB-4, blocked on EXT-1). No
// construction call for this package exists in cmd/server/main.go or
// cmd/server/config.go, and delegation_service.go's validateCreateInput
// makes no call through port.TenderScopeClient — a tender-scoped delegation
// is accepted on the presence-only chk_scope_id check alone until Tender's
// endpoint ships and this client is wired in.
package tender

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

// defaultTimeout is the LLD §7.6.7 configured client timeout (≤50ms p99
// budget; 3s matches the other outbound clients' generous ceiling, not the
// expected steady-state latency), used when the caller supplies neither an
// *http.Client nor a positive timeout.
const defaultTimeout = 3 * time.Second

// HTTPClient implements port.TenderScopeClient against Tender's internal
// tender-existence endpoint.
type HTTPClient struct {
	baseURL    string
	httpClient *http.Client
}

var _ port.TenderScopeClient = (*HTTPClient)(nil)

// NewHTTPClient builds an HTTPClient. baseURL is required — an empty
// baseURL is a construction-time error, not a runtime one, matching
// orgmembership.NewHTTPChecker. If httpClient is nil, a client with timeout
// (or defaultTimeout, if timeout <= 0) is used.
func NewHTTPClient(baseURL string, httpClient *http.Client, timeout time.Duration) (*HTTPClient, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return nil, fmt.Errorf("tender: baseURL is required")
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	httpClient = httpx.Instrument(httpClient, timeout)
	return &HTTPClient{baseURL: strings.TrimRight(baseURL, "/"), httpClient: httpClient}, nil
}

type existsResponse struct {
	Exists bool `json:"exists"`
	Live   bool `json:"live"`
}

// CheckLive calls Tender's tender-existence endpoint. Fails CLOSED per the
// port contract: any network error, timeout, or 5xx status is returned as a
// non-nil error wrapped with port.ErrDependencyUnavailable, never treated as
// "not live". A 404 (LLD §7.6.7: "{exists: false} | HTTP 404") is a business
// answer, not a dependency failure — it is normalized to exists=false,
// live=false, err=nil.
//
// No metrics.Live instrumentation here (unlike orgmembership.HTTPChecker's
// Exists): this client has no wired call site yet (see package doc), and
// DLG-D19 closed the gap of registered-but-never-observed instruments —
// adding one here would reopen it. Instrument this alongside the real
// wiring, not before.
func (c *HTTPClient) CheckLive(ctx context.Context, tenantID, scopeID uuid.UUID) (exists, live bool, err error) {
	return c.checkLive(ctx, tenantID, scopeID)
}

func (c *HTTPClient) checkLive(ctx context.Context, tenantID, scopeID uuid.UUID) (exists, live bool, err error) {
	url := fmt.Sprintf("%s/api/v1/internal/tenants/%s/tenders/%s/exists", c.baseURL, tenantID, scopeID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return false, false, fmt.Errorf("tender: build request: %w", err)
	}
	setInternalHeaders(req, tenantID)
	propagate(ctx, req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, false, fmt.Errorf("tender: request failed: %w: %w", err, port.ErrDependencyUnavailable)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close, nothing actionable on failure

	if resp.StatusCode == http.StatusNotFound {
		return false, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode >= http.StatusInternalServerError {
			return false, false, fmt.Errorf("tender: unexpected status %d: %w", resp.StatusCode, port.ErrDependencyUnavailable)
		}
		return false, false, fmt.Errorf("tender: unexpected status %d", resp.StatusCode)
	}

	var out existsResponse
	if err := json.NewDecoder(httpx.LimitBody(resp.Body)).Decode(&out); err != nil {
		return false, false, fmt.Errorf("tender: decode response: %w", err)
	}
	return out.Exists, out.Live, nil
}
