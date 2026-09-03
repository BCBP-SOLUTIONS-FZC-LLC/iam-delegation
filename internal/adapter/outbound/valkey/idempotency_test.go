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

// TestIdempotencyStore_Get_DecodeFailure covers the json.Unmarshal error
// branch — a value that isn't the expected shape must return found=false
// with a non-nil error, matching the transport-failure contract.
func TestIdempotencyStore_Get_DecodeFailure(t *testing.T) {
	mr := miniredis.RunT(t)
	s := NewIdempotencyStore(NewClient("redis://"+mr.Addr()), &fakeLogger{})
	ctx := context.Background()
	tenantID := uuid.New()

	require.NoError(t, mr.Set(idempotencyKey(tenantID, "key-1"), "not-json"))

	_, found, err := s.Get(ctx, tenantID, "key-1")
	require.Error(t, err)
	require.False(t, found)
}
