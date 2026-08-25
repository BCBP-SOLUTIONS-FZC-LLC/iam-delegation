package postgres

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
)

// TestTxRunner_EnqueueCtx_EnvelopeHasSchemaVersion is a regression test for
// the missing events.WithSchemaVersion("1") call: iam-user-profile and
// iam-org-membership both stamp "1" onto every published envelope's
// specversion field, but this service's txBoundPublisher previously never
// called WithSchemaVersion at all, silently omitting specversion from every
// event this service publishes. Asserts against the real outbox_events row,
// not just the in-memory envelope, so a future regression that only breaks
// serialization would still be caught.
func TestTxRunner_EnqueueCtx_EnvelopeHasSchemaVersion(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	runner := NewTxRunner(db.App, nil)
	tenantID := uuid.New()
	gucCtx := withTenant(ctx, tenantID)

	err := runner.RunInTx(gucCtx, func(txCtx context.Context) error {
		pub, ok := port.EventPublisherFromContext(txCtx)
		require.True(t, ok, "TxRunner must bind a port.EventPublisher into the tx context")
		return pub.EnqueueCtx(txCtx, &domain.DomainEvent{
			Type:     domain.EventDelegationStarted,
			TenantID: tenantID,
			Subject:  "test-subject",
			Actor:    "test-actor",
			Data:     map[string]string{"k": "v"},
		})
	})
	require.NoError(t, err)

	var specVersion, eventType string
	err = db.Raw.QueryRow(ctx,
		`SELECT payload->>'specversion', payload->>'type' FROM outbox_events WHERE event_type = $1`,
		domain.EventDelegationStarted,
	).Scan(&specVersion, &eventType)
	require.NoError(t, err)
	require.Equal(t, "1", specVersion, "published envelope must carry specversion=1, matching iam-user-profile/iam-org-membership")
	require.Equal(t, domain.EventDelegationStarted, eventType)
}
