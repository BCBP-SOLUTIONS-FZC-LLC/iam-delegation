package valkey

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
)

// fakeLogger captures Warn() calls so tests can assert on structured
// logging without depending on platform-gincommon's concrete Zap logger.
type fakeLogger struct {
	msg    string
	fields map[string]interface{}
}

func (f *fakeLogger) Warn(msg string, fields map[string]interface{}) {
	f.msg = msg
	f.fields = fields
}

func newTestCache(t *testing.T) (*Cache, *fakeLogger) {
	t.Helper()
	mr := miniredis.RunT(t)
	fl := &fakeLogger{}
	return NewCache(NewClient("redis://"+mr.Addr()), fl), fl
}

func TestCache_SetGetInvalidate(t *testing.T) {
	c, _ := newTestCache(t)
	ctx := context.Background()
	tenantID, delegatorID := uuid.New(), uuid.New()

	_, hit := c.GetDelegatorList(ctx, tenantID, delegatorID)
	require.False(t, hit, "miss on an empty cache")

	list := []domain.Delegation{{ID: uuid.New(), TenantID: tenantID, DelegatorID: delegatorID, Scope: domain.ScopeAll}}
	c.SetDelegatorList(ctx, tenantID, delegatorID, list)

	got, hit := c.GetDelegatorList(ctx, tenantID, delegatorID)
	require.True(t, hit)
	require.Len(t, got, 1)
	require.Equal(t, list[0].ID, got[0].ID)
	require.Equal(t, domain.ScopeAll, got[0].Scope)

	c.InvalidateDelegatorList(ctx, tenantID, delegatorID)
	_, hit = c.GetDelegatorList(ctx, tenantID, delegatorID)
	require.False(t, hit, "miss after invalidation")
}

// TestCache_DownValkeyDegradesToMiss verifies LLD §9.3: a Valkey outage
// (transport error, not a plain miss) never surfaces as an error to the
// caller — GetDelegatorList degrades to (nil, false) and Set/Invalidate are
// silent no-ops, all logged internally.
func TestCache_DownValkeyDegradesToMiss(t *testing.T) {
	mr := miniredis.RunT(t)
	addr := mr.Addr()
	mr.Close()

	fl := &fakeLogger{}
	c := NewCache(NewClient("redis://"+addr), fl)
	ctx := context.Background()
	tenantID, delegatorID := uuid.New(), uuid.New()

	_, hit := c.GetDelegatorList(ctx, tenantID, delegatorID)
	require.False(t, hit)
	require.NotEmpty(t, fl.msg, "a transport error (not a plain miss) must be logged")

	// Must not panic and must remain silent no-ops.
	c.SetDelegatorList(ctx, tenantID, delegatorID, []domain.Delegation{{ID: uuid.New()}})
	c.InvalidateDelegatorList(ctx, tenantID, delegatorID)
}

func TestCache_NilLoggerIsSafe(t *testing.T) {
	mr := miniredis.RunT(t)
	addr := mr.Addr()
	mr.Close()

	c := NewCache(NewClient("redis://"+addr), nil)
	ctx := context.Background()
	_, hit := c.GetDelegatorList(ctx, uuid.New(), uuid.New())
	require.False(t, hit)
}

// TestCache_GetDelegatorList_CorruptBytes covers the json.Unmarshal failure
// path: a key exists but its value is not valid JSON.
func TestCache_GetDelegatorList_CorruptBytes(t *testing.T) {
	mr := miniredis.RunT(t)
	fl := &fakeLogger{}
	c := NewCache(NewClient("redis://"+mr.Addr()), fl)
	ctx := context.Background()
	tenantID, delegatorID := uuid.New(), uuid.New()

	key := delegatorListKey(tenantID, delegatorID)
	mr.Set(key, "not-valid-json")

	list, hit := c.GetDelegatorList(ctx, tenantID, delegatorID)
	require.False(t, hit)
	require.Nil(t, list)
	require.NotEmpty(t, fl.msg, "corrupt cache value must be logged via warn")
}

// TestCache_SetDelegatorList_RedisError covers the Set error path when Redis
// is down after the Get succeeds (simulated by closing the server).
func TestCache_SetDelegatorList_RedisError(t *testing.T) {
	mr := miniredis.RunT(t)
	fl := &fakeLogger{}
	c := NewCache(NewClient("redis://"+mr.Addr()), fl)
	ctx := context.Background()

	mr.Close() // take Redis down before the Set
	// Should not panic; error is logged internally
	c.SetDelegatorList(ctx, uuid.New(), uuid.New(), []domain.Delegation{{ID: uuid.New()}})
	// The warn is called for the Set error
	require.NotEmpty(t, fl.msg)
}
