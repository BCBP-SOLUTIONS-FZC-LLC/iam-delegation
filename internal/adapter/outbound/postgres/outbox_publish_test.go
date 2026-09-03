package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/outbox"
)

// testOutboxPublisher is a test-only EventPublisher that writes a real
// outbox_events row. Production enqueue is eventbus.Publisher; this package
// cannot import eventbus (eventbus already imports postgres).
type testOutboxPublisher struct{}

func (testOutboxPublisher) EnqueueCtx(ctx context.Context, evt *domain.DomainEvent) error {
	payload, err := json.Marshal(evt.Data)
	if err != nil {
		return err
	}
	env := events.NewEnvelope(
		evt.Type,
		domain.Source,
		json.RawMessage(payload),
		events.WithTenantID(evt.TenantID.String()),
		events.WithSchemaVersion("1"),
		events.WithActor(evt.Actor),
		events.WithSubject(evt.Subject),
	)
	tx, ok := TxFromContext(ctx)
	if !ok {
		return errors.New("event enqueue requires an open RunInTx transaction")
	}
	return outbox.Enqueue(ctx, tx, env)
}

// TestTxRunner_EnqueueCtx_EnvelopeHasSchemaVersion is a regression test for
// events.WithSchemaVersion("1"): published envelopes must carry specversion
// "1" on the real outbox_events row.
func TestTxRunner_EnqueueCtx_EnvelopeHasSchemaVersion(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	runner := NewTxRunner(db.App, testOutboxPublisher{})
	tenantID := uuid.New()
	gucCtx := withTenant(ctx, tenantID)

	err := runner.RunInTx(gucCtx, func(txCtx context.Context) error {
		got, ok := port.EventPublisherFromContext(txCtx)
		require.True(t, ok, "TxRunner must bind a port.EventPublisher into the tx context")
		return got.EnqueueCtx(txCtx, &domain.DomainEvent{
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
