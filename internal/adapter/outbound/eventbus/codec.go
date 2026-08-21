package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	gluetypes "github.com/aws/aws-sdk-go-v2/service/glue/types"
	"github.com/google/uuid"
)

// GlueCodec and NoopCodec implement platform-events' events.Codec directly
// (github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events) — no local
// Codec interface is defined here. They are injected via events.WithCodec at
// the SNS-publish step (see cmd/server/main.go), not at outbox-enqueue time:
// the transactional outbox always stores plain, validated JSON regardless of
// which codec is configured (validated by SchemaValidator, see validator.go);
// wire-format encoding (the Glue header) is applied transiently by the SNS
// publisher immediately before publish (LLD §10.3.1):
//
//	// Enqueue time (owned by internal/adapter/outbound/postgres) — plain
//	// JSON, no codec, inside the state-change tx:
//	raw, _ := json.Marshal(payload)
//	validator.Validate(ctx, eventType, raw)
//	env := events.NewEnvelope(eventType, domain.Source, json.RawMessage(raw), opts...)
//	outbox.Enqueue(ctx, tx, env)
//
//	// Publish time (cmd/server/main.go) — Glue encoding happens here, not above:
//	publisher := events.NewSNSPublisher(events.SNSConfig{...}, events.WithCodec(glueCodec))
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
// registers its three published event schemas under (LLD §10.3.1). Unlike
// the O&M shape this was extracted from, this registry is new and does not
// share Core's — Core drops these three subjects entirely.
const GlueRegistryName = "iam-delegation-events"

// Logger is the structured logging interface StartRefresher's background
// goroutine uses for non-fatal refresh failures. Trimmed to the single
// method needed so any Zap/slog-backed logger built in main.go can satisfy
// it directly without an adapter.
type Logger interface {
	Warn(msg string, fields map[string]interface{})
}

// GlueCodec prepends the AWS Glue Schema Registry wire-format header to each payload:
//
//	[0x03][0x00][16-byte schema version UUID (big-endian)]
//
// Schema version IDs are fetched from Glue at startup (one per published
// event type — DelegationStarted, DelegationEnded,
// DelegationReviewRequested) and cached in memory. A cache miss triggers a
// fresh lookup; the result is stored for subsequent calls. The event type
// string (domain.EventDelegationStarted etc.) is used verbatim as the Glue
// schema name — unlike the O&M/User-Profile precedent, no dot-notation
// translation is needed here because this service's event type constants
// are already the PascalCase Glue schema names (LLD §10.3.1 table).
type GlueCodec struct {
	client       *glue.Client
	registryName string
	mu           sync.RWMutex
	versionCache map[string]string // event type → schema version UUID string
	log          Logger            // optional — see WithLogger
}

var _ events.Codec = (*GlueCodec)(nil)

// WithLogger attaches log so StartRefresher's background refresh-failure
// warnings route through the same structured sink as the rest of the
// service. Optional — nil is a valid value (the default), in which case
// those warnings fall back to slog.Default().
func (g *GlueCodec) WithLogger(log Logger) *GlueCodec {
	g.log = log
	return g
}

// NewGlueCodec creates a GlueCodec and pre-fetches the latest schema
// version ID for each name in schemaNames (pass the three domain event-type
// constants: domain.EventDelegationStarted, domain.EventDelegationEnded,
// domain.EventDelegationReviewRequested). Fails fast at startup if any
// lookup fails — the alternative is a silent publish failure on the first
// event of that type.
func NewGlueCodec(ctx context.Context, client *glue.Client, registryName string, schemaNames []string) (*GlueCodec, error) {
	c := &GlueCodec{
		client:       client,
		registryName: registryName,
		versionCache: make(map[string]string, len(schemaNames)),
	}
	for _, name := range schemaNames {
		id, err := c.fetchVersionID(ctx, name)
		if err != nil {
			return nil, fmt.Errorf(
				"prefetch glue schema %q in registry %q: %w — "+
					"confirm the three expected schemas exist (DelegationStarted, "+
					"DelegationEnded, DelegationReviewRequested)",
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
	if len(encoded) < glueHeaderSize {
		return nil, fmt.Errorf("glue codec: encoded payload is %d bytes — shorter than the %d-byte Glue header", len(encoded), glueHeaderSize)
	}
	if encoded[0] != glueHeaderVersion {
		return nil, fmt.Errorf("glue codec: unexpected header version byte 0x%02x — want 0x%02x", encoded[0], glueHeaderVersion)
	}
	return json.RawMessage(encoded[glueHeaderSize:]), nil
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
	out, err := g.client.GetSchemaVersion(ctx, &glue.GetSchemaVersionInput{
		SchemaId: &gluetypes.SchemaId{
			SchemaName:   &eventType,
			RegistryName: &g.registryName,
		},
		SchemaVersionNumber: &gluetypes.SchemaVersionNumber{
			LatestVersion: true,
		},
	})
	if err != nil {
		return "", err
	}
	if out.SchemaVersionId == nil {
		return "", fmt.Errorf("nil SchemaVersionId for schema %q in registry %q", eventType, g.registryName)
	}
	return *out.SchemaVersionId, nil
}

// StartRefresher runs a background goroutine that re-fetches every cached
// schema version ID at the given interval. This ensures that schema updates
// in the Glue registry are picked up without requiring a pod restart. The
// goroutine exits when ctx is cancelled. Fetch errors are non-fatal — the
// stale cached ID remains in use until the next successful refresh.
func (g *GlueCodec) StartRefresher(ctx context.Context, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				g.mu.RLock()
				names := make([]string, 0, len(g.versionCache))
				for name := range g.versionCache {
					names = append(names, name)
				}
				g.mu.RUnlock()
				for _, name := range names {
					if id, err := g.fetchVersionID(ctx, name); err == nil {
						g.mu.Lock()
						g.versionCache[name] = id
						g.mu.Unlock()
					} else if g.log != nil {
						g.log.Warn("glue schema version refresh failed — using cached ID",
							map[string]interface{}{"schema": name, "error": err.Error()})
					} else {
						slog.Warn("glue schema version refresh failed — using cached ID",
							"schema", name, "error", err.Error())
					}
				}
			}
		}
	}()
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

// NoopCodec is a re-export of platform-events' own identity/reference Codec
// implementation (events.NoopCodec, itself an alias for the library's
// internal port.NoopCodec) — kept as a name in this package purely for
// discoverability alongside GlueCodec, since main.go chooses between the two
// based on whether GLUE_REGISTRY_NAME is set:
//
//	var codec events.Codec = eventbus.NoopCodec{}
//	if registry := os.Getenv("GLUE_REGISTRY_NAME"); registry != "" {
//	    codec, err = eventbus.NewGlueCodec(ctx, glueClient, registry, []string{
//	        domain.EventDelegationStarted, domain.EventDelegationEnded, domain.EventDelegationReviewRequested,
//	    })
//	}
//	publisher, err := events.NewSNSPublisher(snsConfig, events.WithCodec(codec))
//
// Used in dev/test environments without AWS: Encode returns the payload
// unchanged with an empty schemaID (no wire format change, dataschema stays
// absent from the envelope); Decode returns its input unchanged.
type NoopCodec = events.NoopCodec
