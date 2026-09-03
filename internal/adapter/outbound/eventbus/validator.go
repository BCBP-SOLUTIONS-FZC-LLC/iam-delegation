// Package eventbus implements the outbound port.EventPublisher (writes to
// the transactional outbox, DLG-EVT-1) and the wire-format codecs used at
// SNS-publish time (Glue Schema Registry) and at SQS-consume time (decoding
// a Glue-encoded payload from another producer).
package eventbus

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/eventschema"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// SchemaValidator validates each event payload against its JSON Schema
// before the plain-JSON payload is written to the outbox — preventing
// malformed events from corrupting the iam.delegation.events SNS stream and
// all downstream consumers. It performs schema validation only; it does not
// wrap or delegate to a wire-format codec. Wire-format encoding (Glue Schema
// Registry) happens later, at SNS-publish time, via a separate events.Codec
// (GlueCodec in this package) configured through events.WithCodec (see
// cmd/server/main.go).
//
// Production enqueue uses ValidatingCodec (missing schema = pass-through,
// matching iam-realm-provisioner). SchemaValidator remains the fail-closed
// unit-test helper: Validate errors if eventType is unregistered. See
// ValidatePayload in publisher_helpers.go for a package-level function form.
type SchemaValidator struct {
	schemas map[string]*jsonschema.Schema
}

// schemaEntry pairs an event-type name with its raw JSON schema bytes.
type schemaEntry struct {
	name string
	src  []byte
}

// NewSchemaValidator compiles the published-event schemas from
// eventschema.ByEventType. Consumed event types are intentionally NOT
// registered — this service does not validate inbound cascade payloads
// against these schemas.
func NewSchemaValidator() (*SchemaValidator, error) {
	entries := make([]schemaEntry, 0, len(eventschema.ByEventType))
	for name, src := range eventschema.ByEventType {
		entries = append(entries, schemaEntry{name: name, src: src})
	}
	return newSchemaValidatorFromEntries(entries)
}

// newSchemaValidatorFromEntries is the testable core of NewSchemaValidator.
// It accepts an explicit list of schema entries so tests can inject bad
// JSON, duplicate names, or invalid schemas to exercise error paths.
func newSchemaValidatorFromEntries(entries []schemaEntry) (*SchemaValidator, error) {
	c := jsonschema.NewCompiler()
	compiled := make(map[string]*jsonschema.Schema, len(entries))
	for _, e := range entries {
		// v6 AddResource expects an already-decoded JSON value (any), not an io.Reader.
		var v any
		if err := json.Unmarshal(e.src, &v); err != nil {
			return nil, fmt.Errorf("load event schema %q: %w", e.name, err)
		}
		url := "iam-delegation-event-schema:" + e.name
		if err := c.AddResource(url, v); err != nil {
			return nil, fmt.Errorf("load event schema %q: %w", e.name, err)
		}
		sch, err := c.Compile(url)
		if err != nil {
			return nil, fmt.Errorf("compile event schema %q: %w", e.name, err)
		}
		compiled[e.name] = sch
	}
	return &SchemaValidator{schemas: compiled}, nil
}

// Validate checks payload against the schema registered for eventType.
// Returns an error if validation fails or if eventType is not registered —
// the latter prevents silently publishing unvalidated events when a new
// event type is added to domain/event.go but not added to the entries slice
// in NewSchemaValidator.
func (v *SchemaValidator) Validate(_ context.Context, eventType string, payload json.RawMessage) error {
	sch, ok := v.schemas[eventType]
	if !ok {
		return fmt.Errorf("no schema registered for event type %q — add it to NewSchemaValidator entries", eventType)
	}
	var instance any
	if err := json.Unmarshal(payload, &instance); err != nil {
		return fmt.Errorf("schema validation: unmarshal %q payload: %w", eventType, err)
	}
	if err := sch.Validate(instance); err != nil {
		return fmt.Errorf("event payload violates schema %q: %w", eventType, err)
	}
	return nil
}
