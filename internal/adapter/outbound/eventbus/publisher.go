package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/trace"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/outbox"
)

// Publisher implements port.EventPublisher. It JSON-marshals the event
// payload, validates it via the configured Codec (schema validation only —
// no wire encoding), wraps it as plain JSON in an events.Envelope, and
// inserts it into outbox_events on the transaction TxRunner attached to
// ctx — atomic with the business write (DLG-EVT-1).
//
// Glue/wire encoding is NOT performed here. It is deferred to the SNS
// publisher (outbox runner publish path) via events.WithCodec so the
// outbox stores human-readable plain JSON for observability and replay.
// Matching iam-realm-provisioner.
type Publisher struct {
	source string
	codec  Codec
	log    Logger
}

// New builds a Publisher that stamps source on every enqueued envelope and
// validates payloads via codec.
func New(source string, codec Codec) *Publisher {
	return &Publisher{source: source, codec: codec}
}

// WithLogger attaches a logger for enqueue diagnostics and returns the
// publisher for chaining.
func (p *Publisher) WithLogger(log Logger) *Publisher {
	p.log = log
	return p
}

var _ port.EventPublisher = (*Publisher)(nil)

// EnqueueCtx validates and writes one plain-JSON envelope into outbox_events
// on the transaction stored in ctx. Glue encoding is deferred to the
// outbox runner's SNS publisher (WithCodec). The method name is EnqueueCtx
// (this service's port), not realm's Enqueue.
func (p *Publisher) EnqueueCtx(ctx context.Context, evt *domain.DomainEvent) error {
	raw, err := json.Marshal(evt.Data)
	if err != nil {
		return fmt.Errorf("marshal event payload: %w", err)
	}
	if _, _, err := p.codec.Encode(ctx, evt.Type, raw); err != nil {
		return fmt.Errorf("validate event %s: %w", evt.Type, err)
	}
	opts := []events.EnvelopeOpt{
		events.WithTenantID(evt.TenantID.String()),
		events.WithSubject(evt.Subject),
		events.WithActor(evt.Actor),
		events.WithSchemaVersion("1"),
	}
	if evt.IPAddress != "" {
		opts = append(opts, events.WithIPAddress(evt.IPAddress))
	}
	if evt.UserAgent != "" {
		opts = append(opts, events.WithUserAgent(evt.UserAgent))
	}
	if spanCtx := trace.SpanFromContext(ctx).SpanContext(); spanCtx.IsValid() {
		opts = append(opts,
			events.WithTraceID(spanCtx.TraceID().String()),
			events.WithCorrelationID(spanCtx.SpanID().String()),
		)
	}
	env := events.NewEnvelope(evt.Type, p.source, json.RawMessage(raw), opts...)
	tx, ok := pgadapter.TxFromContext(ctx)
	if !ok {
		return errors.New("event enqueue requires an open RunInTx transaction")
	}
	if err := outbox.Enqueue(ctx, tx, env); err != nil {
		if p.log != nil {
			p.log.Error("outbox.Enqueue failed", map[string]interface{}{"type": evt.Type, "error": err.Error()})
		}
		return err
	}
	if p.log != nil {
		p.log.Debug("outbox.Enqueue", map[string]interface{}{"type": evt.Type, "event_id": env.ID})
	}
	return nil
}
