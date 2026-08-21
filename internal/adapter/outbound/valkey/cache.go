package valkey

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
)

// listTTL is the LLD §9/§15 del:list: TTL (cache.listTtlSeconds: 60).
const listTTL = 60 * time.Second

// Cache implements port.Cache — the advisory del:list: read-through cache
// for DLG-1 (LLD §9.1). port.Cache has no error returns: every method here
// logs internally on failure and degrades to a cache miss/no-op.
type Cache struct {
	client *redis.Client
	logger Logger
}

var _ port.Cache = (*Cache)(nil)

// NewCache builds a Cache from an existing *redis.Client (see NewClient).
// logger may be nil, in which case failures are silently swallowed — still
// safe per port.Cache's contract, just without an operator-visible signal.
func NewCache(client *redis.Client, logger Logger) *Cache {
	return &Cache{client: client, logger: logger}
}

func delegatorListKey(tenantID, delegatorID uuid.UUID) string {
	return fmt.Sprintf("del:list:%s:%s", tenantID, delegatorID)
}

func (c *Cache) warn(msg, key string, err error) {
	if c.logger == nil {
		return
	}
	c.logger.Warn(msg, map[string]interface{}{"key": key, "error": err.Error()})
}

// GetDelegatorList returns (list, true) on a cache hit. Any Redis error
// (including redis.Nil, i.e. a plain miss) or decode failure is treated as
// a miss — correctness never depends on this cache (LLD §9.3).
func (c *Cache) GetDelegatorList(ctx context.Context, tenantID, delegatorID uuid.UUID) ([]domain.Delegation, bool) {
	key := delegatorListKey(tenantID, delegatorID)
	val, err := c.client.Get(ctx, key).Bytes()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			c.warn("delegator list cache get failed", key, err)
		}
		return nil, false
	}
	var list []domain.Delegation
	if err := json.Unmarshal(val, &list); err != nil {
		c.warn("delegator list cache decode failed", key, err)
		return nil, false
	}
	return list, true
}

// SetDelegatorList writes the DLG-1 list projection with the LLD §9 60s
// TTL. Failures are logged and otherwise ignored — this cache is advisory
// (§9.3).
func (c *Cache) SetDelegatorList(ctx context.Context, tenantID, delegatorID uuid.UUID, list []domain.Delegation) {
	key := delegatorListKey(tenantID, delegatorID)
	val, err := json.Marshal(list)
	if err != nil {
		c.warn("delegator list cache encode failed", key, err)
		return
	}
	if err := c.client.Set(ctx, key, val, listTTL).Err(); err != nil {
		c.warn("delegator list cache set failed", key, err)
	}
}

// InvalidateDelegatorList evicts the DLG-1 list projection, called on every
// DLG-2/3/4/5 write for that delegator (LLD §9). Failures are logged and
// otherwise ignored — the stale entry self-heals within its own TTL.
func (c *Cache) InvalidateDelegatorList(ctx context.Context, tenantID, delegatorID uuid.UUID) {
	key := delegatorListKey(tenantID, delegatorID)
	if err := c.client.Del(ctx, key).Err(); err != nil {
		c.warn("delegator list cache invalidate failed", key, err)
	}
}
