package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSkipDuplicate_IsProcessedError_Propagates(t *testing.T) {
	idem := newFakeIdempotencyStore()
	idem.isProcessedErr = errors.New("valkey down")

	_, err := skipDuplicate(context.Background(), idem, consumerCascade, uuid.New().String())
	require.Error(t, err)
}

func TestSkipDuplicate_Duplicate_RecordsMetric(t *testing.T) {
	prev := metrics.Live
	m, err := metrics.RegisterOn(prometheus.NewRegistry(), metrics.RegisterConfig{})
	require.NoError(t, err)
	metrics.Live = m
	t.Cleanup(func() { metrics.Live = prev })

	idem := newFakeIdempotencyStore()
	eventID := uuid.New().String()
	idem.processed[consumerCascade+":"+eventID] = true

	skip, err := skipDuplicate(context.Background(), idem, consumerCascade, eventID)
	require.NoError(t, err)
	assert.True(t, skip)
}

func TestMarkProcessedInTx_NilTx_MarksDirectly(t *testing.T) {
	idem := newFakeIdempotencyStore()
	eventID := uuid.New().String()

	err := markProcessedInTx(context.Background(), nil, idem, consumerCascade, eventID)
	require.NoError(t, err)
	assert.True(t, idem.processed[consumerCascade+":"+eventID])
}

func TestMarkProcessedInTx_NilTx_PropagatesMarkError(t *testing.T) {
	idem := newFakeIdempotencyStore()
	idem.markProcessedErr = errors.New("db down")

	err := markProcessedInTx(context.Background(), nil, idem, consumerCascade, uuid.New().String())
	require.Error(t, err)
}

func TestMarkProcessedInTx_WithTx_PropagatesMarkError(t *testing.T) {
	idem := newFakeIdempotencyStore()
	idem.markProcessedErr = errors.New("db down")

	err := markProcessedInTx(context.Background(), passthroughTx{}, idem, consumerCascade, uuid.New().String())
	require.Error(t, err)
}

func TestAckUnknown_RecordsMetricAndMarksProcessed(t *testing.T) {
	prev := metrics.Live
	m, err := metrics.RegisterOn(prometheus.NewRegistry(), metrics.RegisterConfig{})
	require.NoError(t, err)
	metrics.Live = m
	t.Cleanup(func() { metrics.Live = prev })

	idem := newFakeIdempotencyStore()
	eventID := uuid.New().String()
	env := events.Envelope[json.RawMessage]{ID: eventID, Type: "SomeUnknownEvent", Payload: json.RawMessage(`{}`)}

	err = ackUnknown(context.Background(), passthroughTx{}, idem, noopLogger{}, consumerCascade, env)
	require.NoError(t, err)
	assert.True(t, idem.processed[consumerCascade+":"+eventID])
}
