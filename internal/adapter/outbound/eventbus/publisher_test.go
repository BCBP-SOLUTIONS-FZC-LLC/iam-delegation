package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
)

// fakeTx implements pgx.Tx by embedding the nil interface (any method other
// than Exec panics if called) and recording every Exec call — sufficient
// because outbox.Enqueue's InsertRecord calls only tx.Exec (verified by
// reading platform-events' outboxstore.InsertRecord).
type fakeTx struct {
	pgx.Tx
	execCalls [][]any
	execErr   error
}

func (f *fakeTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.execCalls = append(f.execCalls, append([]any{sql}, args...))
	if f.execErr != nil {
		return pgconn.CommandTag{}, f.execErr
	}
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

type failingCodec struct{ err error }

func (f failingCodec) Encode(context.Context, string, []byte) ([]byte, string, error) {
	return nil, "", f.err
}

type fakePublisherLogger struct {
	errorCount int
}

func (f *fakePublisherLogger) Debug(string, map[string]interface{}) {}
func (f *fakePublisherLogger) Info(string, map[string]interface{})  {}
func (f *fakePublisherLogger) Warn(string, map[string]interface{})  {}
func (f *fakePublisherLogger) Error(string, map[string]interface{}) { f.errorCount++ }

func TestPublisher_EnqueueCtx_HappyPath_InsertsIntoOutbox(t *testing.T) {
	p := New(domain.Source, NoopCodec{})
	tx := &fakeTx{}

	tenantID := uuid.New()
	event := &domain.DomainEvent{
		Type:     domain.EventDelegationStarted,
		TenantID: tenantID,
		Subject:  tenantID.String(),
		Actor:    domain.SystemActorID.String(),
		Data:     map[string]string{"k": "v"},
	}

	err := p.EnqueueCtx(pgadapter.WithTx(context.Background(), tx), event)
	require.NoError(t, err)
	require.Len(t, tx.execCalls, 1, "exactly one INSERT into outbox_events")

	call := tx.execCalls[0]
	sql, ok := call[0].(string)
	require.True(t, ok)
	assert.Contains(t, sql, "INSERT INTO outbox_events")
}

func TestPublisher_EnqueueCtx_RequiresOpenTx(t *testing.T) {
	p := New(domain.Source, NoopCodec{})
	err := p.EnqueueCtx(context.Background(), &domain.DomainEvent{
		Type:     domain.EventDelegationStarted,
		TenantID: uuid.New(),
		Data:     map[string]string{"k": "v"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RunInTx")
}

func TestPublisher_EnqueueCtx_CodecRejectionNeverTouchesOutbox(t *testing.T) {
	wantErr := errors.New("schema validation failed")
	p := New(domain.Source, failingCodec{err: wantErr})
	tx := &fakeTx{}

	err := p.EnqueueCtx(pgadapter.WithTx(context.Background(), tx), &domain.DomainEvent{
		Type:     domain.EventDelegationStarted,
		TenantID: uuid.New(),
		Data:     map[string]string{"k": "v"},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
	assert.Empty(t, tx.execCalls, "a codec-rejected payload must never reach the outbox insert")
}

func TestPublisher_EnqueueCtx_ExecFailurePropagates(t *testing.T) {
	p := New(domain.Source, NoopCodec{})
	execErr := errors.New("connection reset")
	tx := &fakeTx{execErr: execErr}

	err := p.EnqueueCtx(pgadapter.WithTx(context.Background(), tx), &domain.DomainEvent{
		Type:     domain.EventDelegationEnded,
		TenantID: uuid.New(),
		Data:     map[string]string{"k": "v"},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, execErr)
}

func TestPublisher_WithLogger_ReturnsSamePublisherForChaining(t *testing.T) {
	p := New(domain.Source, NoopCodec{})
	got := p.WithLogger(nil)
	assert.Same(t, p, got)
}

func TestPublisher_WithLogger_RoutesExecFailureLogThroughAttachedLogger(t *testing.T) {
	fl := &fakePublisherLogger{}
	p := New(domain.Source, NoopCodec{}).WithLogger(fl)
	tx := &fakeTx{execErr: errors.New("connection reset")}

	require.Error(t, p.EnqueueCtx(pgadapter.WithTx(context.Background(), tx), &domain.DomainEvent{
		Type:     domain.EventDelegationEnded,
		TenantID: uuid.New(),
		Data:     map[string]string{"k": "v"},
	}))
	require.Equal(t, 1, fl.errorCount, "the attached logger must receive the enqueue failure")
}

func TestPublisher_EnqueueCtx_MarshalErrorPropagates(t *testing.T) {
	p := New(domain.Source, NoopCodec{})
	tx := &fakeTx{}

	err := p.EnqueueCtx(pgadapter.WithTx(context.Background(), tx), &domain.DomainEvent{
		Type:     domain.EventDelegationEnded,
		TenantID: uuid.New(),
		Data:     make(chan int),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "marshal event payload")
	assert.Empty(t, tx.execCalls)
}

func TestPublisher_EnqueueCtx_IPAddressAndUserAgentPropagateIntoEnvelope(t *testing.T) {
	p := New(domain.Source, NoopCodec{})
	tx := &fakeTx{}

	tenantID := uuid.New()
	require.NoError(t, p.EnqueueCtx(pgadapter.WithTx(context.Background(), tx), &domain.DomainEvent{
		Type:      domain.EventDelegationEnded,
		TenantID:  tenantID,
		Subject:   tenantID.String(),
		Actor:     domain.SystemActorID.String(),
		IPAddress: "203.0.113.7",
		UserAgent: "test-agent/1.0",
		Data:      map[string]string{"k": "v"},
	}))
	require.Len(t, tx.execCalls, 1)

	var envelope struct {
		IPAddress string `json:"ip_address"`
		UserAgent string `json:"user_agent"`
	}
	payloadArg, ok := tx.execCalls[0][3].(json.RawMessage)
	require.True(t, ok)
	require.NoError(t, json.Unmarshal(payloadArg, &envelope))
	assert.Equal(t, "203.0.113.7", envelope.IPAddress)
	assert.Equal(t, "test-agent/1.0", envelope.UserAgent)
}

func TestPublisher_EnqueueCtx_ValidSpanContextPropagatesTraceAndCorrelationID(t *testing.T) {
	p := New(domain.Source, NoopCodec{})
	tx := &fakeTx{}

	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	require.NoError(t, err)
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	require.NoError(t, p.EnqueueCtx(pgadapter.WithTx(ctx, tx), &domain.DomainEvent{
		Type:     domain.EventDelegationEnded,
		TenantID: uuid.New(),
		Data:     map[string]string{"k": "v"},
	}))
	require.Len(t, tx.execCalls, 1)

	var envelope struct {
		TraceID       string `json:"trace_id"`
		CorrelationID string `json:"correlation_id"`
	}
	payloadArg, ok := tx.execCalls[0][3].(json.RawMessage)
	require.True(t, ok)
	require.NoError(t, json.Unmarshal(payloadArg, &envelope))
	assert.Equal(t, traceID.String(), envelope.TraceID)
	assert.Equal(t, spanID.String(), envelope.CorrelationID)
}

func TestPublisher_EnqueueCtx_StampsSchemaVersion(t *testing.T) {
	p := New(domain.Source, NoopCodec{})
	tx := &fakeTx{}

	require.NoError(t, p.EnqueueCtx(pgadapter.WithTx(context.Background(), tx), &domain.DomainEvent{
		Type:     domain.EventDelegationStarted,
		TenantID: uuid.New(),
		Data:     map[string]string{"k": "v"},
	}))
	require.Len(t, tx.execCalls, 1)

	var envelope struct {
		SpecVersion string `json:"specversion"`
		Type        string `json:"type"`
	}
	payloadArg, ok := tx.execCalls[0][3].(json.RawMessage)
	require.True(t, ok)
	require.NoError(t, json.Unmarshal(payloadArg, &envelope))
	assert.Equal(t, "1", envelope.SpecVersion)
	assert.Equal(t, domain.EventDelegationStarted, envelope.Type)
}
