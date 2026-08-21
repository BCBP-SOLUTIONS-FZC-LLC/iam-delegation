package consumer

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	events "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

// processed_events consumer-name buckets (LLD §7.2.3, "consumer ∈
// {cascade, offboarding}") — see ProcessedEvents' doc comment for why one
// Go consumer type uses two bucket names.
const (
	consumerCascade     = "cascade"
	consumerOffboarding = "offboarding"
)

// systemPrincipal is the GUC user identity bound for this consumer's
// system-initiated writes — there is no per-request caller identity for an
// event delivery. Mirrors the "iam-system" convention used elsewhere in the
// IAM services (GUCBridgeMiddleware, cron jobs) for the same reason.
const systemPrincipal = "iam-system"

// cascadeService is the minimal slice of *service.CascadeService this
// consumer needs, kept local so this package doesn't couple to the rest of
// that type's surface — satisfied implicitly (Go structural typing) by
// *service.CascadeService. Lets tests inject a fake.
type cascadeService interface {
	EndForUser(ctx context.Context, tenantID, userID uuid.UUID) error
	ScrubTenant(ctx context.Context, tenantID uuid.UUID) error
}

// idempotencyStore is the minimal slice of *ProcessedEvents this consumer
// needs. Satisfied implicitly by *ProcessedEvents; lets tests inject a fake.
type idempotencyStore interface {
	IsProcessed(ctx context.Context, consumer, eventID string) (bool, error)
	MarkProcessed(ctx context.Context, consumer, eventID string) error
}

// Logger is the structured logging port this consumer needs. The map-based
// signature matches platform-events' internal port.Logger and
// platform-gincommon's ZapLogger, so a caller's existing logger typically
// satisfies this directly with no adapter (see wiring.go).
type Logger interface {
	Debug(msg string, fields map[string]interface{})
	Info(msg string, fields map[string]interface{})
	Warn(msg string, fields map[string]interface{})
	Error(msg string, fields map[string]interface{})
}

// GUCBinder binds the tenant (and a system user identity) into ctx as a
// pgcommon GUCSet, so that whatever the cascade handler does downstream —
// CascadeService.EndForUser's TxRunner, CascadeService.ScrubTenant's
// direct repository calls — sees app.tenant_id bound before it runs any
// UPDATE (LLD §7.3 RLS-6: "the cascade consumer MUST bind it before any
// UPDATE, or the WITH CHECK silently affects zero rows"). Injected as a
// function rather than importing the postgres adapter package directly —
// this package stays framework-free / adapter-to-adapter decoupled, per
// Clean Architecture. Typically wired to a small wrapper around
// pgcommon.WithGUCSet in cmd/server.
//
// Bound explicitly from the decoded payload's TenantID (not env.TenantID)
// deliberately: platform-events' SQS loop already injects
// pgcommon.WithGUCSet(ctx, GUCSet{TenantID: env.TenantID}) before Handle is
// even called, but that trusts the raw envelope attribute. Binding again
// here from the schema-validated payload is redundant-but-safe belt and
// suspenders against a producer bug where the envelope's TenantID
// attribute and the payload's tenant_id disagree or the attribute is
// empty — the payload is what CascadeService actually acts on, so it's
// also what must gate the RLS check.
type GUCBinder func(ctx context.Context, tenantID uuid.UUID, userID string) context.Context

// CascadeConsumer handles delegation-cascade-q — this service's one
// inbound subscription (LLD §10.1). Its Handle method matches
// platform-events' events.Handler function type
// (func(ctx, events.Envelope[json.RawMessage]) error) and is passed
// directly to events.NewSQSConsumerWithClient (see wiring.go).
type CascadeConsumer struct {
	cascade     cascadeService
	idempotency idempotencyStore
	bindGUC     GUCBinder
	logger      Logger
}

// NewCascadeConsumer builds a CascadeConsumer.
//
//	NewCascadeConsumer(cascade *service.CascadeService, idempotency *ProcessedEvents, bindGUC GUCBinder, logger Logger) *CascadeConsumer
//
// cascade and idempotency are declared here against small local interfaces
// (cascadeService, idempotencyStore) rather than the concrete types, but
// *service.CascadeService and *ProcessedEvents both satisfy them
// structurally, so cmd/server can pass the concrete types directly.
func NewCascadeConsumer(cascade cascadeService, idempotency idempotencyStore, bindGUC GUCBinder, logger Logger) *CascadeConsumer {
	return &CascadeConsumer{cascade: cascade, idempotency: idempotency, bindGUC: bindGUC, logger: logger}
}

// Handle implements the events.Handler function signature.
//
// Idempotency is checked before doing any work and recorded only after the
// dispatched cascade fully succeeds, so a crash mid-cascade simply leaves
// the event unmarked and redelivery reruns the (idempotent)
// EndForUser/ScrubTenant call to completion.
//
// A malformed/unparseable envelope id is logged and acked (returns nil,
// ack-and-drop) — redelivery can never fix a permanently malformed id, so
// retrying it forever would just wedge the queue (mirrors
// iam-user-profile's user_event_consumer.go). An unrecognized event type is
// also acked without dispatching — this queue's SNS filter policy only
// admits MembershipRevoked/TenantOffboarded, so anything else arriving is
// unexpected but must not crash the consumer. A payload that fails to
// JSON-decode returns an error instead: LLD §10.1 routes a schema-decode
// failure to the DLQ rather than retrying indefinitely, which happens
// automatically via the queue's redrive policy (maxReceiveCount=5) once
// this handler keeps returning an error — no explicit DLQ-routing code
// belongs here.
func (c *CascadeConsumer) Handle(ctx context.Context, env events.Envelope[json.RawMessage]) error {
	eventID, err := uuid.Parse(env.ID)
	if err != nil || eventID == uuid.Nil {
		c.logger.Error("envelope has invalid or missing id — cannot dedup, acknowledging to prevent redelivery loop", map[string]interface{}{
			"event_type": env.Type,
			"event_id":   env.ID,
		})
		return nil //nolint:nilerr // deliberate: a malformed id can never be fixed by redelivery, so ack rather than loop
	}

	var consumerName string
	switch env.Type {
	case domain.EventMembershipRevoked:
		consumerName = consumerCascade
	case domain.EventTenantOffboarded:
		consumerName = consumerOffboarding
	default:
		c.logger.Warn("no handler registered for event type on delegation-cascade-q — acknowledging to prevent redelivery", map[string]interface{}{
			"event_type": env.Type,
			"event_id":   env.ID,
		})
		return nil
	}

	processed, err := c.idempotency.IsProcessed(ctx, consumerName, eventID.String())
	if err != nil {
		return fmt.Errorf("cascadeconsumer: check idempotency for event %s: %w", eventID, err)
	}
	if processed {
		c.logger.Info("duplicate event delivery, skipping cascade", map[string]interface{}{
			"event_type": env.Type,
			"event_id":   eventID.String(),
			"consumer":   consumerName,
		})
		return nil
	}

	var handleErr error
	switch env.Type {
	case domain.EventMembershipRevoked:
		handleErr = c.handleMembershipRevoked(ctx, env)
	case domain.EventTenantOffboarded:
		handleErr = c.handleTenantOffboarded(ctx, env)
	}
	if handleErr != nil {
		return handleErr
	}

	if err := c.idempotency.MarkProcessed(ctx, consumerName, eventID.String()); err != nil {
		return fmt.Errorf("cascadeconsumer: mark event %s processed: %w", eventID, err)
	}

	c.logger.Info("cascade complete", map[string]interface{}{
		"event_type": env.Type,
		"event_id":   eventID.String(),
		"consumer":   consumerName,
	})
	return nil
}

// handleMembershipRevoked decodes a MembershipRevoked payload and dispatches
// to CascadeService.EndForUser (LLD §11.5).
func (c *CascadeConsumer) handleMembershipRevoked(ctx context.Context, env events.Envelope[json.RawMessage]) error {
	var payload domain.MembershipRevokedPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return fmt.Errorf("cascadeconsumer: decode MembershipRevoked payload for event %s: %w", env.ID, err)
	}

	gucCtx := c.bindGUC(ctx, payload.TenantID, systemPrincipal)
	if err := c.cascade.EndForUser(gucCtx, payload.TenantID, payload.UserID); err != nil {
		return fmt.Errorf("cascadeconsumer: EndForUser tenant=%s user=%s: %w", payload.TenantID, payload.UserID, err)
	}
	return nil
}

// handleTenantOffboarded decodes a TenantOffboarded payload and dispatches
// to CascadeService.ScrubTenant (LLD §11.6).
func (c *CascadeConsumer) handleTenantOffboarded(ctx context.Context, env events.Envelope[json.RawMessage]) error {
	var payload domain.TenantOffboardedPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return fmt.Errorf("cascadeconsumer: decode TenantOffboarded payload for event %s: %w", env.ID, err)
	}

	gucCtx := c.bindGUC(ctx, payload.TenantID, systemPrincipal)
	if err := c.cascade.ScrubTenant(gucCtx, payload.TenantID); err != nil {
		return fmt.Errorf("cascadeconsumer: ScrubTenant tenant=%s: %w", payload.TenantID, err)
	}
	return nil
}
