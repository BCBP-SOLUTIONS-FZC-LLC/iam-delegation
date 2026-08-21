// Package postgres is the outbound Postgres adapter: the RLS-scoped
// delegation_app connection pool, the TxRunner that wraps
// pgcommon.RunInTx and binds a tx-scoped port.EventPublisher into context,
// and the DelegationRepository / SettingsRepository implementations.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/outbox"
	pgcdomain "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// SystemDSNFromEnv returns SYSTEM_DATABASE_URL — the delegation_migrator
// (BYPASSRLS) role's DSN, required by the reconciler's cross-tenant sweep
// queries (ListExpiringBefore, FindDueForWarning7d/3d, FindDueForAutoEnd,
// HardPurgeSoftDeletedBefore, LLD §11.3/§11.4/§18.4) and the cascade
// consumer's processed_events ledger (RLS-exempt, LLD §7.2.3). Falls back
// to DATABASE_URL (with a caller-logged warning) so a misconfigured
// environment degrades to RLS-filtered — and therefore incomplete — sweep
// results rather than failing to start, mirroring iam-user-profile's
// identical SystemDSNFromEnv.
func SystemDSNFromEnv() (dsn string, usedFallback bool) {
	if v := os.Getenv("SYSTEM_DATABASE_URL"); v != "" {
		return v, false
	}
	return os.Getenv("DATABASE_URL"), true
}

// NewPool builds the runtime app pool from the standard PG_*/DATABASE_URL
// environment variables via pgcommon.ConfigFromEnv, wiring GUCSetFromContext
// so every connection checkout injects app.tenant_id/app.user_id from
// whatever GUCSet is bound on ctx (requestctx middleware for handlers,
// WithTenantGUC for the reconciler jobs and cascade consumer). Returned
// warnings describe invalid/insecure env values that were replaced by safe
// defaults — log them at startup.
func NewPool(ctx context.Context, logger pgcdomain.Logger) (*pgcommon.Pool, []pgcommon.ConfigWarning, error) {
	cfg, warnings := pgcommon.ConfigFromEnv()
	cfg.GUCProvider = pgcommon.GUCSetFromContext
	cfg.Logger = logger
	pool, err := pgcommon.NewPool(ctx, cfg)
	return pool, warnings, err
}

// WithTenantGUC binds app.tenant_id (and, when userID is non-empty,
// app.user_id) into ctx via pgcommon's GUCSet so the next pool checkout
// issues `SET LOCAL app.tenant_id = ...` (RLS-6). The reconciler jobs
// (delegation-expiry, delegation-review) and the cascade consumer MUST call
// this before any UPDATE against a specific tenant's rows — without it the
// WITH CHECK / USING clause silently affects zero rows rather than erroring,
// the split-brain trap DEL-6 guards against (LLD §7.3).
func WithTenantGUC(ctx context.Context, tenantID uuid.UUID, userID string) context.Context {
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.TenantID = tenantID.String()
	if userID != "" {
		g.UserID = userID
	}
	return pgcommon.WithGUCSet(ctx, g)
}

// TxRunner implements port.TxRunner over pgcommon.RunInTx. Inside the
// callback it binds the active pgx.Tx into context (so repository methods
// called via port.DelegationRepository/SettingsRepository join the same
// transaction rather than opening a new one) and binds a tx-scoped
// port.EventPublisher via port.WithEventPublisher so service code can
// enqueue events atomically with the state change (DLG-EVT-1) without ever
// importing this package or touching pgx.Tx directly.
type TxRunner struct {
	pool      *pgcommon.Pool
	validator PayloadValidator
}

var _ port.TxRunner = (*TxRunner)(nil)

// PayloadValidator validates an event payload against its registered JSON
// Schema before it is written to the outbox (LLD §10.3.1: "validated
// against the registered schema" at enqueue time). Satisfied by
// *eventbus.SchemaValidator, injected via NewTxRunner rather than imported
// directly so this package doesn't depend on eventbus (Clean Architecture —
// both are outbound adapters, neither should import the other; cmd/server
// wires the concrete type). Nil skips validation.
type PayloadValidator interface {
	Validate(ctx context.Context, eventType string, payload json.RawMessage) error
}

// NewTxRunner builds a TxRunner over pool, validating every enqueued
// event's payload with validator (nil skips validation).
func NewTxRunner(pool *pgcommon.Pool, validator PayloadValidator) *TxRunner {
	return &TxRunner{pool: pool, validator: validator}
}

// RunInTx implements port.TxRunner (see the TxRunner doc comment above).
func (r *TxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return wrapConnErr(pgcommon.RunInTx(ctx, r.pool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		txCtx := withTx(ctx, tx)
		txCtx = port.WithEventPublisher(txCtx, &txBoundPublisher{tx: tx, validator: r.validator})
		return fn(txCtx)
	}))
}

// txBoundPublisher is the port.EventPublisher bound into ctx by TxRunner for
// the lifetime of one transaction. EnqueueCtx JSON-marshals evt.Data itself
// (plain JSON at enqueue time — the Glue codec, if any, is a publish-time
// concern that lives in a different package) and writes the resulting
// envelope into outbox_events within the caller's active pgx.Tx via
// outbox.Enqueue, atomic with the surrounding state change.
type txBoundPublisher struct {
	tx        pgx.Tx
	validator PayloadValidator
}

func (p *txBoundPublisher) EnqueueCtx(ctx context.Context, evt *domain.DomainEvent) error {
	payload, err := json.Marshal(evt.Data)
	if err != nil {
		return fmt.Errorf("marshal event payload: %w", err)
	}
	if p.validator != nil {
		if err := p.validator.Validate(ctx, evt.Type, payload); err != nil {
			return fmt.Errorf("event payload failed schema validation: %w", err)
		}
	}
	env := events.NewEnvelope(
		evt.Type,
		domain.Source,
		json.RawMessage(payload),
		events.WithTenantID(evt.TenantID.String()),
		events.WithTraceID(events.TraceIDFromContext(ctx)),
		events.WithActor(evt.Actor),
		events.WithIPAddress(evt.IPAddress),
		events.WithUserAgent(evt.UserAgent),
		events.WithSubject(evt.Subject),
	)
	return outbox.Enqueue(ctx, p.tx, env)
}

// txKey stores the active pgx.Tx opened by TxRunner.RunInTx in context so
// repository methods can join it instead of opening a second, independent
// transaction (which would deadlock or, worse, silently split one logical
// unit of work across two DB transactions).
type txKey struct{}

func withTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

func txFromContext(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	return tx, ok
}

// withPool runs fn against the tx already bound on ctx by TxRunner, or, when
// no tx is bound (a plain read-only call outside a TxRunner.RunInTx
// closure), opens a short-lived transaction of its own via pgcommon.RunInTx.
func withPool(ctx context.Context, pool *pgcommon.Pool, fn func(pgx.Tx) error) error {
	if tx, ok := txFromContext(ctx); ok {
		return wrapConnErr(fn(tx))
	}
	return wrapConnErr(pgcommon.RunInTx(ctx, pool, pgx.TxOptions{}, func(_ context.Context, tx pgx.Tx) error {
		return fn(tx)
	}))
}

// wrapConnErr converts connectivity failures (network unreachable, pool
// exhausted, TLS handshake) to port.ErrDependencyUnavailable so service code
// can distinguish "the database is down" from a SQL-level rejection.
// *domain.Error, *pgconn.PgError (a real SQL error the server returned),
// pgx.ErrNoRows, and context cancellation/deadline errors all pass through
// unchanged — none of those are connectivity failures.
func wrapConnErr(err error) error {
	if err == nil {
		return nil
	}
	var de *domain.Error
	if errors.As(err, &de) {
		return err
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %s", port.ErrDependencyUnavailable, err.Error())
}
