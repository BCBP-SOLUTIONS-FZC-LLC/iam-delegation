package port

import "context"

// TxRunner abstracts pgcommon.RunInTx so internal/core stays framework-free
// (LLD §6.2). Implementations bind the tenant GUC and a tx-scoped
// EventPublisher (via WithEventPublisher) into the callback's context
// before invoking fn.
type TxRunner interface {
	RunInTx(ctx context.Context, fn func(ctx context.Context) error) error
}
