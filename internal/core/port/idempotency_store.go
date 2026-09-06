package port

import (
	"context"

	"github.com/google/uuid"
)

// IdempotencyRecord is the stored value behind one create idempotency key
// (DLG-2, DLG-Q3/DLG-D8).
type IdempotencyRecord struct {
	DelegationID uuid.UUID
	Status       string // e.g. "created" — the HTTP status the original call returned
}

// IdempotencyStore is the create-idempotency-key dedup store's port —
// Valkey-backed, del:idem: keyspace, 24h TTL. Distinct from the inbound
// cascade consumer's processed_events ledger, which is not a port
// (implemented directly in the consumer adapter, mirroring iam-tender-acl).
type IdempotencyStore interface {
	// Get returns the stored record for a create idempotency key, if any.
	Get(ctx context.Context, tenantID uuid.UUID, key string) (rec IdempotencyRecord, found bool, err error)

	// Save stores the record with a 24h TTL. Best-effort by convention —
	// callers must not fail the request if Save errors (§9.2: Valkey down
	// degrades create-idempotency to best-effort, logged).
	Save(ctx context.Context, tenantID uuid.UUID, key string, rec IdempotencyRecord) error

	// Reserve atomically claims key for this in-flight request (SET NX):
	// reserved is false when another call already holds it (in flight or
	// already completed). Closes the race a plain Get-then-Save leaves
	// open between two concurrent calls that both miss Get before either
	// Saves. A Valkey-unavailable err degrades to best-effort, same as
	// Get/Save — the caller proceeds unreserved rather than fail the
	// request.
	Reserve(ctx context.Context, tenantID uuid.UUID, key string) (reserved bool, err error)

	// Release frees a reservation this call made via Reserve, so a client
	// retrying with the same key after a failed attempt isn't stuck
	// waiting out the TTL. Callers must only call this on a reservation
	// they themselves obtained.
	Release(ctx context.Context, tenantID uuid.UUID, key string) error
}
