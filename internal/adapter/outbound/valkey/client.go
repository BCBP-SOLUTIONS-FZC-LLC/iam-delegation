// Package valkey implements port.Cache and port.IdempotencyStore against a
// Valkey/Redis endpoint (LLD §9). Both are advisory/best-effort: correctness
// never depends on either — a miss or a down cache always falls through to
// Postgres (§9.3), and Save failures degrade create-idempotency to
// best-effort (§9.2), logged rather than returned as a request failure.
package valkey

import (
	"time"

	"github.com/redis/go-redis/v9"
)

// Logger is the minimal structured-logging capability this package needs —
// satisfied structurally by platform-gincommon/pkg/logger's port.Logger (an
// internal type there, so this package declares its own duck-typed
// interface rather than importing it directly), or by any other logger
// whose Warn method matches this shape.
type Logger interface {
	Warn(msg string, fields map[string]interface{})
}

// NewClient builds a *redis.Client from addr, shared by NewCache and
// NewIdempotencyStore (both keyspaces — del:list: and del:idem: — live on
// the same Valkey endpoint, LLD §9). addr may be a plain host:port or a
// full URL (redis://user:pass@host or rediss://... for TLS). Timeouts are
// tight by default (100ms dial, 50ms read/write): nothing here is on a hot
// path (§9.3), so a slow/down Valkey must degrade to a fast miss rather
// than stall the request.
func NewClient(addr string) *redis.Client {
	opts, err := redis.ParseURL(addr)
	if err != nil {
		opts = &redis.Options{Addr: addr}
	}
	if opts.DialTimeout == 0 {
		opts.DialTimeout = 100 * time.Millisecond
	}
	if opts.ReadTimeout == 0 {
		opts.ReadTimeout = 50 * time.Millisecond
	}
	if opts.WriteTimeout == 0 {
		opts.WriteTimeout = 50 * time.Millisecond
	}
	return redis.NewClient(opts)
}
