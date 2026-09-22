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
	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/httpx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/metrics"
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
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	// Always wrap with httpx so W3C traceparent + a client span are
	// emitted even when the caller supplied a raw *http.Client
	// (iam-realm-provisioner: every outbound hop uses otelhttp).
	httpClient = httpx.Instrument(httpClient, timeout)
	return &HTTPClient{baseURL: strings.TrimRight(baseURL, "/"), httpClient: httpClient}, nil
}

// buildBody constructs the wire payload for req. Fields left zero-value on
// req are omitted; delegate_id gets two distinct wire states:
//   - req.DelegateID != nil  -> "delegate_id": "<uuid>"
//   - otherwise              -> "delegate_id" omitted entirely
//
// A plain struct with `omitempty` can't express "explicit null vs omitted"
// on the same field, so this builds a map instead.
//
// SetAvailability is the create path only — status is always set when called
// from delegation. Pointer-clear calls use ClearDelegatePointer instead.
func buildBody(req port.SetAvailabilityRequest) map[string]any {
	body := map[string]any{}
	if req.Status != nil {
		body["status"] = *req.Status
	}
	if req.OOOFrom != nil {
		body["ooo_from"] = req.OOOFrom.UTC().Format(time.RFC3339)
	}
	if req.OOOUntil != nil {
		body["ooo_until"] = req.OOOUntil.UTC().Format(time.RFC3339)
	}
	if req.DelegateID != nil {
		body["delegate_id"] = req.DelegateID.String()
	}
	if req.Note != "" {
		body["note"] = req.Note
	}
	return body
}

// GetAvailability calls GET /api/v1/users/:id/availability on iam-user-profile
// and returns the delegate's current status and OOO end time.
// 404 → user not provisioned in UP, treated as available (no OOO).
// 5xx and network failures are wrapped with port.ErrDependencyUnavailable.
func (c *HTTPClient) GetAvailability(ctx context.Context, tenantID, userID uuid.UUID) (*port.AvailabilitySnapshot, error) {
	started := time.Now()
	snap, err := c.getAvailability(ctx, tenantID, userID)
	elapsed := time.Since(started).Seconds()
	metrics.ObserveDependencyLatency("user_profile", "get_availability", elapsed)
	if err != nil {
		metrics.IncDependencyError("user_profile", "get_availability", metrics.DependencyOutcome(err))
	}
	return snap, err
}

func (c *HTTPClient) getAvailability(ctx context.Context, tenantID, userID uuid.UUID) (*port.AvailabilitySnapshot, error) {
	url := fmt.Sprintf("%s/api/v1/users/%s/availability", c.baseURL, userID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("userprofile: build request: %w", err)
	}
	setInternalHeaders(httpReq, tenantID)
	propagate(ctx, httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("userprofile: request failed: %w: %w", err, port.ErrDependencyUnavailable)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close, nothing actionable on failure

	if resp.StatusCode == http.StatusNotFound {
		return &port.AvailabilitySnapshot{Status: "available"}, nil
	}
	if resp.StatusCode >= http.StatusInternalServerError {
		return nil, fmt.Errorf("userprofile: unexpected status %d: %w", resp.StatusCode, port.ErrDependencyUnavailable)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("userprofile: unexpected status %d", resp.StatusCode)
	}

	var body struct {
		Status   string  `json:"status"`
		OOOUntil *string `json:"ooo_until"`
	}
	if err := json.NewDecoder(httpx.LimitBody(resp.Body)).Decode(&body); err != nil {
		return nil, fmt.Errorf("userprofile: decode response: %w", err)
	}
	snap := &port.AvailabilitySnapshot{Status: body.Status}
	if body.OOOUntil != nil {
		t, parseErr := time.Parse(time.RFC3339, *body.OOOUntil)
		if parseErr != nil {
			return nil, fmt.Errorf("userprofile: parse ooo_until: %w", parseErr)
		}
		snap.OOOUntil = &t
	}
	return snap, nil
}

// ClearDelegatePointer calls iam-user-profile's dedicated pointer-clear
// endpoint (DELETE /api/v1/internal/users/:id/availability/delegate).
// Unlike SetAvailability, this never touches the user's status — UP preserves
// whatever status the user had before the delegation started (Gap 3 Option B).
// 5xx and network failures are wrapped with port.ErrDependencyUnavailable.
func (c *HTTPClient) ClearDelegatePointer(ctx context.Context, tenantID, userID uuid.UUID) error {
	started := time.Now()
	err := c.clearDelegatePointer(ctx, tenantID, userID)
	elapsed := time.Since(started).Seconds()
	metrics.ObserveDependencyLatency("user_profile", "clear_delegate_pointer", elapsed)
	if err != nil {
		metrics.IncDependencyError("user_profile", "clear_delegate_pointer", metrics.DependencyOutcome(err))
	}
	return err
}

func (c *HTTPClient) clearDelegatePointer(ctx context.Context, tenantID, userID uuid.UUID) error {
	url := fmt.Sprintf("%s/api/v1/internal/users/%s/availability/delegate", c.baseURL, userID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("userprofile: build request: %w", err)
	}
	setInternalHeaders(httpReq, tenantID)
	propagate(ctx, httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("userprofile: request failed: %w: %w", err, port.ErrDependencyUnavailable)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return nil
	}
	if resp.StatusCode >= http.StatusInternalServerError {
		return fmt.Errorf("userprofile: unexpected status %d: %w", resp.StatusCode, port.ErrDependencyUnavailable)
	}
	var errResp gincommon.ErrorResponse
	if decErr := json.NewDecoder(httpx.LimitBody(resp.Body)).Decode(&errResp); decErr == nil && errResp.Error != "" {
		return fmt.Errorf("userprofile: %s", errResp.Error)
	}
	return fmt.Errorf("userprofile: unexpected status %d", resp.StatusCode)
}

// SetAvailability calls iam-user-profile's internal availability endpoint.
// 5xx and network/timeout failures are wrapped with
// port.ErrDependencyUnavailable so the service layer's errors.Is check
// resolves to 503 user_profile_unavailable; 4xx business rejections (e.g.
// 422 delegate_unavailable) are returned as a plain error whose message
// contains the callee's error code, so the service layer's
// strings.Contains(err.Error(), "delegate_unavailable") check still works.
func (c *HTTPClient) SetAvailability(ctx context.Context, req port.SetAvailabilityRequest) error {
	started := time.Now()
	err := c.setAvailability(ctx, req)
	elapsed := time.Since(started).Seconds()
	metrics.ObserveDependencyLatency("user_profile", "set_availability", elapsed)
	if err != nil {
		metrics.IncDependencyError("user_profile", "set_availability", metrics.DependencyOutcome(err))
	}
	return err
}

func (c *HTTPClient) setAvailability(ctx context.Context, req port.SetAvailabilityRequest) error {
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

	// 4xx business rejection — decode iam-user-profile's hybrid error envelope.
	// UP returns extra fields beyond gincommon.ErrorResponse (code, message,
	// details for 422) that this struct does not map; encoding/json ignores
	// them since DisallowUnknownFields is not set. Reading only the error
	// field is sufficient — it carries the same value as code.
	var errResp gincommon.ErrorResponse
	if decErr := json.NewDecoder(httpx.LimitBody(resp.Body)).Decode(&errResp); decErr == nil && errResp.Error != "" {
		return fmt.Errorf("userprofile: %s", errResp.Error)
	}
	return fmt.Errorf("userprofile: unexpected status %d", resp.StatusCode)
}
