package port

import (
	"context"
)

// TxRunner abstracts pgcommon.RunInTx so internal/core stays framework-free
// (LLD §6.2). Implementations bind the tenant GUC and a tx-scoped
// EventPublisher (via WithEventPublisher) into the callback's context
// before invoking fn.
//
// Services depend on this port; they never touch pgx directly. Repositories
// participating in the tx read ctx via TxFromContext (through the
// postgres.withPool helper) so a write inside RunInTx joins the same tx.
type TxRunner interface {
	RunInTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// txKey is the context key WithTx/TxFromContext use to carry the active
// transaction handle. Lives here (not in adapter/outbound/postgres) so
// outbound adapters outside the postgres package — e.g.
// eventbus.Publisher.EnqueueCtx — can read it without importing another
// outbound adapter, matching iam-user-profile / iam-org-membership.
//
// Stored/retrieved as `any`, not the concrete pgx.Tx type: internal/core
// imports no adapter, Gin, pgx, or AWS SDK (this file's package doc rule
// above). The postgres adapter (WithTx's only setter) and eventbus (its
// only getter) each hold the pgx.Tx type assertion on their own side of
// this seam — core/port itself never needs to know what a transaction
// handle actually is.
type txKey struct{}

// WithTx stores the active transaction handle in ctx so repository helpers
// and EventPublisher.EnqueueCtx join the same transaction. tx is opaque to
// this package — callers set it from, and read it back as, the same
// concrete type (pgx.Tx, in every current caller).
func WithTx(ctx context.Context, tx any) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

// TxFromContext retrieves the active transaction handle set by WithTx, if
// any. The caller is responsible for asserting it back to the concrete
// type it expects (pgx.Tx, in every current caller).
func TxFromContext(ctx context.Context) (any, bool) {
	tx := ctx.Value(txKey{})
	return tx, tx != nil
}
