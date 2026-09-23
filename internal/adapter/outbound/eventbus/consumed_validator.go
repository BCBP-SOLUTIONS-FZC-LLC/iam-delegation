package eventbus

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/eventschema"
)

// ErrSchemaViolation marks an inbound payload that fails its consumed
// schema. Redelivery can never fix it; the consumer returns it so SQS's
// redrive policy moves the message to delegation-cascade-q-dlq, where it
// can be inspected and redriven once the producer (or this schema) is
// fixed — never silently dropped.
var ErrSchemaViolation = errors.New("inbound payload violates consumed schema")

// ConsumedValidator validates inbound delegation-cascade-q payloads against
// eventschema.Consumed (DLG-D51) before the consumer dispatches them.
type ConsumedValidator struct {
	inner *SchemaValidator
}

// NewConsumedValidator compiles eventschema.Consumed. A compile failure is
// a programming error surfaced at startup.
func NewConsumedValidator() (*ConsumedValidator, error) {
	entries := make([]schemaEntry, 0, len(eventschema.Consumed))
	for name, src := range eventschema.Consumed {
		entries = append(entries, schemaEntry{name: name, src: src})
	}
	inner, err := newSchemaValidatorFromEntries(entries)
	if err != nil {
		return nil, fmt.Errorf("eventbus: consumed schemas: %w", err)
	}
	return &ConsumedValidator{inner: inner}, nil
}

// Validate checks payload against eventType's consumed schema. An event
// type with no consumed schema passes (the consumer acks unknown types
// itself). A violation wraps ErrSchemaViolation.
func (v *ConsumedValidator) Validate(eventType string, payload json.RawMessage) error {
	sch, ok := v.inner.schemas[eventType]
	if !ok {
		return nil
	}
	var instance any
	if err := json.Unmarshal(payload, &instance); err != nil {
		return fmt.Errorf("%w: %s payload is not JSON: %w", ErrSchemaViolation, eventType, err)
	}
	if err := sch.Validate(instance); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrSchemaViolation, eventType, err)
	}
	return nil
}
