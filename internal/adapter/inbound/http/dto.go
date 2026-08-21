// Package http is the inbound HTTP adapter for the Delegation Service:
// DLG-1…7 (public, gateway-fronted), DLG-I1…I4 (internal, mesh-only), health
// checks, and the DTO/error-code translation between the wire and
// internal/core/service's business types (LLD §8).
package http

import (
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
)

// ── DLG-2 create ────────────────────────────────────────────────────────

// DelegationCreateRequest is the DLG-2 request body (LLD §8.4). The wire
// field is `ooo_note`, NOT `reason` — it rides the User Profile
// SetAvailability call as the display note and is stored verbatim in
// delegations.reason (audit-only, DEL-10); it is mapped onto
// service.CreateInput.Reason by delegation_handler.go's toCreateInput.
type DelegationCreateRequest struct {
	DelegateID uuid.UUID  `json:"delegate_id"`
	Scope      string     `json:"scope"`
	ScopeID    *uuid.UUID `json:"scope_id,omitempty" swaggertype:"string" format:"uuid"`
	StartsAt   *time.Time `json:"starts_at,omitempty"`
	EndsAt     *time.Time `json:"ends_at,omitempty"`
	OOONote    string     `json:"ooo_note,omitempty"`
}

// DelegationResponse is the DLG-2/3/5 response shape (LLD §8.4) — the
// review_* fields are only populated for open-ended (EndsAt == nil)
// delegations and are omitted otherwise.
type DelegationResponse struct {
	DelegationID           uuid.UUID  `json:"delegation_id"`
	DelegatorID            uuid.UUID  `json:"delegator_id"`
	DelegateID             uuid.UUID  `json:"delegate_id"`
	Scope                  string     `json:"scope"`
	ScopeID                *uuid.UUID `json:"scope_id,omitempty" swaggertype:"string" format:"uuid"`
	StartsAt               time.Time  `json:"starts_at"`
	EndsAt                 *time.Time `json:"ends_at,omitempty"`
	Status                 string     `json:"status"`
	RecordVersion          int64      `json:"record_version"`
	ReviewDueAt            *time.Time `json:"review_due_at,omitempty"`
	ReviewLastWarnedBucket *int       `json:"review_last_warned_bucket,omitempty"`
	ReviewWindowDays       *int       `json:"review_window_days,omitempty"`
}

// DelegationToResponse maps a domain.Delegation onto its wire shape.
func DelegationToResponse(d domain.Delegation) DelegationResponse {
	return DelegationResponse{
		DelegationID:           d.ID,
		DelegatorID:            d.DelegatorID,
		DelegateID:             d.DelegateID,
		Scope:                  string(d.Scope),
		ScopeID:                d.ScopeID,
		StartsAt:               d.StartsAt,
		EndsAt:                 d.EndsAt,
		Status:                 string(d.Status),
		RecordVersion:          d.RecordVersion,
		ReviewDueAt:            d.ReviewDueAt,
		ReviewLastWarnedBucket: d.ReviewLastWarnedBucket,
		ReviewWindowDays:       d.ReviewWindowDays,
	}
}

// DelegationListResponse wraps the DLG-1 payload.
type DelegationListResponse struct {
	Items []DelegationResponse `json:"items"`
}

// ── DLG-4 extend ────────────────────────────────────────────────────────

// DelegationExtendRequest is the DLG-4 body. ExtendDays is optional — when
// omitted the service falls back to the per-delegation review-window
// override, then the tenant default (LLD §8.4). Must be in [1,180] if
// provided (ErrExtendDaysOutOfRange).
type DelegationExtendRequest struct {
	ExtendDays    *int  `json:"extend_days,omitempty"`
	RecordVersion int64 `json:"record_version"`
}

// DelegationExtendResponse is DLG-4's exact 200 response shape (LLD §8.4) —
// deliberately narrower than DelegationResponse: only these three fields
// are documented on the wire for this route.
type DelegationExtendResponse struct {
	DelegationID           uuid.UUID  `json:"delegation_id"`
	ReviewDueAt            *time.Time `json:"review_due_at"`
	ReviewLastWarnedBucket *int       `json:"review_last_warned_bucket"`
}

// DelegationToExtendResponse builds DLG-4's response shape from a domain.Delegation.
func DelegationToExtendResponse(d domain.Delegation) DelegationExtendResponse {
	return DelegationExtendResponse{
		DelegationID:           d.ID,
		ReviewDueAt:            d.ReviewDueAt,
		ReviewLastWarnedBucket: d.ReviewLastWarnedBucket,
	}
}

// ── DLG-5 reassign ──────────────────────────────────────────────────────

// DelegationReassignRequest is the DLG-5 body (LLD §8.4, DLG-D11/DLG-Q7) —
// every field optional, each defaulting to the current delegation's value.
// ScopeID and EndsAt need raw-JSON presence detection (see
// delegation_handler.go's Reassign) to distinguish "field omitted" (keep
// current value) from "field present, explicitly null" (clear it /
// open-ended) — a plain nil-pointer check after json.Unmarshal cannot tell
// those apart, which is exactly what service.ReassignInput's
// ScopeIDSet/EndsAtProvided flags exist to capture.
type DelegationReassignRequest struct {
	NewDelegateID *uuid.UUID `json:"new_delegate_id,omitempty" swaggertype:"string" format:"uuid"`
	Scope         *string    `json:"scope,omitempty"`
	ScopeID       *uuid.UUID `json:"scope_id,omitempty" swaggertype:"string" format:"uuid"`
	EndsAt        *time.Time `json:"ends_at,omitempty"`
	Reason        *string    `json:"reason,omitempty"`
	RecordVersion int64      `json:"record_version"`
}

// ── DLG-6/7 settings ────────────────────────────────────────────────────

// SettingsResponse is the DLG-6/7 response shape (LLD §8.4).
type SettingsResponse struct {
	MaxDurationDays  int   `json:"max_duration_days"`
	ReviewWindowDays int   `json:"review_window_days"`
	RecordVersion    int64 `json:"record_version"`
}

// SettingsToResponse builds DLG-6/7's response shape from the domain policy.
func SettingsToResponse(s domain.DelegationTenantSettings) SettingsResponse {
	return SettingsResponse{
		MaxDurationDays:  s.MaxDurationDays,
		ReviewWindowDays: s.ReviewWindowDays,
		RecordVersion:    s.RecordVersion,
	}
}

// SettingsSetRequest is the DLG-7 body — both fields required, each in
// [1,180] (LLD §8.4, enforced by SettingsService.Set).
type SettingsSetRequest struct {
	MaxDurationDays  int `json:"max_duration_days"`
	ReviewWindowDays int `json:"review_window_days"`
}

// ── DLG-I3/I4 internal reads ────────────────────────────────────────────

// DeptDelegateResponse is the DLG-I3 response — Core's §8.8.4 dept-scope
// removal precision (WFI-11). Found is false (with every other field
// omitted) when no active department-scope delegation exists for the given
// user/department; DLG-I3 has no LLD-documented example body, so this
// shape is this adapter's own design, kept minimal to what Core's removal
// gate actually needs (the delegation_id to check against, plus the
// delegate for display).
type DeptDelegateResponse struct {
	Found        bool       `json:"found"`
	DelegationID *uuid.UUID `json:"delegation_id,omitempty" swaggertype:"string" format:"uuid"`
	DelegatorID  *uuid.UUID `json:"delegator_id,omitempty" swaggertype:"string" format:"uuid"`
	DelegateID   *uuid.UUID `json:"delegate_id,omitempty" swaggertype:"string" format:"uuid"`
}

// ActiveDelegationView is one entry in DLG-I4's response (LLD §8.4) — the
// escape hatch replacing I-8's removed active_delegations[] field. Six
// fields, byte-compatible with the old shape.
type ActiveDelegationView struct {
	DelegationID uuid.UUID  `json:"delegation_id"`
	DelegatorID  uuid.UUID  `json:"delegator_id"`
	DelegateID   uuid.UUID  `json:"delegate_id"`
	Scope        string     `json:"scope"`
	ScopeID      *uuid.UUID `json:"scope_id" swaggertype:"string" format:"uuid"`
	EndsAt       *time.Time `json:"ends_at"`
}

// DelegationToActiveView builds DLG-I4's six-field escape-hatch shape from a domain.Delegation.
func DelegationToActiveView(d domain.Delegation) ActiveDelegationView {
	return ActiveDelegationView{
		DelegationID: d.ID,
		DelegatorID:  d.DelegatorID,
		DelegateID:   d.DelegateID,
		Scope:        string(d.Scope),
		ScopeID:      d.ScopeID,
		EndsAt:       d.EndsAt,
	}
}

// ActiveDelegationsResponse wraps the DLG-I4 payload.
type ActiveDelegationsResponse struct {
	ActiveDelegations []ActiveDelegationView `json:"active_delegations"`
}

// ── DLG-I1/I2 cron entry points ─────────────────────────────────────────

// ExpiryRunResponse is the DLG-I1 response (LLD §11.3 sequence diagram).
type ExpiryRunResponse struct {
	Attempted int `json:"attempted"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
}

// ReviewSweepRunResponse is the DLG-I2 response (LLD §11.4 sequence
// diagram) — four independent counters across the dual 7d/3d warn passes
// plus the auto-end pass.
type ReviewSweepRunResponse struct {
	Warned7d int `json:"warned_7d"`
	Warned3d int `json:"warned_3d"`
	Expired  int `json:"expired"`
	Deferred int `json:"deferred"`
}
