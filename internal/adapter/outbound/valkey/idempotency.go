package valkey

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
)

// idempotencyTTL is the LLD §9/§15 del:idem: TTL
// (cache.idempotencyTtlSeconds: 86400).
const idempotencyTTL = 24 * time.Hour

// IdempotencyStore implements port.IdempotencyStore — the create-
// idempotency-key dedup store (DLG-2, DLG-Q3/DLG-D8, LLD §9.2). Distinct
// from Cache: Get/Save here DO return errors, since the service layer
// already treats a non-nil err from either as "no hit, proceed"/"best
// effort, ignore" respectively (see delegation_service.go's Create).
type IdempotencyStore struct {
	client *redis.Client
	logger port.Logger
}

var _ port.IdempotencyStore = (*IdempotencyStore)(nil)

// NewIdempotencyStore builds an IdempotencyStore from an existing
// *redis.Client (see NewClient). logger may be nil.
func NewIdempotencyStore(client *redis.Client, logger port.Logger) *IdempotencyStore {
	return &IdempotencyStore{client: client, logger: logger}
}

func idempotencyKey(tenantID uuid.UUID, key string) string {
	return fmt.Sprintf("del:idem:%s:%s", tenantID, key)
}

// Get returns the stored record for a create idempotency key, if any.
// found is false both on a genuine cache miss (redis.Nil) and on any
// Valkey error — Valkey unavailability degrades create-idempotency to
// best-effort (LLD §9.2/§9.3); the caller already treats a non-nil err as
// "no hit, proceed".
func (s *IdempotencyStore) Get(ctx context.Context, tenantID uuid.UUID, key string) (port.IdempotencyRecord, bool, error) {
	k := idempotencyKey(tenantID, key)
	val, err := s.client.Get(ctx, k).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return port.IdempotencyRecord{}, false, nil
		}
		if s.logger != nil {
			s.logger.Warn("idempotency get failed", map[string]interface{}{"key": k, "error": err.Error()})
		}
		return port.IdempotencyRecord{}, false, err
	}
	var rec port.IdempotencyRecord
	if err := json.Unmarshal(val, &rec); err != nil {
		if s.logger != nil {
			s.logger.Warn("idempotency decode failed", map[string]interface{}{"key": k, "error": err.Error()})
		}
		return port.IdempotencyRecord{}, false, err
	}
	return rec, true, nil
}

// Save stores rec with a 24h TTL, written after the create transaction
// commits (LLD §9.2 — the caller's responsibility; this method only does
// the write). Best-effort by convention: the caller must not, and per
// delegation_service.go's Create does not, fail the request if Save
// errors.
func (s *IdempotencyStore) Save(ctx context.Context, tenantID uuid.UUID, key string, rec port.IdempotencyRecord) error {
	k := idempotencyKey(tenantID, key)
	val, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("valkey: encode idempotency record: %w", err)
	}
	if err := s.client.Set(ctx, k, val, idempotencyTTL).Err(); err != nil {
		if s.logger != nil {
			s.logger.Warn("idempotency save failed", map[string]interface{}{"key": k, "error": err.Error()})
		}
		return err
	}
	return nil
}

// Reserve atomically claims key via SET NX, storing a Status:"pending"
// placeholder under the same key/TTL a completed Save would use — closing
// the race a plain Get-then-Save pattern leaves open between two
// concurrent calls that both miss Get before either Saves (the bug this
// method was added to fix: two requests sharing one Idempotency-Key could
// otherwise both pass the Get check and both create a delegation). A
// successful Save later simply overwrites this placeholder; a failed
// caller must call Release so a retry with the same key isn't stuck
// waiting out the TTL.
func (s *IdempotencyStore) Reserve(ctx context.Context, tenantID uuid.UUID, key string) (bool, error) {
	k := idempotencyKey(tenantID, key)
	val, err := json.Marshal(port.IdempotencyRecord{Status: "pending"})
	if err != nil {
		return false, fmt.Errorf("valkey: encode idempotency reservation: %w", err)
	}
	ok, err := s.client.SetNX(ctx, k, val, idempotencyTTL).Result()
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("idempotency reserve failed", map[string]interface{}{"key": k, "error": err.Error()})
		}
		return false, err
	}
	return ok, nil
}

// Release removes a reservation this process made via Reserve.
func (s *IdempotencyStore) Release(ctx context.Context, tenantID uuid.UUID, key string) error {
	k := idempotencyKey(tenantID, key)
	if err := s.client.Del(ctx, k).Err(); err != nil {
		if s.logger != nil {
			s.logger.Warn("idempotency release failed", map[string]interface{}{"key": k, "error": err.Error()})
		}
		return err
	}
	return nil
}
