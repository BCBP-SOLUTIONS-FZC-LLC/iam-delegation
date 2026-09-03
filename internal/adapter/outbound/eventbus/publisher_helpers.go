package eventbus

import (
	"context"
	"encoding/json"
)

// ValidatePayload is a thin, package-level wrapper around
// (*SchemaValidator).Validate. Production enqueue goes through
// ValidatingCodec (missing schema = pass-through, matching
// iam-realm-provisioner); this helper is the fail-closed form used by
// SchemaValidator unit tests.
//
// Equivalent to validator.Validate(ctx, eventType, json.RawMessage(raw)).
func ValidatePayload(ctx context.Context, v *SchemaValidator, eventType string, payload []byte) error {
	return v.Validate(ctx, eventType, json.RawMessage(payload))
}
