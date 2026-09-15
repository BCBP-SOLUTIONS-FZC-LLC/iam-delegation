// Package consumer implements the delegation-cascade-q SQS consumer —
// Core's MembershipRevoked/TenantMembershipsPurged and User Profile's
// UserUpdated (delegate-disabled) events — plus its idempotency dedup
// (processed_events, backed by internal/adapter/outbound/postgres) and
// the wiring that assembles the consumer from cmd/server.
package consumer

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
	events "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

// processed_events consumer-name buckets (LLD §7.2.3, "consumer ∈
// {cascade, offboarding, delegate_disable}") — see ProcessedEvents' doc
// comment for why one Go consumer type uses multiple bucket names.
const (
	consumerCascade         = "cascade"
	consumerOffboarding     = "offboarding"
	consumerDelegateDisable = "delegate_disable"
)

// cascadeQueueName is the logical name of the SQS queue this consumer reads
// from — used as the queue label value on platform_messages_* Tier 1 metrics.
const cascadeQueueName = "delegation-cascade-q"

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
	// EndForDisabledDelegate handles Bug 2 — User Profile's
	// UserUpdated{status: disabled}, a second SNS subscription onto this
	// same queue (see Handle's doc comment).
	EndForDisabledDelegate(ctx context.Context, tenantID, delegateID uuid.UUID) error
}

// idempotencyStore is the minimal slice of *postgres.ProcessedEventsRepository
// this consumer needs. Satisfied implicitly; lets tests inject a fake.
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

// CascadeConsumer handles delegation-cascade-q. As of Bug 2, this queue
// carries TWO SNS subscriptions: Core's iam.membership.events
// (MembershipRevoked, TenantMembershipsPurged — the original LLD §10.1
// "one inbound subscription") and, additionally, User Profile's
// iam.user.events filtered to EventType = "UserUpdated" — provisioned
// externally (this repo does not own SNS/SQS infrastructure, only the
// consumer code and CASCADE_QUEUE_URL wiring, matching every other queue
// in this service). Its Handle method matches platform-events'
// events.Handler function type
// (func(ctx, events.Envelope[json.RawMessage]) error) and is passed
// directly to events.NewSQSConsumerWithClient (see wiring.go).
//
// Gap-6 PRE-DEPLOY ACTION REQUIRED (infrastructure team):
// The second SNS subscription (iam.user.events → delegation-cascade-q,
// filter: EventType=UserUpdated) is NOT YET PROVISIONED in any environment.
// Until it is added, the delegate-disabled cascade (EndForDisabledDelegate)
// will never fire. Add via Terraform/CDK before deploying to production.
type CascadeConsumer struct {
	cascade     cascadeService
	idempotency idempotencyStore
	bindGUC     GUCBinder
	tx          port.TxRunner
	logger      Logger
}

// NewCascadeConsumer builds a CascadeConsumer.
//
// cascade and idempotency are declared against small local interfaces
// rather than the concrete types. tx is the IDEMP-2 seam: cascade PG
// writes + MarkProcessed run inside one RunInTx (CascadeService joins
// that ambient tx), matching iam-realm-provisioner's TenantConsumer and
// iam-org-membership's MembershipEventConsumer. Unknown-type and
// filtered-UserUpdated acks also mark inside RunInTx. Nil tx marks
// directly (tests).
func NewCascadeConsumer(cascade cascadeService, idempotency idempotencyStore, bindGUC GUCBinder, tx port.TxRunner, logger Logger) *CascadeConsumer {
	return &CascadeConsumer{cascade: cascade, idempotency: idempotency, bindGUC: bindGUC, tx: tx, logger: logger}
}

// Handle implements the events.Handler function signature.
//
// Idempotency is checked before doing any work. Cascade PG writes (and
// outbox enqueue) plus the processed_events insert commit in one
// TxRunner.RunInTx (IDEMP-2, matching iam-realm-provisioner TenantConsumer
// / iam-org-membership MembershipEventConsumer). A crash mid-cascade
// rolls both back and redelivery reruns the (idempotent)
// EndForUser/ScrubTenant/EndForDisabledDelegate call to completion.
//
// A malformed/unparseable envelope id is logged and acked (returns nil,
// ack-and-drop) — redelivery can never fix a permanently malformed id, so
// retrying it forever would just wedge the queue (mirrors
// iam-user-profile's user_event_consumer.go). An unrecognized event type is
// acked, counted, and recorded in processed_events inside RunInTx
// (iam-realm-provisioner IDEMP-2) so redelivery does not storm the same
// unknown type. A UserUpdated event whose payload.status isn't "disabled"
// — the filter policy matches on EventType only, not payload content, so
// most UserUpdated deliveries are for unrelated field changes — is also
// acked without dispatching and recorded in processed_events so SQS
// redelivery does not re-decode the same no-op; only "disabled" ever
// reaches CascadeService. A payload that fails to JSON-decode returns an
// error instead: LLD §10.1 routes a schema-decode failure to the DLQ
// rather than retrying indefinitely, which happens automatically via the
// queue's redrive policy (maxReceiveCount=5) once this handler keeps
// returning an error — no explicit DLQ-routing code belongs here.
func (c *CascadeConsumer) Handle(ctx context.Context, env events.Envelope[json.RawMessage]) error {
	// Count every SQS delivery regardless of outcome (duplicate, unknown type,
	// decode error, success) — the platform_messages_received_total Tier 1
	// signal is the broadest visibility into queue throughput.
	if metrics.Live != nil {
		metrics.Live.RecordMessageReceived(cascadeQueueName)
	}

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
	case domain.EventTenantMembershipsPurged:
		consumerName = consumerOffboarding
	case domain.EventUserUpdated:
		disabled, err := isUserUpdatedDisabled(env)
		if err != nil {
			return fmt.Errorf("cascadeconsumer: decode UserUpdated payload for event %s: %w", env.ID, err)
		}
		if !disabled {
			// Most UserUpdated deliveries are unrelated field changes
			// (display_name, timezone, ...) — the filter policy can only
			// match on EventType, not payload.status. Record the ack in
			// processed_events (same bucket as the disabled path) so
			// redelivery does not re-decode the same no-op.
			consumerName = consumerDelegateDisable
			skip, err := skipDuplicate(ctx, c.idempotency, consumerName, eventID.String())
			if err != nil {
				return fmt.Errorf("cascadeconsumer: check idempotency for event %s: %w", eventID, err)
			}
			if skip {
				return nil
			}
			if err := markProcessedInTx(ctx, c.tx, c.idempotency, consumerName, eventID.String()); err != nil {
				return fmt.Errorf("cascadeconsumer: mark event %s processed: %w", eventID, err)
			}
			return nil
		}
		consumerName = consumerDelegateDisable
	default:
		if err := ackUnknown(ctx, c.tx, c.idempotency, c.logger, consumerCascade, env); err != nil {
			return fmt.Errorf("cascadeconsumer: %w", err)
		}
		return nil
	}

	skip, err := skipDuplicate(ctx, c.idempotency, consumerName, eventID.String())
	if err != nil {
		return fmt.Errorf("cascadeconsumer: check idempotency for event %s: %w", eventID, err)
	}
	if skip {
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
		handleErr = c.handleMembershipRevoked(ctx, env, consumerName, eventID.String())
	case domain.EventTenantMembershipsPurged:
		handleErr = c.handleTenantMembershipsPurged(ctx, env, consumerName, eventID.String())
	case domain.EventUserUpdated:
		handleErr = c.handleUserDisabled(ctx, env, consumerName, eventID.String())
	}
	if handleErr != nil {
		if metrics.Live != nil {
			metrics.Live.RecordCascadeDLQ()
		}
		return handleErr
	}

	if metrics.Live != nil {
		metrics.Live.RecordCascadeProcessed()
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
func (c *CascadeConsumer) handleMembershipRevoked(ctx context.Context, env events.Envelope[json.RawMessage], consumerName, eventID string) error {
	var payload domain.MembershipRevokedPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return fmt.Errorf("cascadeconsumer: decode MembershipRevoked payload for event %s: %w", env.ID, err)
	}

	gucCtx := c.bindGUC(ctx, payload.TenantID, systemPrincipal)
	return c.runCascadeAndMark(gucCtx, consumerName, eventID, func(txCtx context.Context) error {
		if err := c.cascade.EndForUser(txCtx, payload.TenantID, payload.UserID); err != nil {
			return fmt.Errorf("cascadeconsumer: EndForUser tenant=%s user=%s: %w", payload.TenantID, payload.UserID, err)
		}
		return nil
	})
}

// handleTenantMembershipsPurged decodes a TenantMembershipsPurged payload
// and dispatches to CascadeService.ScrubTenant (LLD §11.6). This is Core's
// (iam-org-membership's) tenant-scrub relay on iam.membership.events —
// renamed from "TenantOffboarded" on Core's side to avoid colliding with
// Realm Provisioner's own TenantOffboarded event.
func (c *CascadeConsumer) handleTenantMembershipsPurged(ctx context.Context, env events.Envelope[json.RawMessage], consumerName, eventID string) error {
	var payload domain.TenantMembershipsPurgedPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return fmt.Errorf("cascadeconsumer: decode TenantMembershipsPurged payload for event %s: %w", env.ID, err)
	}

	gucCtx := c.bindGUC(ctx, payload.TenantID, systemPrincipal)
	return c.runCascadeAndMark(gucCtx, consumerName, eventID, func(txCtx context.Context) error {
		if err := c.cascade.ScrubTenant(txCtx, payload.TenantID); err != nil {
			return fmt.Errorf("cascadeconsumer: ScrubTenant tenant=%s: %w", payload.TenantID, err)
		}
		return nil
	})
}

// isUserUpdatedDisabled decodes a UserUpdated payload and reports whether
// its status field is "disabled" (Bug 2). false, nil for any other status
// value or an absent status field (most UserUpdated deliveries — this
// queue's filter policy admits every UserUpdated on iam.user.events, not
// just status changes).
func isUserUpdatedDisabled(env events.Envelope[json.RawMessage]) (bool, error) {
	var payload domain.UserUpdatedPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return false, err
	}
	return payload.Status != nil && *payload.Status == "disabled", nil
}

// handleUserDisabled decodes a UserUpdated{status: disabled} payload —
// already confirmed disabled by isUserUpdatedDisabled before Handle
// dispatches here — and calls CascadeService.EndForDisabledDelegate
// (Bug 2). tenantID comes from the envelope, not the payload: unlike
// MembershipRevoked/TenantMembershipsPurged, UserUpdatedPayload carries no
// tenant_id field of its own (it is User Profile's own event, scoped by
// the envelope like every other event on iam.user.events).
func (c *CascadeConsumer) handleUserDisabled(ctx context.Context, env events.Envelope[json.RawMessage], consumerName, eventID string) error {
	var payload domain.UserUpdatedPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return fmt.Errorf("cascadeconsumer: decode UserUpdated payload for event %s: %w", env.ID, err)
	}
	tenantID, err := uuid.Parse(env.TenantID)
	if err != nil {
		return fmt.Errorf("cascadeconsumer: envelope has invalid tenant_id for event %s: %w", env.ID, err)
	}

	gucCtx := c.bindGUC(ctx, tenantID, systemPrincipal)
	return c.runCascadeAndMark(gucCtx, consumerName, eventID, func(txCtx context.Context) error {
		if err := c.cascade.EndForDisabledDelegate(txCtx, tenantID, payload.UserID); err != nil {
			return fmt.Errorf("cascadeconsumer: EndForDisabledDelegate tenant=%s delegate=%s: %w", tenantID, payload.UserID, err)
		}
		return nil
	})
}

// runCascadeAndMark opens one TxRunner transaction around the cascade
// handler and MarkProcessed (IDEMP-2). CascadeService.RunInTx joins that
// ambient tx (postgres.TxRunner), so the PG write, outbox enqueue, and
// processed_events insert commit together. EndForUser's best-effort User
// Profile call still runs after its inner RunInTx returns — while this
// outer tx is open. That call is timeout-bounded (USER_PROFILE_TIMEOUT_MS)
// and fail-open, matching the existing cascade contract. Nil tx (unit
// tests) runs fn then marks directly.
func (c *CascadeConsumer) runCascadeAndMark(ctx context.Context, consumerName, eventID string, fn func(ctx context.Context) error) error {
	if c.tx == nil {
		if err := fn(ctx); err != nil {
			return err
		}
		if err := c.idempotency.MarkProcessed(ctx, consumerName, eventID); err != nil {
			return fmt.Errorf("cascadeconsumer: mark event %s processed: %w", eventID, err)
		}
		return nil
	}
	return c.tx.RunInTx(ctx, func(txCtx context.Context) error {
		if err := fn(txCtx); err != nil {
			return err
		}
		if err := c.idempotency.MarkProcessed(txCtx, consumerName, eventID); err != nil {
			return fmt.Errorf("cascadeconsumer: mark event %s processed: %w", eventID, err)
		}
		return nil
	})
}
