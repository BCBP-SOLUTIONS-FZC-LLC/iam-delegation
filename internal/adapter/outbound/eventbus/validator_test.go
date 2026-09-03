package eventbus

import (
	"context"
	"testing"

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
			// Bug 2/DLG-D26 regression: EndReasonDelegateDisabled was added to
			// the Go enum but the JSON Schema's ended_reason enum was
			// initially left stale, so this exact payload shape would fail
			// real schema validation at publish time despite every unit test
			// passing (they all use a fake EventPublisher that skips
			// validation entirely).
			name:      "DelegationEnded_DelegateDisabled",
			eventType: domain.EventDelegationEnded,
			payload: `{
				"delegation_id": "018f4c3e-a1b2-7000-9d3e-4f8c1a2b3d4e",
				"tenant_id":     "018f4c3e-a1b2-7000-9d3e-4f8c1a2b3d4f",
				"delegator_id":  "018f4c3e-a1b2-7000-9d3e-4f8c1a2b3d50",
				"delegate_id":   "018f4c3e-a1b2-7000-9d3e-4f8c1a2b3d51",
				"ended_reason":  "delegate_disabled",
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

func TestNewSchemaValidatorFromEntries_MalformedJSON_Errors(t *testing.T) {
	_, err := newSchemaValidatorFromEntries([]schemaEntry{{name: "Bad", src: []byte("{not json")}})
	if err == nil {
		t.Fatal("expected an error for malformed schema JSON")
	}
}

func TestNewSchemaValidatorFromEntries_InvalidSchema_Errors(t *testing.T) {
	// "type" must be a string or array per JSON Schema — this compiles as
	// valid JSON but fails schema compilation.
	_, err := newSchemaValidatorFromEntries([]schemaEntry{{name: "Bad", src: []byte(`{"type": 123}`)}})
	if err == nil {
		t.Fatal("expected a compile error for an invalid schema")
	}
}

func TestSchemaValidator_Validate_MalformedPayload_Errors(t *testing.T) {
	v := mustValidator(t)
	err := v.Validate(context.Background(), domain.EventDelegationStarted, []byte("{not json"))
	if err == nil {
		t.Fatal("expected an error for a malformed payload")
	}
}
