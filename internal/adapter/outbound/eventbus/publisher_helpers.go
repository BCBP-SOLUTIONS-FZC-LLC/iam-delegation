package eventbus

import (
	"context"
	"encoding/json"
)

// ValidatePayload is a thin, package-level wrapper around
// (*SchemaValidator).Validate for callers that would rather not carry a
// *SchemaValidator field through their own constructor signature — notably
// the postgres-adapter's txBoundPublisher
// (internal/adapter/outbound/postgres/db.go), which implements
// port.EventPublisher and must validate every event's payload before
// handing it to outbox.Enqueue inside the caller's active pgx.Tx (DLG-EVT-1).
//
// Usage from that package:
//
//	validator, err := eventbus.NewSchemaValidator() // once, at startup
//	...
//	raw, err := json.Marshal(evt.Data)
//	if err != nil { return err }
//	if err := eventbus.ValidatePayload(ctx, validator, evt.Type, raw); err != nil {
//	    return err // abort the tx — do not enqueue an invalid payload
//	}
//
// This is exactly equivalent to calling validator.Validate(ctx, evt.Type,
// json.RawMessage(raw)) directly — both forms are part of this package's
// supported public API; use whichever reads better at the call site.
func ValidatePayload(ctx context.Context, v *SchemaValidator, eventType string, payload []byte) error {
	return v.Validate(ctx, eventType, json.RawMessage(payload))
}
