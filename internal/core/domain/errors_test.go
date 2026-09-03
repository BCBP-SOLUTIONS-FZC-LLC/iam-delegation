package domain

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func TestError_Error(t *testing.T) {
	err := &Error{Code: "invalid_delegate", Message: "delegate is not an active member"}
	assert.Equal(t, "invalid_delegate: delegate is not an active member", err.Error())
}

func TestError_Unwrap(t *testing.T) {
	err := &Error{Code: "invalid_delegate", Message: "x", Cause: ErrInvalidDelegate}
	assert.Equal(t, ErrInvalidDelegate, err.Unwrap())
	assert.True(t, errors.Is(err, ErrInvalidDelegate))
}

func TestNewError(t *testing.T) {
	err := NewError(ErrSelfDelegation, "delegator and delegate are the same user")
	assert.Equal(t, "self_delegation", err.Code)
	assert.Equal(t, "delegator and delegate are the same user", err.Message)
	assert.Equal(t, ErrSelfDelegation, err.Cause)
	assert.Equal(t, "self_delegation: delegator and delegate are the same user", err.Error())
}

func TestDefaultDelegationTenantSettings(t *testing.T) {
	tenantID := uuid.New()
	got := DefaultDelegationTenantSettings(tenantID)
	assert.Equal(t, tenantID, got.TenantID)
	assert.Equal(t, DefaultMaxDurationDays, got.MaxDurationDays)
	assert.Equal(t, DefaultReviewWindowDays, got.ReviewWindowDays)
}
