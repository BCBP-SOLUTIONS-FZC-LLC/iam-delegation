package valkey

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
)

func newTestIdempotencyStore(t *testing.T) *IdempotencyStore {
	t.Helper()
	mr := miniredis.RunT(t)
	return NewIdempotencyStore(NewClient("redis://"+mr.Addr()), &fakeLogger{})
}

func TestIdempotencyStore_SaveThenGet(t *testing.T) {
	s := newTestIdempotencyStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	rec := port.IdempotencyRecord{DelegationID: uuid.New(), Status: "created"}

	_, found, err := s.Get(ctx, tenantID, "key-1")
	require.NoError(t, err)
	require.False(t, found)

	require.NoError(t, s.Save(ctx, tenantID, "key-1", rec))

	got, found, err := s.Get(ctx, tenantID, "key-1")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, rec, got)
}

func TestIdempotencyStore_ScopedByTenant(t *testing.T) {
	s := newTestIdempotencyStore(t)
	ctx := context.Background()
	rec := port.IdempotencyRecord{DelegationID: uuid.New(), Status: "created"}

	require.NoError(t, s.Save(ctx, uuid.New(), "same-key", rec))

	// A different tenant with the same client-supplied key must not see the
	// other tenant's record — del:idem: is namespaced per tenant.
	_, found, err := s.Get(ctx, uuid.New(), "same-key")
	require.NoError(t, err)
	require.False(t, found)
}

// TestIdempotencyStore_DownValkey verifies LLD §9.2/§9.3: Get returns
// found=false with a non-nil err on a transport failure (the service layer
// already treats any err from Get as "no hit, proceed"), and Save returns
// an error too (best-effort by convention — the caller must not fail the
// request on it).
func TestIdempotencyStore_DownValkey(t *testing.T) {
	mr := miniredis.RunT(t)
	addr := mr.Addr()
	mr.Close()

	s := NewIdempotencyStore(NewClient("redis://"+addr), &fakeLogger{})
	ctx := context.Background()
	tenantID := uuid.New()

	_, found, err := s.Get(ctx, tenantID, "key-1")
	require.Error(t, err)
	require.False(t, found)

	err = s.Save(ctx, tenantID, "key-1", port.IdempotencyRecord{DelegationID: uuid.New(), Status: "created"})
	require.Error(t, err)
}

// TestIdempotencyStore_DownValkey_NilLogger verifies that Get and Save with a
// nil logger degrade gracefully without panicking when Redis is unavailable.
func TestIdempotencyStore_DownValkey_NilLogger(t *testing.T) {
	mr := miniredis.RunT(t)
	addr := mr.Addr()
	mr.Close()

	s := NewIdempotencyStore(NewClient("redis://"+addr), nil) // no logger
	ctx := context.Background()
	tenantID := uuid.New()

	_, found, err := s.Get(ctx, tenantID, "key-nil")
	require.Error(t, err)
	require.False(t, found)

	err = s.Save(ctx, tenantID, "key-nil", port.IdempotencyRecord{DelegationID: uuid.New()})
	require.Error(t, err)
}

// TestIdempotencyStore_Get_CorruptValue covers the json.Unmarshal failure in Get.
func TestIdempotencyStore_Get_CorruptValue(t *testing.T) {
	mr := miniredis.RunT(t)
	fl := &fakeLogger{}
	s := NewIdempotencyStore(NewClient("redis://"+mr.Addr()), fl)
	ctx := context.Background()
	tenantID := uuid.New()

	k := idempotencyKey(tenantID, "corrupt-key")
	mr.Set(k, "not-json-at-all")

	_, found, err := s.Get(ctx, tenantID, "corrupt-key")
	require.Error(t, err)
	require.False(t, found)
	require.NotEmpty(t, fl.msg, "corrupt value must be logged")
}

// TestIdempotencyStore_Save_NilLogger_RedisError verifies Save with nil logger
// and a failing Redis returns the error without panicking.
func TestIdempotencyStore_Save_NilLogger_RedisError(t *testing.T) {
	mr := miniredis.RunT(t)
	addr := mr.Addr()
	// Don't close yet — we need to build the store first
	s := NewIdempotencyStore(NewClient("redis://"+addr), nil)
	mr.Close() // take it down after construction

	ctx := context.Background()
	err := s.Save(ctx, uuid.New(), "k", port.IdempotencyRecord{DelegationID: uuid.New()})
	require.Error(t, err)
}
