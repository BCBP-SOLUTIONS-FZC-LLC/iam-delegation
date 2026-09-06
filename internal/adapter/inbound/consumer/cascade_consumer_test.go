package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	events "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

// ── fakes ────────────────────────────────────────────────────────────────

type endForUserCall struct{ tenantID, userID uuid.UUID }

type fakeCascadeService struct {
	endForUserCalls  []endForUserCall
	endForUserErr    error
	scrubTenantCalls []uuid.UUID
	scrubTenantErr   error

	endForDisabledDelegateCalls []endForUserCall // reused shape: {tenantID, delegateID}
	endForDisabledDelegateErr   error
}

func (f *fakeCascadeService) EndForUser(_ context.Context, tenantID, userID uuid.UUID) error {
	f.endForUserCalls = append(f.endForUserCalls, endForUserCall{tenantID, userID})
	return f.endForUserErr
}

func (f *fakeCascadeService) ScrubTenant(_ context.Context, tenantID uuid.UUID) error {
	f.scrubTenantCalls = append(f.scrubTenantCalls, tenantID)
	return f.scrubTenantErr
}

func (f *fakeCascadeService) EndForDisabledDelegate(_ context.Context, tenantID, delegateID uuid.UUID) error {
	f.endForDisabledDelegateCalls = append(f.endForDisabledDelegateCalls, endForUserCall{tenantID, delegateID})
	return f.endForDisabledDelegateErr
}

type fakeIdempotencyStore struct {
	processed        map[string]bool
	isProcessedErr   error
	markProcessedErr error
	markCalls        []string
}

func newFakeIdempotencyStore() *fakeIdempotencyStore {
	return &fakeIdempotencyStore{processed: map[string]bool{}}
}

func (f *fakeIdempotencyStore) IsProcessed(_ context.Context, consumer, eventID string) (bool, error) {
	if f.isProcessedErr != nil {
		return false, f.isProcessedErr
	}
	return f.processed[consumer+":"+eventID], nil
}

func (f *fakeIdempotencyStore) MarkProcessed(_ context.Context, consumer, eventID string) error {
	f.markCalls = append(f.markCalls, consumer+":"+eventID)
	if f.markProcessedErr != nil {
		return f.markProcessedErr
	}
	f.processed[consumer+":"+eventID] = true
	return nil
}

type noopLogger struct{}

func (noopLogger) Debug(string, map[string]interface{}) {}
func (noopLogger) Info(string, map[string]interface{})  {}
func (noopLogger) Warn(string, map[string]interface{})  {}
func (noopLogger) Error(string, map[string]interface{}) {}

type gucBindCall struct {
	tenantID uuid.UUID
	userID   string
}

// passthroughTx implements port.TxRunner by invoking fn on ctx — unit tests
// don't need a real transaction; markProcessedInTx still goes through the
// production RunInTx seam.
type passthroughTx struct{}

func (passthroughTx) RunInTx(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}

func newTestConsumer(cascade cascadeService, idem idempotencyStore, bindGUC GUCBinder) *CascadeConsumer {
	return NewCascadeConsumer(cascade, idem, bindGUC, passthroughTx{}, noopLogger{})
}

// fakeGUCBinder records every bind call and returns ctx unmodified — good
// enough for these tests, which only assert on dispatch, not on RLS
// enforcement itself (that's covered by CascadeService/repository tests).
func fakeGUCBinder(calls *[]gucBindCall) GUCBinder {
	return func(ctx context.Context, tenantID uuid.UUID, userID string) context.Context {
		*calls = append(*calls, gucBindCall{tenantID, userID})
		return ctx
	}
}

func mustEnvelope(t *testing.T, id, eventType string, payload any) events.Envelope[json.RawMessage] {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return events.Envelope[json.RawMessage]{
		ID:      id,
		Type:    eventType,
		Payload: raw,
	}
}

// ── tests ────────────────────────────────────────────────────────────────

func TestHandle_MembershipRevoked_DispatchesToEndForUser(t *testing.T) {
	cascade := &fakeCascadeService{}
	idem := newFakeIdempotencyStore()
	var gucCalls []gucBindCall
	c := newTestConsumer(cascade, idem, fakeGUCBinder(&gucCalls))

	tenantID, userID := uuid.New(), uuid.New()
	eventID := uuid.New().String()
	env := mustEnvelope(t, eventID, domain.EventMembershipRevoked, domain.MembershipRevokedPayload{
		TenantID: tenantID,
		UserID:   userID,
		ActorID:  uuid.New(),
	})

	if err := c.Handle(context.Background(), env); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}

	if len(cascade.endForUserCalls) != 1 {
		t.Fatalf("expected 1 EndForUser call, got %d", len(cascade.endForUserCalls))
	}
	got := cascade.endForUserCalls[0]
	if got.tenantID != tenantID || got.userID != userID {
		t.Fatalf("EndForUser called with (%s, %s), want (%s, %s)", got.tenantID, got.userID, tenantID, userID)
	}
	if len(cascade.scrubTenantCalls) != 0 {
		t.Fatalf("ScrubTenant should not have been called")
	}
	if len(gucCalls) != 1 || gucCalls[0].tenantID != tenantID || gucCalls[0].userID != systemPrincipal {
		t.Fatalf("expected GUC bind for tenant %s as %q, got %+v", tenantID, systemPrincipal, gucCalls)
	}
	if !idem.processed[consumerCascade+":"+eventID] {
		t.Fatalf("expected event marked processed under %q bucket", consumerCascade)
	}
}

func TestHandle_TenantMembershipsPurged_DispatchesToScrubTenant(t *testing.T) {
	cascade := &fakeCascadeService{}
	idem := newFakeIdempotencyStore()
	var gucCalls []gucBindCall
	c := newTestConsumer(cascade, idem, fakeGUCBinder(&gucCalls))

	tenantID := uuid.New()
	eventID := uuid.New().String()
	env := mustEnvelope(t, eventID, domain.EventTenantMembershipsPurged, domain.TenantMembershipsPurgedPayload{
		TenantID: tenantID,
		ActorID:  uuid.New(),
	})

	if err := c.Handle(context.Background(), env); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}

	if len(cascade.scrubTenantCalls) != 1 || cascade.scrubTenantCalls[0] != tenantID {
		t.Fatalf("expected ScrubTenant called with %s, got %+v", tenantID, cascade.scrubTenantCalls)
	}
	if len(cascade.endForUserCalls) != 0 {
		t.Fatalf("EndForUser should not have been called")
	}
	if !idem.processed[consumerOffboarding+":"+eventID] {
		t.Fatalf("expected event marked processed under %q bucket", consumerOffboarding)
	}
}

func TestHandle_AlreadyProcessed_SkipsDispatch(t *testing.T) {
	cascade := &fakeCascadeService{}
	idem := newFakeIdempotencyStore()
	c := newTestConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}))

	tenantID, userID := uuid.New(), uuid.New()
	eventID := uuid.New().String()
	idem.processed[consumerCascade+":"+eventID] = true

	env := mustEnvelope(t, eventID, domain.EventMembershipRevoked, domain.MembershipRevokedPayload{
		TenantID: tenantID, UserID: userID, ActorID: uuid.New(),
	})

	if err := c.Handle(context.Background(), env); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(cascade.endForUserCalls) != 0 {
		t.Fatalf("EndForUser should not have been called for an already-processed event")
	}
	if len(idem.markCalls) != 0 {
		t.Fatalf("MarkProcessed should not have been called again")
	}
}

func TestHandle_HandlerError_DoesNotMarkProcessed(t *testing.T) {
	cascade := &fakeCascadeService{endForUserErr: errors.New("db unavailable")}
	idem := newFakeIdempotencyStore()
	c := newTestConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}))

	tenantID, userID := uuid.New(), uuid.New()
	eventID := uuid.New().String()
	env := mustEnvelope(t, eventID, domain.EventMembershipRevoked, domain.MembershipRevokedPayload{
		TenantID: tenantID, UserID: userID, ActorID: uuid.New(),
	})

	if err := c.Handle(context.Background(), env); err == nil {
		t.Fatalf("expected Handle to return an error when EndForUser fails")
	}
	if len(idem.markCalls) != 0 {
		t.Fatalf("MarkProcessed must not be called when the handler errors — got calls %v", idem.markCalls)
	}
	if idem.processed[consumerCascade+":"+eventID] {
		t.Fatalf("event must not be recorded as processed when the handler errors")
	}
}

func TestHandle_RecordsMetrics(t *testing.T) {
	prev := metrics.Live
	m, err := metrics.RegisterOn(prometheus.NewRegistry())
	require.NoError(t, err)
	metrics.Live = m
	t.Cleanup(func() { metrics.Live = prev })

	t.Run("success records CascadeProcessed", func(t *testing.T) {
		cascade := &fakeCascadeService{}
		idem := newFakeIdempotencyStore()
		c := newTestConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}))
		env := mustEnvelope(t, uuid.New().String(), domain.EventMembershipRevoked, domain.MembershipRevokedPayload{
			TenantID: uuid.New(), UserID: uuid.New(), ActorID: uuid.New(),
		})
		require.NoError(t, c.Handle(context.Background(), env))
	})

	t.Run("handler error records CascadeDLQ", func(t *testing.T) {
		cascade := &fakeCascadeService{endForUserErr: errors.New("db unavailable")}
		idem := newFakeIdempotencyStore()
		c := newTestConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}))
		env := mustEnvelope(t, uuid.New().String(), domain.EventMembershipRevoked, domain.MembershipRevokedPayload{
			TenantID: uuid.New(), UserID: uuid.New(), ActorID: uuid.New(),
		})
		require.Error(t, c.Handle(context.Background(), env))
	})

	t.Run("mark-processed failure records CascadeDLQ", func(t *testing.T) {
		cascade := &fakeCascadeService{}
		idem := newFakeIdempotencyStore()
		idem.markProcessedErr = errors.New("db down")
		c := newTestConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}))
		env := mustEnvelope(t, uuid.New().String(), domain.EventMembershipRevoked, domain.MembershipRevokedPayload{
			TenantID: uuid.New(), UserID: uuid.New(), ActorID: uuid.New(),
		})
		require.Error(t, c.Handle(context.Background(), env))
	})
}

func TestHandle_MalformedEnvelopeID_AcksWithoutCrashing(t *testing.T) {
	cascade := &fakeCascadeService{}
	idem := newFakeIdempotencyStore()
	c := newTestConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}))

	env := mustEnvelope(t, "not-a-uuid", domain.EventMembershipRevoked, domain.MembershipRevokedPayload{
		TenantID: uuid.New(), UserID: uuid.New(), ActorID: uuid.New(),
	})

	if err := c.Handle(context.Background(), env); err != nil {
		t.Fatalf("expected nil (ack-and-drop) for a malformed envelope id, got %v", err)
	}
	if len(cascade.endForUserCalls) != 0 {
		t.Fatalf("EndForUser should not have been called for a malformed envelope id")
	}
	if len(idem.markCalls) != 0 {
		t.Fatalf("MarkProcessed should not have been called for a malformed envelope id")
	}
}

func TestHandle_UnknownEventType_AcksWithoutDispatching(t *testing.T) {
	cascade := &fakeCascadeService{}
	idem := newFakeIdempotencyStore()
	c := newTestConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}))

	env := mustEnvelope(t, uuid.New().String(), "SomeOtherEvent", json.RawMessage(`{}`))

	if err := c.Handle(context.Background(), env); err != nil {
		t.Fatalf("expected nil (ack) for an unknown event type, got %v", err)
	}
	if len(cascade.endForUserCalls) != 0 || len(cascade.scrubTenantCalls) != 0 {
		t.Fatalf("no cascade method should have been called for an unknown event type")
	}
	if len(idem.markCalls) != 1 {
		t.Fatalf("MarkProcessed must be called for an unknown event type (IDEMP-2) — got %v", idem.markCalls)
	}
}

// mustUserUpdatedEnvelope builds a UserUpdated envelope with TenantID set at
// the envelope level (not the payload — UserUpdatedPayload carries no
// tenant_id field of its own, per handleUserDisabled's doc comment).
func mustUserUpdatedEnvelope(t *testing.T, id string, tenantID uuid.UUID, payload domain.UserUpdatedPayload) events.Envelope[json.RawMessage] {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return events.Envelope[json.RawMessage]{
		ID:       id,
		Type:     domain.EventUserUpdated,
		TenantID: tenantID.String(),
		Payload:  raw,
	}
}

func strPtr(s string) *string { return &s }

// TestHandle_UserUpdatedDisabled_DispatchesToEndForDisabledDelegate is Bug 2's
// consumer-level test: a UserUpdated event with status="disabled" must
// dispatch to CascadeService.EndForDisabledDelegate, bound with tenantID
// from the envelope (not the payload) and userID from the payload.
func TestHandle_UserUpdatedDisabled_DispatchesToEndForDisabledDelegate(t *testing.T) {
	cascade := &fakeCascadeService{}
	idem := newFakeIdempotencyStore()
	var gucCalls []gucBindCall
	c := newTestConsumer(cascade, idem, fakeGUCBinder(&gucCalls))

	tenantID, delegateID := uuid.New(), uuid.New()
	eventID := uuid.New().String()
	env := mustUserUpdatedEnvelope(t, eventID, tenantID, domain.UserUpdatedPayload{
		UserID:        delegateID,
		ChangedFields: []string{"status"},
		Status:        strPtr("disabled"),
	})

	if err := c.Handle(context.Background(), env); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}

	if len(cascade.endForDisabledDelegateCalls) != 1 {
		t.Fatalf("expected 1 EndForDisabledDelegate call, got %d", len(cascade.endForDisabledDelegateCalls))
	}
	got := cascade.endForDisabledDelegateCalls[0]
	if got.tenantID != tenantID || got.userID != delegateID {
		t.Fatalf("EndForDisabledDelegate called with (%s, %s), want (%s, %s)", got.tenantID, got.userID, tenantID, delegateID)
	}
	if len(gucCalls) != 1 || gucCalls[0].tenantID != tenantID || gucCalls[0].userID != systemPrincipal {
		t.Fatalf("expected GUC bind for tenant %s as %q, got %+v", tenantID, systemPrincipal, gucCalls)
	}
	if !idem.processed[consumerDelegateDisable+":"+eventID] {
		t.Fatalf("expected event marked processed under %q bucket", consumerDelegateDisable)
	}
}

// TestHandle_UserUpdatedNotDisabled_AcksWithoutDispatching covers the common
// case: this queue's filter policy admits every UserUpdated on
// iam.user.events (it can only filter on EventType), so most deliveries are
// unrelated field changes and must be acked without calling the cascade or
// touching idempotency state at all.
func TestHandle_UserUpdatedNotDisabled_AcksWithoutDispatching(t *testing.T) {
	tests := []struct {
		name    string
		payload domain.UserUpdatedPayload
	}{
		{"status changed to active, not disabled", domain.UserUpdatedPayload{
			UserID: uuid.New(), ChangedFields: []string{"status"}, Status: strPtr("active"),
		}},
		{"unrelated field changed, status absent", domain.UserUpdatedPayload{
			UserID: uuid.New(), ChangedFields: []string{"display_name"},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cascade := &fakeCascadeService{}
			idem := newFakeIdempotencyStore()
			c := newTestConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}))

			env := mustUserUpdatedEnvelope(t, uuid.New().String(), uuid.New(), tc.payload)
			if err := c.Handle(context.Background(), env); err != nil {
				t.Fatalf("expected nil (ack) for a non-disabled UserUpdated, got %v", err)
			}
			if len(cascade.endForDisabledDelegateCalls) != 0 {
				t.Fatalf("EndForDisabledDelegate must not be called")
			}
			if len(idem.markCalls) != 0 {
				t.Fatalf("MarkProcessed should not have been called")
			}
			if len(idem.processed) != 0 {
				t.Fatalf("IsProcessed should not have been checked either — no cascade work means nothing to dedup")
			}
		})
	}
}

// TestHandle_UserUpdatedMalformedPayload_ReturnsErrorForDLQ covers the
// decode-failure path in the pre-dispatch switch (not the dispatch
// switch's own decode) — must return an error so the SQS redrive policy
// eventually routes it to the DLQ, not silently ack a payload nobody could
// interpret.
func TestHandle_UserUpdatedMalformedPayload_ReturnsErrorForDLQ(t *testing.T) {
	cascade := &fakeCascadeService{}
	idem := newFakeIdempotencyStore()
	c := newTestConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}))

	env := events.Envelope[json.RawMessage]{
		ID:       uuid.New().String(),
		Type:     domain.EventUserUpdated,
		TenantID: uuid.New().String(),
		Payload:  json.RawMessage(`{"status": 12345}`), // status must be a string
	}

	if err := c.Handle(context.Background(), env); err == nil {
		t.Fatalf("expected a decode error to be returned (routed to DLQ), got nil")
	}
	if len(cascade.endForDisabledDelegateCalls) != 0 {
		t.Fatalf("EndForDisabledDelegate must not be called on a decode failure")
	}
}

func TestHandle_TenantMembershipsPurged_ScrubTenantError_ReturnsErrorForDLQ(t *testing.T) {
	cascade := &fakeCascadeService{scrubTenantErr: errors.New("db unavailable")}
	idem := newFakeIdempotencyStore()
	c := newTestConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}))
	eventID := uuid.New().String()
	env := mustEnvelope(t, eventID, domain.EventTenantMembershipsPurged, domain.TenantMembershipsPurgedPayload{
		TenantID: uuid.New(), ActorID: uuid.New(),
	})

	if err := c.Handle(context.Background(), env); err == nil {
		t.Fatalf("expected ScrubTenant's error to be returned")
	}
	if idem.processed[consumerOffboarding+":"+eventID] {
		t.Fatalf("event must not be recorded as processed when the handler errors")
	}
}

func TestHandle_UserUpdatedDisabled_EndForDisabledDelegateError_ReturnsErrorForDLQ(t *testing.T) {
	cascade := &fakeCascadeService{endForDisabledDelegateErr: errors.New("db unavailable")}
	idem := newFakeIdempotencyStore()
	c := newTestConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}))
	tenantID := uuid.New()
	eventID := uuid.New().String()
	env := mustUserUpdatedEnvelope(t, eventID, tenantID, domain.UserUpdatedPayload{
		UserID: uuid.New(), Status: strPtr("disabled"),
	})

	if err := c.Handle(context.Background(), env); err == nil {
		t.Fatalf("expected EndForDisabledDelegate's error to be returned")
	}
	if idem.processed[consumerDelegateDisable+":"+eventID] {
		t.Fatalf("event must not be recorded as processed when the handler errors")
	}
}

func TestHandle_MembershipRevokedMalformedPayload_ReturnsErrorForDLQ(t *testing.T) {
	cascade := &fakeCascadeService{}
	idem := newFakeIdempotencyStore()
	c := newTestConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}))

	env := events.Envelope[json.RawMessage]{
		ID:      uuid.New().String(),
		Type:    domain.EventMembershipRevoked,
		Payload: json.RawMessage(`{"tenant_id": 12345}`), // tenant_id must be a string
	}

	if err := c.Handle(context.Background(), env); err == nil {
		t.Fatalf("expected a decode error to be returned (routed to DLQ), got nil")
	}
	if len(cascade.endForUserCalls) != 0 {
		t.Fatalf("EndForUser must not be called on a decode failure")
	}
}

func TestHandle_TenantMembershipsPurgedMalformedPayload_ReturnsErrorForDLQ(t *testing.T) {
	cascade := &fakeCascadeService{}
	idem := newFakeIdempotencyStore()
	c := newTestConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}))

	env := events.Envelope[json.RawMessage]{
		ID:      uuid.New().String(),
		Type:    domain.EventTenantMembershipsPurged,
		Payload: json.RawMessage(`{"tenant_id": 12345}`), // tenant_id must be a string
	}

	if err := c.Handle(context.Background(), env); err == nil {
		t.Fatalf("expected a decode error to be returned (routed to DLQ), got nil")
	}
	if len(cascade.scrubTenantCalls) != 0 {
		t.Fatalf("ScrubTenant must not be called on a decode failure")
	}
}

// TestHandle_UserUpdatedInvalidTenantID_ReturnsErrorForDLQ covers
// handleUserDisabled's own uuid.Parse(env.TenantID) branch — a status:
// "disabled" delivery whose envelope carries a malformed tenant_id.
// TestHandle_UnknownEventType_AckError covers lines 170–172: ackUnknown
// propagates an error when MarkProcessed fails (e.g. idempotency store is down).
func TestHandle_UnknownEventType_AckError_PropagatesError(t *testing.T) {
	cascade := &fakeCascadeService{}
	idem := newFakeIdempotencyStore()
	idem.markProcessedErr = errors.New("db down")
	c := newTestConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}))

	env := mustEnvelope(t, uuid.New().String(), "SomeUnregisteredEventType", struct{}{})

	err := c.Handle(context.Background(), env)
	require.Error(t, err, "Handle must propagate ackUnknown's error")
	require.Empty(t, cascade.endForUserCalls)
	require.Empty(t, cascade.scrubTenantCalls)
}

// TestHandle_IdempotencyCheckError covers lines 177–179: skipDuplicate
// returns an error when IsProcessed fails (e.g. Valkey is down).
func TestHandle_IdempotencyCheckError_PropagatesError(t *testing.T) {
	cascade := &fakeCascadeService{}
	idem := newFakeIdempotencyStore()
	idem.isProcessedErr = errors.New("valkey down")
	c := newTestConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}))

	env := mustEnvelope(t, uuid.New().String(), domain.EventMembershipRevoked, domain.MembershipRevokedPayload{
		TenantID: uuid.New(), UserID: uuid.New(), ActorID: uuid.New(),
	})

	err := c.Handle(context.Background(), env)
	require.Error(t, err, "Handle must propagate the idempotency-check error")
	require.Empty(t, cascade.endForUserCalls)
}

// TestHandleUserDisabled_MalformedPayload covers lines 278–280: handleUserDisabled
// returns a decode error when user_id is not a UUID string. This path is exercised
// directly (white-box) because Handle's isUserUpdatedDisabled gate would also reject
// the same payload before dispatching, making the line unreachable via Handle.
func TestHandleUserDisabled_MalformedPayload_ReturnsError(t *testing.T) {
	cascade := &fakeCascadeService{}
	idem := newFakeIdempotencyStore()
	c := newTestConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}))

	env := events.Envelope[json.RawMessage]{
		ID:       uuid.New().String(),
		Type:     domain.EventUserUpdated,
		TenantID: uuid.New().String(),
		Payload:  json.RawMessage(`{"user_id": 99999}`), // int instead of UUID string
	}

	err := c.handleUserDisabled(context.Background(), env)
	require.Error(t, err, "handleUserDisabled must return a decode error for a malformed user_id")
	require.Empty(t, cascade.endForDisabledDelegateCalls)
}

func TestHandle_UserUpdatedInvalidTenantID_ReturnsErrorForDLQ(t *testing.T) {
	cascade := &fakeCascadeService{}
	idem := newFakeIdempotencyStore()
	c := newTestConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}))

	payload, err := json.Marshal(domain.UserUpdatedPayload{UserID: uuid.New(), Status: strPtr("disabled")})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	env := events.Envelope[json.RawMessage]{
		ID:       uuid.New().String(),
		Type:     domain.EventUserUpdated,
		TenantID: "not-a-uuid",
		Payload:  payload,
	}

	if err := c.Handle(context.Background(), env); err == nil {
		t.Fatalf("expected an error for the invalid tenant_id (routed to DLQ), got nil")
	}
	if len(cascade.endForDisabledDelegateCalls) != 0 {
		t.Fatalf("EndForDisabledDelegate must not be called when tenant_id fails to parse")
	}
}
