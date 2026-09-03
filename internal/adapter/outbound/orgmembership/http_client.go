// Package orgmembership is the HTTP adapter implementing
// port.MembershipCheckClient against Core's internal GET
// /api/v1/internal/tenants/{tenantID}/members/{userID}/exists endpoint — replacing
// the composite membership FKs this table lost when delegation moved out of
// Core's database (LLD §7.6.2, DLG-D3). Mirrors iam-tender-acl's
// internal/adapter/outbound/membershipcheck/http_client.go in spirit, but
// uses gincommon.PropagateHeaders for trace propagation (see propagate.go)
// instead of a hand-rolled traceparent helper.
package orgmembership

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/httpx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
)

// defaultTimeout is the LLD §15 configured client timeout
// (orgMembership.membershipCheckTimeoutMs: 3000), used when the caller
// supplies neither an *http.Client nor a positive timeout.
const defaultTimeout = 3 * time.Second

// HTTPChecker implements port.MembershipCheckClient against Core's internal
// membership-existence endpoint.
type HTTPChecker struct {
	baseURL    string
	httpClient *http.Client
}

var _ port.MembershipCheckClient = (*HTTPChecker)(nil)

// NewHTTPChecker builds an HTTPChecker. baseURL is required — LLD §15 "base
// URLs required, fail-fast on empty for the data-bearing clients" — an
// empty baseURL is a construction-time error, not a runtime one. If
// httpClient is nil, a client with timeout (or defaultTimeout, if timeout
// <= 0) is used.
func NewHTTPChecker(baseURL string, httpClient *http.Client, timeout time.Duration) (*HTTPChecker, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return nil, fmt.Errorf("orgmembership: baseURL is required")
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	// Always wrap with httpx so W3C traceparent + a client span are
	// emitted even when the caller supplied a raw *http.Client
	// (iam-realm-provisioner: every outbound hop uses otelhttp).
	httpClient = httpx.Instrument(httpClient, timeout)
	return &HTTPChecker{baseURL: strings.TrimRight(baseURL, "/"), httpClient: httpClient}, nil
}

type existsResponse struct {
	Active             bool       `json:"active"`
	TenantMembershipID *uuid.UUID `json:"tenant_membership_id,omitempty"`
}

// Exists calls Core's membership-existence endpoint. Fails CLOSED per the
// port contract: any network error, timeout, or non-2xx status is returned
// as a non-nil error, never treated as "not active". 5xx and network/
// timeout failures are wrapped with port.ErrDependencyUnavailable so the
// service layer's errors.Is checks resolve to 503 org_membership_unavailable;
// this endpoint carries no 4xx business-rejection shape to preserve
// unwrapped (the service layer's checkBothMemberships treats any non-nil
// err from Exists identically, regardless of type).
func (c *HTTPChecker) Exists(ctx context.Context, tenantID, userID uuid.UUID) (bool, uuid.UUID, error) {
	started := time.Now()
	active, id, err := c.exists(ctx, tenantID, userID)
	if m := metrics.Live; m != nil {
		m.ObserveMembershipCheckDuration(time.Since(started).Seconds())
		if err != nil {
			m.RecordMembershipCheckFailure()
		}
	}
	return active, id, err
}

func (c *HTTPChecker) exists(ctx context.Context, tenantID, userID uuid.UUID) (bool, uuid.UUID, error) {
	url := fmt.Sprintf("%s/api/v1/internal/tenants/%s/members/%s/exists", c.baseURL, tenantID, userID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return false, uuid.UUID{}, fmt.Errorf("orgmembership: build request: %w", err)
	}
	setInternalHeaders(req, tenantID)
	propagate(ctx, req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, uuid.UUID{}, fmt.Errorf("orgmembership: request failed: %w: %w", err, port.ErrDependencyUnavailable)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close, nothing actionable on failure

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode >= http.StatusInternalServerError {
			return false, uuid.UUID{}, fmt.Errorf("orgmembership: unexpected status %d: %w", resp.StatusCode, port.ErrDependencyUnavailable)
		}
		return false, uuid.UUID{}, fmt.Errorf("orgmembership: unexpected status %d", resp.StatusCode)
	}

	var out existsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, uuid.UUID{}, fmt.Errorf("orgmembership: decode response: %w", err)
	}
	if !out.Active {
		return false, uuid.UUID{}, nil
	}

	var membershipID uuid.UUID
	if out.TenantMembershipID != nil {
		membershipID = *out.TenantMembershipID
	}
	return true, membershipID, nil
}
