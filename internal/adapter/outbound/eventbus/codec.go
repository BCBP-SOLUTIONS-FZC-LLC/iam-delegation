package eventbus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/eventschema"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	gluetypes "github.com/aws/aws-sdk-go-v2/service/glue/types"
	"github.com/google/uuid"
)

// GlueCodec implements platform-events' events.Codec for SNS-publish time.
// Enqueue-time validation lives on ValidatingCodec (validating_codec.go),
// wrapping events.NoopCodec. The library ships no Glue implementation —
// this is the service-side Codec injected via events.WithCodec. The
// transactional outbox always stores plain, validated JSON; wire-format
// encoding (the Glue header) is applied transiently by the SNS publisher
// immediately before publish (LLD §10.3.1):
//
//	// Enqueue time (eventbus.Publisher) — plain JSON, ValidatingCodec, inside
//	// the state-change tx:
//	pub := eventbus.New(domain.Source, validatingCodec)
//	txRunner := postgres.NewTxRunner(pool, pub)
//
//	// Publish time (cmd/server/main.go) — Glue encoding happens here, not above:
//	publisher := events.NewSNSPublisher(..., events.WithCodec(glueCodec))
//
// Registry name: "iam-delegation-events" (a new, dedicated Glue registry —
// LLD §10.3.1; it does not share Core's registry).

const (
	// glueHeaderVersion is the magic byte for the AWS Glue Schema Registry wire format.
	glueHeaderVersion byte = 0x03
	// glueNoCompression signals an uncompressed payload.
	glueNoCompression byte = 0x00
	// glueHeaderSize = 1 (version) + 1 (compression) + 16 (schema version UUID) = 18 bytes.
	glueHeaderSize = 18
)

// GlueRegistryName is the dedicated Glue Schema Registry this service
// registers its four published event schemas under (LLD §10.3.1). Unlike
// the O&M shape this was extracted from, this registry is new and does not
// share Core's — Core drops these subjects entirely.
const GlueRegistryName = "iam-delegation-events"

// GlueCodec prepends the AWS Glue Schema Registry wire-format header to each payload:
//
//	[0x03][0x00][16-byte schema version UUID (big-endian)]
//
// Schema version IDs are resolved from Glue once at startup BY DEFINITION
// (one per published event type — DelegationStarted, DelegationEnded,
// DelegationReviewRequested, DelegationEscalationRequested):
// GetSchemaByDefinition with this binary's own embedded schema
// (eventschema.ByEventType), so every event is stamped with the version this
// binary actually produces — never merely the registry's latest, which runs
// ahead of the running code when a schema is registered before deploy (or
// after a rollback) and behind it when a deploy races schema-registry.yml.
// The IDs are cached for the life of the process; there is no background
// refresh. The event type
// string (domain.EventDelegationStarted etc.) is used verbatim as the Glue
// schema name — unlike the O&M/User-Profile precedent, no dot-notation
// translation is needed here because this service's event type constants
// are already the PascalCase Glue schema names (LLD §10.3.1 table).
type GlueCodec struct {
	client       *glue.Client
	registryName string
	mu           sync.RWMutex
	definitions  map[string][]byte // event type → embedded JSON Schema
	versionCache map[string]string // event type → schema version UUID string
}

var _ events.Codec = (*GlueCodec)(nil)

// NewGlueCodec creates a GlueCodec and resolves, for each name in
// schemaNames (pass the four domain event-type constants:
// domain.EventDelegationStarted, domain.EventDelegationEnded,
// domain.EventDelegationReviewRequested,
// domain.EventDelegationEscalationRequested), the version whose definition
// matches this binary's embedded schema. Fails fast at startup if any
// lookup fails — including a definition not registered yet (a deploy that
// outran schema-registry.yml, which self-heals on the first restart after
// registration lands). Starting anyway would stamp events with a version
// that doesn't describe them.
func NewGlueCodec(ctx context.Context, client *glue.Client, registryName string, schemaNames []string) (*GlueCodec, error) {
	c := &GlueCodec{
		client:       client,
		registryName: registryName,
		definitions:  eventschema.ByEventType,
		versionCache: make(map[string]string, len(schemaNames)),
	}
	for _, name := range schemaNames {
		id, err := c.fetchVersionID(ctx, name)
		if err != nil {
			return nil, fmt.Errorf(
				"resolve glue schema %q in registry %q by definition: %w — "+
					"this binary's schema version isn't registered yet: wait for "+
					"schema-registry.yml to register it, or run `make schema-verify`",
				name, registryName, err,
			)
		}
		c.versionCache[name] = id
	}
	return c, nil
}

// Encode looks up the cached schema version ID for eventType, prepends the
// 18-byte Glue wire-format header to payload, and returns the schema
// version UUID so the caller (the SNS publisher) can stamp it into the
// envelope's dataschema field.
func (g *GlueCodec) Encode(ctx context.Context, eventType string, payload json.RawMessage) (encoded []byte, schemaVersionID string, err error) {
	versionID, err := g.versionID(ctx, eventType)
	if err != nil {
		return nil, "", fmt.Errorf("get glue schema version %q: %w", eventType, err)
	}
	out, err := prependGlueHeader(versionID, payload)
	if err != nil {
		return nil, "", err
	}
	return out, versionID, nil
}

// Decode strips the 18-byte Glue wire-format header from encoded and
// returns the remaining plain-JSON payload. The schemaID parameter is
// accepted for events.Codec interface compatibility but is not needed to
// strip the header — the schema version UUID is self-contained in the
// encoded bytes at offset 2:18 — so it is not cross-checked against
// anything here.
func (g *GlueCodec) Decode(_ context.Context, _ string, encoded []byte) (json.RawMessage, error) {
	return stripGlueHeader(encoded)
}

// stripGlueHeader removes the 18-byte Glue wire-format header, shared by
// GlueCodec.Decode and GlueDecodeCodec.Decode below — stripping needs no
// registry/schema-name context, only the fixed header layout.
func stripGlueHeader(encoded []byte) (json.RawMessage, error) {
	if len(encoded) < glueHeaderSize {
		return nil, fmt.Errorf("glue codec: encoded payload is %d bytes — shorter than the %d-byte Glue header", len(encoded), glueHeaderSize)
	}
	if encoded[0] != glueHeaderVersion {
		return nil, fmt.Errorf("glue codec: unexpected header version byte 0x%02x — want 0x%02x", encoded[0], glueHeaderVersion)
	}
	return json.RawMessage(encoded[glueHeaderSize:]), nil
}

// GlueDecodeCodec decodes Glue-wire-format payloads WITHOUT a Glue client,
// registry name, or schema names — stripping the header is fully
// self-describing (see stripGlueHeader), so no registry lookup is needed to
// decode, only to encode.
//
// This exists for the inbound delegation-cascade-q consumer: Core
// (iam-org-membership) Glue-encodes MembershipRevoked/TenantMembershipsPurged
// whenever ITS OWN GLUE_REGISTRY_MEMBERSHIP_NAME is set — which it is by
// default in iam-org-membership's committed Helm values
// (deploy/helm/values.yaml: GLUE_REGISTRY_MEMBERSHIP_NAME:
// "iam-membership-events") — independently of whether THIS service's own
// GLUE_REGISTRY_NAME (outbound publish side) is set. platform-events' SQS
// consumer decodes an inbound envelope only when that envelope's SchemaID is
// non-empty (internal/adapter/outbound/sqs/consumer.go); with no
// events.WithConsumerCodec configured, a Glue-encoded message would fail
// decode with "message has schema_id ... but no Codec is configured" on
// every single delivery, retry to exhaustion, and land in
// delegation-cascade-q-dlq — silently breaking the removal/tenant-purge
// cascade (DEL-6/DEL-7). Wiring this codec unconditionally (cmd/server/
// main.go) is safe in every environment: when the producer uses NoopCodec
// (SchemaID empty, e.g. local/dev), Decode is never invoked at all.
type GlueDecodeCodec struct{}

var _ events.Codec = GlueDecodeCodec{}

// Encode always errors — this codec is decode-only. The inbound SQS
// consumer never calls Encode; a caller reaching this path has mistakenly
// wired GlueDecodeCodec as a publish-time codec instead of GlueCodec.
func (GlueDecodeCodec) Encode(_ context.Context, eventType string, _ json.RawMessage) (encoded []byte, schemaVersionID string, err error) {
	return nil, "", fmt.Errorf("glue decode codec: Encode is not supported (decode-only, event type %q) — use GlueCodec to publish", eventType)
}

// Decode strips the 18-byte Glue wire-format header; see stripGlueHeader.
func (GlueDecodeCodec) Decode(_ context.Context, _ string, encoded []byte) (json.RawMessage, error) {
	return stripGlueHeader(encoded)
}

func (g *GlueCodec) versionID(ctx context.Context, eventType string) (string, error) {
	g.mu.RLock()
	id, ok := g.versionCache[eventType]
	g.mu.RUnlock()
	if ok {
		return id, nil
	}
	id, err := g.fetchVersionID(ctx, eventType)
	if err != nil {
		return "", err
	}
	g.mu.Lock()
	g.versionCache[eventType] = id
	g.mu.Unlock()
	return id, nil
}

func (g *GlueCodec) fetchVersionID(ctx context.Context, eventType string) (string, error) {
	// eventType (e.g. "DelegationStarted") IS the Glue schema name — no
	// translation table needed (see GlueCodec's doc comment).
	raw, ok := g.definitions[eventType]
	if !ok {
		return "", fmt.Errorf("no embedded schema for %q", eventType)
	}
	definition, err := registeredDefinition(raw)
	if err != nil {
		return "", fmt.Errorf("schema %q: %w", eventType, err)
	}
	out, err := g.client.GetSchemaByDefinition(ctx, &glue.GetSchemaByDefinitionInput{
		SchemaId: &gluetypes.SchemaId{
			SchemaName:   &eventType,
			RegistryName: &g.registryName,
		},
		SchemaDefinition: &definition,
	})
	if err != nil {
		return "", err
	}
	if out.SchemaVersionId == nil {
		return "", fmt.Errorf("nil SchemaVersionId for schema %q in registry %q", eventType, g.registryName)
	}
	if out.Status != gluetypes.SchemaVersionStatusAvailable {
		return "", fmt.Errorf("schema %q version %s in registry %q is %s, not AVAILABLE", eventType, *out.SchemaVersionId, g.registryName, out.Status)
	}
	return *out.SchemaVersionId, nil
}

// registeredDefinition returns raw in exactly the form schema-gov register
// uploads it: Python's json.dumps(schema, separators=(",", ":")) —
// compact, key order preserved, non-ASCII escaped as \uXXXX (ensure_ascii
// defaults to True — this service's schemas DO carry non-ASCII, e.g. "§"
// and "—" in descriptions). Sending the byte-identical string makes
// GetSchemaByDefinition match whether or not Glue normalises JSON
// definitions. TestRegisteredDefinition_MatchesSchemaGov pins this against
// real Python for every produced schema.
func registeredDefinition(raw []byte) (string, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return "", fmt.Errorf("compact schema: %w", err)
	}
	return asciiEscape(compact.Bytes()), nil
}

// asciiEscape rewrites every non-ASCII rune as a JSON \uXXXX escape
// (UTF-16 surrogate pairs above U+FFFF), matching Python's ensure_ascii.
// Non-ASCII can only appear inside JSON strings, so this is always safe.
func asciiEscape(b []byte) string {
	var out strings.Builder
	out.Grow(len(b))
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		b = b[size:]
		switch {
		case r < utf8.RuneSelf:
			out.WriteRune(r)
		case r > 0xFFFF:
			r -= 0x10000
			fmt.Fprintf(&out, "\\u%04x\\u%04x", 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		default:
			fmt.Fprintf(&out, "\\u%04x", r)
		}
	}
	return out.String()
}

func prependGlueHeader(schemaVersionID string, payload []byte) ([]byte, error) {
	id, err := uuid.Parse(schemaVersionID)
	if err != nil {
		return nil, fmt.Errorf("parse schema version UUID %q: %w", schemaVersionID, err)
	}
	out := make([]byte, glueHeaderSize+len(payload))
	out[0] = glueHeaderVersion
	out[1] = glueNoCompression
	copy(out[2:18], id[:])
	copy(out[glueHeaderSize:], payload)
	return out, nil
}

// SNS identity codec is events.NoopCodec, chosen in cmd/server when
// GLUE_REGISTRY_NAME is unset. The same type wraps ValidatingCodec at
// enqueue time — there is no second, Encode-only NoopCodec.
