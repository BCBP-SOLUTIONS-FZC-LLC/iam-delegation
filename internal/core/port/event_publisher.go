package port

import "context"

import "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"

// EventPublisher is the event sink services enqueue onto inside a
// TxRunner.RunInTx. The eventbus Publisher validates the payload, wraps it
// in an events.Envelope, and calls outbox.Enqueue on the pgx.Tx bound in
// ctx via TxFromContext (DLG-EVT-1). Callers never pass a pgx.Tx.
type EventPublisher interface {
	EnqueueCtx(ctx context.Context, evt *domain.DomainEvent) error
}

type eventPublisherCtxKey struct{}

// WithEventPublisher binds a tx-scoped EventPublisher into ctx. Called by
// the postgres TxRunner implementation inside RunInTx so service code never
// touches the outbox/pgx.Tx directly.
func WithEventPublisher(ctx context.Context, p EventPublisher) context.Context {
	return context.WithValue(ctx, eventPublisherCtxKey{}, p)
}

// EventPublisherFromContext retrieves the tx-scoped publisher bound by
// WithEventPublisher. Service code calls this inside a TxRunner.RunInTx
// closure; a nil/false result means no publisher is bound (e.g. a read-only
// tx) and the caller should skip emission rather than fail.
func EventPublisherFromContext(ctx context.Context) (EventPublisher, bool) {
	p, ok := ctx.Value(eventPublisherCtxKey{}).(EventPublisher)
	return p, ok
}
