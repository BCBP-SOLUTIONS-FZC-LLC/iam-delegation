// Package eventbus provides the publish-side pieces of this service's event
// pipeline that sit outside the transactional outbox itself (owned by
// internal/adapter/outbound/postgres): JSON Schema validation of outbound
// payloads before they are enqueued, and the AWS Glue Schema Registry wire
// codec applied only at SNS-publish time. See codec.go's package-level
// comment for the enqueue-time vs. publish-time split (LLD §10.3.1).
package eventbus

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
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
// Cross-package contract: the postgres-adapter's txBoundPublisher
// (internal/adapter/outbound/postgres, implementing port.EventPublisher) is
// the caller. Construct one *SchemaValidator with NewSchemaValidator() at
// startup (it compiles all three schemas once and is safe for concurrent
// use — jsonschema.Schema.Validate takes no lock and holds no mutable
// state), hold it as a singleton, and inside EnqueueCtx / RunInTx call:
//
//	raw, _ := json.Marshal(evt.Data)
//	if err := validator.Validate(ctx, evt.Type, raw); err != nil {
//	    return err // abort the tx — never write an invalid payload to the outbox
//	}
//
// before building the events.Envelope and calling outbox.Enqueue. See also
// ValidatePayload in publisher_helpers.go for a package-level function form
// of the same call, for callers that prefer not to hold the receiver type
// directly in their own signatures.
type SchemaValidator struct {
	schemas map[string]*jsonschema.Schema
}

// schemaEntry pairs an event-type name with its raw JSON schema bytes.
type schemaEntry struct {
	name string
	src  []byte
}

// defaultSchemaEntries are the three published-event schemas embedded at
// build time (internal/eventschema), keyed by the exact domain event-type
// constants (internal/core/domain/event.go) — the same PascalCase strings
// used as the Glue schema names (LLD §10.3.1) and the envelope `type` /
// SNS `EventType` attribute value.
var defaultSchemaEntries = []schemaEntry{
	{domain.EventDelegationStarted, eventschema.DelegationStarted},
	{domain.EventDelegationEnded, eventschema.DelegationEnded},
	{domain.EventDelegationReviewRequested, eventschema.DelegationReviewRequested},
}

// NewSchemaValidator compiles the three published-event schemas from the
// embedded JSON files and returns a validator ready to check payloads
// before they are enqueued. Consumed event types (MembershipRevoked,
// TenantOffboarded) are intentionally NOT registered here — this service
// does not validate inbound cascade payloads against these schemas; that is
// the SQS consumer's own concern.
func NewSchemaValidator() (*SchemaValidator, error) {
	return newSchemaValidatorFromEntries(defaultSchemaEntries)
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
