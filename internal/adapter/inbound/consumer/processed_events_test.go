package consumer

import (
	"context"
	"fmt"
	"testing"
	"time"

	pgcommon "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const processedEventsSchema = `
CREATE TABLE IF NOT EXISTS public.processed_events (
    event_id     text NOT NULL,
    consumer     text NOT NULL,
    processed_at timestamp with time zone NOT NULL DEFAULT now(),
    CONSTRAINT processed_events_pkey PRIMARY KEY (event_id, consumer)
);`

func setupProcessedEventsDB(t *testing.T) *pgcommon.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping postgres integration test in short mode")
	}
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		Image:        "postgres:16-alpine",
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_DB":       "test",
			"POSTGRES_USER":     "postgres",
			"POSTGRES_PASSWORD": "postgres",
		},
		WaitingFor: wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).
			WithStartupTimeout(60 * time.Second),
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })

	host, err := ctr.Host(ctx)
	require.NoError(t, err)
	port, err := ctr.MappedPort(ctx, "5432")
	require.NoError(t, err)

	dsn := fmt.Sprintf("postgres://postgres:postgres@%s:%s/test?sslmode=disable", host, port.Port())

	raw, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer raw.Close()
	_, err = raw.Exec(ctx, processedEventsSchema)
	require.NoError(t, err)

	pool, err := pgcommon.NewPool(ctx, pgcommon.Config{DSN: dsn})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func TestProcessedEvents_RoundTrip(t *testing.T) {
	pool := setupProcessedEventsDB(t)
	ctx := context.Background()
	pe := NewProcessedEvents(pool)

	const consumer, eventID = "cascade", "evt-roundtrip-1"

	ok, err := pe.IsProcessed(ctx, consumer, eventID)
	require.NoError(t, err)
	require.False(t, ok, "must report not-processed before marking")

	require.NoError(t, pe.MarkProcessed(ctx, consumer, eventID))

	ok, err = pe.IsProcessed(ctx, consumer, eventID)
	require.NoError(t, err)
	require.True(t, ok, "must report processed after marking")

	// idempotent: second mark must not error
	require.NoError(t, pe.MarkProcessed(ctx, consumer, eventID))
}

func TestProcessedEvents_CleanupExpired_DeletesOldRows(t *testing.T) {
	pool := setupProcessedEventsDB(t)
	ctx := context.Background()
	pe := NewProcessedEvents(pool)

	require.NoError(t, pe.MarkProcessed(ctx, "offboarding", "evt-cleanup-1"))

	// cleanup with zero retention removes all rows
	require.NoError(t, pe.CleanupExpired(ctx, 0))

	ok, err := pe.IsProcessed(ctx, "offboarding", "evt-cleanup-1")
	require.NoError(t, err)
	require.False(t, ok, "row must have been deleted by CleanupExpired")
}

func TestProcessedEvents_SeparateConsumerBuckets(t *testing.T) {
	pool := setupProcessedEventsDB(t)
	ctx := context.Background()
	pe := NewProcessedEvents(pool)

	const eventID = "evt-buckets-1"
	require.NoError(t, pe.MarkProcessed(ctx, "cascade", eventID))

	// same event ID under a different consumer is independent
	ok, err := pe.IsProcessed(ctx, "offboarding", eventID)
	require.NoError(t, err)
	require.False(t, ok, "different consumer bucket must be independent")
}

// TestProcessedEvents_CancelledContext_ReturnsErrors covers the pool.WithConn
// error path in IsProcessed, MarkProcessed, and CleanupExpired.
func TestProcessedEvents_CancelledContext_ReturnsErrors(t *testing.T) {
	pool := setupProcessedEventsDB(t)
	pe := NewProcessedEvents(pool)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so every pool checkout fails

	_, err := pe.IsProcessed(ctx, "cascade", "any-id")
	require.Error(t, err, "IsProcessed must return error on cancelled context")

	err = pe.MarkProcessed(ctx, "cascade", "any-id")
	require.Error(t, err, "MarkProcessed must return error on cancelled context")

	err = pe.CleanupExpired(ctx, 0)
	require.Error(t, err, "CleanupExpired must return error on cancelled context")
}
