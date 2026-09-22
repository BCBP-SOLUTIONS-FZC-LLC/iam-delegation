package domain

import "errors"

// Sentinel error codes. String value = wire code returned in the response
// body via gincommon.ErrorResponse. Kept as errors.New so callers can
// wrap/unwrap with errors.Is. Canonical taxonomy per LLD §20 (DLG-Q8):
// scope_id_required / not_review_tracked kept; invalid_scope_id /
// reason_too_long added; delegation_not_open_ended dropped in favor of
// not_review_tracked.
var (
	// Common / cross-cutting
	ErrValidation             = errors.New("validation_error")
	ErrMissingIdentity        = errors.New("missing_identity_headers")
	ErrInsufficientRole       = errors.New("insufficient_role")
	ErrOptimisticLockConflict = errors.New("optimistic_lock_conflict")

	// 400 — malformed input
	ErrInvalidDelegationScope           = errors.New("invalid_delegation_scope")
	ErrInvalidDelegationMaxDurationDays = errors.New("invalid_delegation_max_duration_days")
	ErrInvalidDelegationReviewWindow    = errors.New("invalid_delegation_review_window_days")

	// 404
	ErrDelegationNotFound = errors.New("delegation_not_found")

	// 409
	// (ErrOptimisticLockConflict above)
	ErrIdempotencyKeyInFlight = errors.New("idempotency_key_in_flight")

	// 422 — domain-rule validation
	ErrScopeIDRequired             = errors.New("scope_id_required")
	ErrInvalidScopeID              = errors.New("invalid_scope_id")
	ErrSelfDelegation              = errors.New("self_delegation")
	ErrInvalidDelegate             = errors.New("invalid_delegate")
	ErrDelegateUnavailable         = errors.New("delegate_unavailable")
	ErrReasonTooLong               = errors.New("reason_too_long")
	ErrDelegationWindowInverted    = errors.New("delegation_window_inverted")
	ErrDelegationWindowTooLong     = errors.New("delegation_window_too_long")
	ErrDelegationStartInPast       = errors.New("delegation_start_in_past")
	ErrDelegationStartTooFarFuture = errors.New("delegation_start_too_far_future")
	ErrNotReviewTracked            = errors.New("not_review_tracked")
	ErrExtendDaysOutOfRange        = errors.New("extend_days_out_of_range")

	// 503 — dependency
	ErrOrgMembershipUnavailable = errors.New("org_membership_unavailable")
	ErrUserProfileUnavailable   = errors.New("user_profile_unavailable")
	ErrCatalogAdminUnavailable  = errors.New("catalog_admin_unavailable")
	ErrDBUnavailable            = errors.New("db_unavailable")
)

// Error wraps a sentinel with a human-readable message and optional
// structured details. Handlers translate Error.Code into an HTTP status via
// the mapping in the http adapter's error mapper (LLD §20).
type Error struct {
	Code    string
	Message string
	Cause   error
	// Details is a free-form map serialized into the response body —
	// e.g. record_version on ErrOptimisticLockConflict,
	// delegation_max_duration_days on ErrDelegationWindowTooLong.
	Details map[string]any
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }
func (e *Error) Unwrap() error { return e.Cause }

// NewError creates an Error whose Cause is the given sentinel and whose wire
// Code is the sentinel's string value.
func NewError(sentinel error, message string) *Error {
	return &Error{Code: sentinel.Error(), Message: message, Cause: sentinel}
}

// WithDetails attaches structured detail fields to the error body. Chainable.
func (e *Error) WithDetails(d map[string]any) *Error {
	e.Details = d
	return e
}
