// Package userprofile is the HTTP adapter implementing
// port.UserProfileClient against iam-user-profile's internal
// PUT /api/v1/internal/users/{userID}/availability endpoint — the availability-
// first coordination point for DEL-6 (LLD §11.1/§11.2/§11.3/§11.4).
package userprofile

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
)

// defaultTimeout is the LLD §15 configured client timeout
// (userProfile.timeoutMs: 3000), used when the caller supplies neither an
// *http.Client nor a positive timeout.
const defaultTimeout = 3 * time.Second

// HTTPClient implements port.UserProfileClient against iam-user-profile.
type HTTPClient struct {
	baseURL    string
	httpClient *http.Client
}

var _ port.UserProfileClient = (*HTTPClient)(nil)

// NewHTTPClient builds an HTTPClient. baseURL is required — LLD §15 "base
// URLs required, fail-fast on empty for the data-bearing clients" — an
// empty baseURL is a construction-time error, not a runtime one. If
// httpClient is nil, a client with timeout (or defaultTimeout, if timeout
// <= 0) is used.
func NewHTTPClient(baseURL string, httpClient *http.Client, timeout time.Duration) (*HTTPClient, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return nil, fmt.Errorf("userprofile: baseURL is required")
	}
	if httpClient == nil {
		if timeout <= 0 {
			timeout = defaultTimeout
		}
		httpClient = &http.Client{Timeout: timeout}
	}
	return &HTTPClient{baseURL: strings.TrimRight(baseURL, "/"), httpClient: httpClient}, nil
}

// buildBody constructs the wire payload for req. Fields left zero-value on
// req are omitted; delegate_id gets three distinct wire states per the port
// doc comment:
//   - req.ClearDelegate == true      -> "delegate_id": null (explicit clear)
//   - req.DelegateID != nil          -> "delegate_id": "<uuid>"
//   - otherwise                      -> "delegate_id" omitted entirely
//
// A plain struct with `omitempty` can't express "explicit null vs omitted"
// on the same field, so this builds a map instead.
//
// status is REQUIRED on iam-user-profile's current PutAvailabilityRequest
// DTO (binding:"required") — a status-less {delegate_id: null} body, which
// this client's End path used to send per this port's original "NEVER
// {status:\"available\"}, UP owns that transition" design, now gets a
// clean 400 from iam-user-profile rather than a clean pointer-clear. Until
// iam-user-profile makes status optional again (or exposes a dedicated
// pointer-clear-only endpoint), default to "available" on the End path so
// the call succeeds — this does shift the return-to-available decision
// into iam-delegation, which is a real product/ownership question, not a
// purely mechanical one; flagged for the iam-user-profile team to confirm.
func buildBody(req port.SetAvailabilityRequest) map[string]any {
	body := map[string]any{}
	switch {
	case req.Status != nil:
		body["status"] = *req.Status
	case req.ClearDelegate:
		body["status"] = "available"
	}
	if req.OOOFrom != nil {
		body["ooo_from"] = req.OOOFrom.UTC().Format(time.RFC3339)
	}
	if req.OOOUntil != nil {
		body["ooo_until"] = req.OOOUntil.UTC().Format(time.RFC3339)
	}
	switch {
	case req.ClearDelegate:
		body["delegate_id"] = nil
	case req.DelegateID != nil:
		body["delegate_id"] = req.DelegateID.String()
	}
	if req.Note != "" {
		body["note"] = req.Note
	}
	return body
}

// SetAvailability calls iam-user-profile's internal availability endpoint.
// 5xx and network/timeout failures are wrapped with
// port.ErrDependencyUnavailable so the service layer's errors.Is check
// resolves to 503 user_profile_unavailable; 4xx business rejections (e.g.
// 422 delegate_unavailable) are returned as a plain error whose message
// contains the callee's error code, so the service layer's
// strings.Contains(err.Error(), "delegate_unavailable") check still works.
func (c *HTTPClient) SetAvailability(ctx context.Context, req port.SetAvailabilityRequest) error {
	body, err := json.Marshal(buildBody(req))
	if err != nil {
		return fmt.Errorf("userprofile: encode request: %w", err)
	}

	url := fmt.Sprintf("%s/api/v1/internal/users/%s/availability", c.baseURL, req.UserID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("userprofile: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	setInternalHeaders(httpReq, req.TenantID)
	propagate(ctx, httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("userprofile: request failed: %w: %w", err, port.ErrDependencyUnavailable)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close, nothing actionable on failure

	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return nil
	}

	if resp.StatusCode >= http.StatusInternalServerError {
		return fmt.Errorf("userprofile: unexpected status %d: %w", resp.StatusCode, port.ErrDependencyUnavailable)
	}

	// 4xx business rejection — decode gincommon's standard error body
	// ({"error": "<code>", "status", "trace_id", "request_id"}, per
	// iam-user-profile's own newErrorResponse, which mirrors
	// platform-gincommon.ErrorResponse) and surface the code in the
	// returned error's message.
	var errResp gincommon.ErrorResponse
	if decErr := json.NewDecoder(resp.Body).Decode(&errResp); decErr == nil && errResp.Error != "" {
		return fmt.Errorf("userprofile: %s", errResp.Error)
	}
	return fmt.Errorf("userprofile: unexpected status %d", resp.StatusCode)
}
