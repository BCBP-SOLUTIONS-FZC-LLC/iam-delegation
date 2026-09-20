package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/eventschema"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ValidatingCodec wraps inner (always events.NoopCodec at enqueue time) and
// validates each event's payload against the embedded JSON Schema for its
// event type before allowing the outbox insert to proceed. Compiles every
// schema in internal/eventschema once at construction time.
//
// platform-events ships no schema validator — this is the service-side
// events.Codec that Validate-then-delegates, matching iam-user-profile.
// Missing schema = pass-through (consumed-only / future additive types).
type ValidatingCodec struct {
	inner   events.Codec
	schemas map[string]*jsonschema.Schema
	mu      sync.RWMutex
}

var _ events.Codec = (*ValidatingCodec)(nil)

// NewValidatingCodec compiles the embedded schemas and returns a codec that
// validates against them, falling through to inner. Compilation failure is
// a programming error surfaced immediately (CrashLoopBackoff rather than
// silently publishing malformed events). Nil inner becomes events.NoopCodec.
func NewValidatingCodec(inner events.Codec) (*ValidatingCodec, error) {
	if inner == nil {
		inner = events.NoopCodec{}
	}
	c := &ValidatingCodec{inner: inner, schemas: make(map[string]*jsonschema.Schema, len(eventschema.ByEventType))}
	compiler := jsonschema.NewCompiler()
	for name, raw := range eventschema.ByEventType {
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("eventbus: parse embedded schema %s: %w", name, err)
		}
		if err := compiler.AddResource(name, doc); err != nil {
			return nil, fmt.Errorf("eventbus: add schema resource %s: %w", name, err)
		}
		sch, err := compiler.Compile(name)
		if err != nil {
			return nil, fmt.Errorf("eventbus: compile schema %s: %w", name, err)
		}
		c.schemas[name] = sch
	}
	return c, nil
}

// Encode validates payload against eventType's schema (a no-op pass-through
// when eventType has no registered schema — e.g. a consumed-only type or a
// future additive event not yet backed by a JSON Schema) and then delegates
// to inner.
func (c *ValidatingCodec) Encode(ctx context.Context, eventType string, payload json.RawMessage) (encoded []byte, schemaVersionID string, err error) {
	c.mu.RLock()
	sch, ok := c.schemas[eventType]
	c.mu.RUnlock()
	if ok {
		var doc any
		if err := json.Unmarshal(payload, &doc); err != nil {
			return nil, "", fmt.Errorf("validate %s: payload is not JSON: %w", eventType, err)
		}
		if err := sch.Validate(doc); err != nil {
			return nil, "", fmt.Errorf("validate %s: %w", eventType, err)
		}
	}
	return c.inner.Encode(ctx, eventType, payload)
}

// Decode delegates to inner — enqueue never decodes; this satisfies events.Codec.
func (c *ValidatingCodec) Decode(ctx context.Context, schemaID string, encoded []byte) (json.RawMessage, error) {
	return c.inner.Decode(ctx, schemaID, encoded)
}
