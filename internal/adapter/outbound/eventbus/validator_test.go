package eventbus

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
)

func mustValidator(t *testing.T) *SchemaValidator {
	t.Helper()
	v, err := NewSchemaValidator()
	if err != nil {
		t.Fatalf("NewSchemaValidator() error = %v", err)
	}
	return v
}

func TestSchemaValidator_ValidPayloads(t *testing.T) {
	v := mustValidator(t)
	ctx := context.Background()

	tests := []struct {
		name      string
		eventType string
		payload   string
	}{
		{
			name:      "DelegationStarted",
			eventType: domain.EventDelegationStarted,
			payload: `{
				"delegation_id": "018f4c3e-a1b2-7000-9d3e-4f8c1a2b3d4e",
				"tenant_id":     "018f4c3e-a1b2-7000-9d3e-4f8c1a2b3d4f",
				"delegator_id":  "018f4c3e-a1b2-7000-9d3e-4f8c1a2b3d50",
				"delegate_id":   "018f4c3e-a1b2-7000-9d3e-4f8c1a2b3d51",
				"scope":         "all",
				"starts_at":     "2026-08-21T00:00:00Z",
				"actor_id":      "018f4c3e-a1b2-7000-9d3e-4f8c1a2b3d52"
			}`,
		},
		{
			name:      "DelegationEnded",
			eventType: domain.EventDelegationEnded,
			payload: `{
				"delegation_id": "018f4c3e-a1b2-7000-9d3e-4f8c1a2b3d4e",
				"tenant_id":     "018f4c3e-a1b2-7000-9d3e-4f8c1a2b3d4f",
				"delegator_id":  "018f4c3e-a1b2-7000-9d3e-4f8c1a2b3d50",
				"delegate_id":   "018f4c3e-a1b2-7000-9d3e-4f8c1a2b3d51",
				"ended_reason":  "review_expired",
				"actor_id":      "018f4c3e-a1b2-7000-9d3e-4f8c1a2b3d52"
			}`,
		},
		{
			name:      "DelegationReviewRequested",
			eventType: domain.EventDelegationReviewRequested,
			payload: `{
				"delegation_id":  "018f4c3e-a1b2-7000-9d3e-4f8c1a2b3d4e",
				"tenant_id":      "018f4c3e-a1b2-7000-9d3e-4f8c1a2b3d4f",
				"delegator_id":   "018f4c3e-a1b2-7000-9d3e-4f8c1a2b3d50",
				"delegate_id":    "018f4c3e-a1b2-7000-9d3e-4f8c1a2b3d51",
				"days_remaining": 3,
				"actor_id":       "018f4c3e-a1b2-7000-9d3e-4f8c1a2b3d52"
			}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := v.Validate(ctx, tt.eventType, []byte(tt.payload)); err != nil {
				t.Fatalf("Validate(%s) unexpected error: %v", tt.eventType, err)
			}
		})
	}
}

func TestSchemaValidator_MissingRequiredField(t *testing.T) {
	v := mustValidator(t)
	ctx := context.Background()

	// DelegationEnded requires delegate_id; omit it.
	payload := []byte(`{
		"delegation_id": "018f4c3e-a1b2-7000-9d3e-4f8c1a2b3d4e",
		"delegator_id":  "018f4c3e-a1b2-7000-9d3e-4f8c1a2b3d50",
		"ended_reason":  "cancelled"
	}`)

	if err := v.Validate(ctx, domain.EventDelegationEnded, payload); err == nil {
		t.Fatal("Validate() expected error for missing required field delegate_id, got nil")
	}
}

func TestSchemaValidator_InvalidEndedReasonEnum(t *testing.T) {
	v := mustValidator(t)
	ctx := context.Background()

	payload := []byte(`{
		"delegation_id": "018f4c3e-a1b2-7000-9d3e-4f8c1a2b3d4e",
		"delegator_id":  "018f4c3e-a1b2-7000-9d3e-4f8c1a2b3d50",
		"delegate_id":   "018f4c3e-a1b2-7000-9d3e-4f8c1a2b3d51",
		"ended_reason":  "not_a_real_reason"
	}`)

	if err := v.Validate(ctx, domain.EventDelegationEnded, payload); err == nil {
		t.Fatal("Validate() expected error for invalid ended_reason enum value, got nil")
	}
}

func TestSchemaValidator_UnregisteredEventType(t *testing.T) {
	v := mustValidator(t)
	ctx := context.Background()

	if err := v.Validate(ctx, "SomethingElse", []byte(`{}`)); err == nil {
		t.Fatal("Validate() expected error for unregistered event type, got nil")
	}
}

// TestNewSchemaValidatorFromEntries_BadJSON covers the json.Unmarshal error path.
func TestNewSchemaValidatorFromEntries_BadJSON(t *testing.T) {
	_, err := newSchemaValidatorFromEntries([]schemaEntry{
		{name: "Bad", src: []byte("not json at all {")},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "load event schema")
}

// TestNewSchemaValidatorFromEntries_InvalidSchema covers the Compile error path
// by passing a valid JSON that is an invalid JSON Schema (type must be a string,
// not an integer).
func TestNewSchemaValidatorFromEntries_InvalidSchema(t *testing.T) {
	_, err := newSchemaValidatorFromEntries([]schemaEntry{
		{name: "Bad", src: []byte(`{"type": 42}`)}, // type must be string/array per JSON Schema
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "compile event schema")
}

// TestSchemaValidator_Validate_MalformedPayload covers the json.Unmarshal
// error path inside Validate when the payload is not valid JSON.
func TestSchemaValidator_Validate_MalformedPayload(t *testing.T) {
	v := mustValidator(t)
	err := v.Validate(context.Background(), domain.EventDelegationStarted, []byte("{not json"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unmarshal")
}

func TestValidatePayload_MatchesMethodForm(t *testing.T) {
	v := mustValidator(t)
	ctx := context.Background()

	payload := []byte(`{
		"tenant_id": "018f4c3e-a1b2-7000-9d3e-4f8c1a2b3d4f",
		"actor_id":  "018f4c3e-a1b2-7000-9d3e-4f8c1a2b3d52"
	}`)

	// TenantMembershipsPurged is a consumed type, deliberately not
	// registered in this validator — confirms ValidatePayload surfaces the
	// same "unregistered" error as calling v.Validate directly, rather than
	// silently succeeding.
	err := ValidatePayload(ctx, v, domain.EventTenantMembershipsPurged, payload)
	if err == nil {
		t.Fatal("ValidatePayload() expected error for unregistered (consumed) event type, got nil")
	}
}
