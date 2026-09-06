package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessedEventsRepository_IsProcessedMarkCleanup(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewProcessedEventsRepository(db.Bypass)

	ok, err := repo.IsProcessed(ctx, "cascade", "evt-1")
	require.NoError(t, err)
	assert.False(t, ok)

	require.NoError(t, repo.MarkProcessed(ctx, "cascade", "evt-1"))
	require.NoError(t, repo.MarkProcessed(ctx, "cascade", "evt-1"), "ON CONFLICT must be idempotent")

	ok, err = repo.IsProcessed(ctx, "cascade", "evt-1")
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = repo.IsProcessed(ctx, "offboarding", "evt-1")
	require.NoError(t, err)
	assert.False(t, ok, "a different consumer bucket is a distinct ledger row")

	require.NoError(t, repo.CleanupExpired(ctx, time.Nanosecond))
	ok, err = repo.IsProcessed(ctx, "cascade", "evt-1")
	require.NoError(t, err)
	assert.False(t, ok, "a 1ns retention must delete the just-written row")
}

func TestProcessedEventsRepository_Prune_BoundedDelete(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewProcessedEventsRepository(db.Bypass)

	require.NoError(t, repo.MarkProcessed(ctx, "cascade", "evt-old"))
	n, err := repo.Prune(ctx, 0, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	ok, err := repo.IsProcessed(ctx, "cascade", "evt-old")
	require.NoError(t, err)
	assert.False(t, ok, "ttlDays=0 must delete rows with processed_at < now()")
}

// TestProcessedEventsRepository_IsProcessed_CancelledContext covers lines
// 40–42: a pre-cancelled context causes the query to fail.
func TestProcessedEventsRepository_IsProcessed_CancelledContext(t *testing.T) {
	db := setupTestDB(t)
	repo := NewProcessedEventsRepository(db.Bypass)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := repo.IsProcessed(ctx, "cascade", "evt-x")
	require.Error(t, err)
}

// TestProcessedEventsRepository_Prune_CancelledContext covers lines
// 87–89: a pre-cancelled context causes the DELETE to fail.
func TestProcessedEventsRepository_Prune_CancelledContext(t *testing.T) {
	db := setupTestDB(t)
	repo := NewProcessedEventsRepository(db.Bypass)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := repo.Prune(ctx, 0, 10)
	require.Error(t, err)
}
