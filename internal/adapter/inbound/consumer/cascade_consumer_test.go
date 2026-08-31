package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

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
}

func (f *fakeCascadeService) EndForUser(_ context.Context, tenantID, userID uuid.UUID) error {
	f.endForUserCalls = append(f.endForUserCalls, endForUserCall{tenantID, userID})
	return f.endForUserErr
}

func (f *fakeCascadeService) ScrubTenant(_ context.Context, tenantID uuid.UUID) error {
	f.scrubTenantCalls = append(f.scrubTenantCalls, tenantID)
	return f.scrubTenantErr
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
	c := NewCascadeConsumer(cascade, idem, fakeGUCBinder(&gucCalls), noopLogger{})

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
	c := NewCascadeConsumer(cascade, idem, fakeGUCBinder(&gucCalls), noopLogger{})

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
	c := NewCascadeConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}), noopLogger{})

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
	c := NewCascadeConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}), noopLogger{})

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

func TestHandle_MalformedEnvelopeID_AcksWithoutCrashing(t *testing.T) {
	cascade := &fakeCascadeService{}
	idem := newFakeIdempotencyStore()
	c := NewCascadeConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}), noopLogger{})

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
	c := NewCascadeConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}), noopLogger{})

	env := mustEnvelope(t, uuid.New().String(), "SomeOtherEvent", json.RawMessage(`{}`))

	if err := c.Handle(context.Background(), env); err != nil {
		t.Fatalf("expected nil (ack) for an unknown event type, got %v", err)
	}
	if len(cascade.endForUserCalls) != 0 || len(cascade.scrubTenantCalls) != 0 {
		t.Fatalf("no cascade method should have been called for an unknown event type")
	}
	if len(idem.markCalls) != 0 {
		t.Fatalf("MarkProcessed should not have been called for an unknown event type")
	}
}

// FM-CASC-01: MembershipRevoked with zero active delegations → cascade is no-op
func TestHandle_MembershipRevoked_NoActiveDelegations_NoOp(t *testing.T) {
	cascade := &fakeCascadeService{} // EndForUser returns nil (no error, no delegations ended)
	idem := newFakeIdempotencyStore()
	c := NewCascadeConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}), noopLogger{})

	tenantID, userID := uuid.New(), uuid.New()
	eventID := uuid.New().String()
	env := mustEnvelope(t, eventID, domain.EventMembershipRevoked, domain.MembershipRevokedPayload{
		TenantID: tenantID, UserID: userID, ActorID: uuid.New(),
	})

	if err := c.Handle(context.Background(), env); err != nil {
		t.Fatalf("Handle must not error when there are no active delegations: %v", err)
	}
	if len(cascade.endForUserCalls) != 1 {
		t.Fatalf("EndForUser must still be called even when there are no rows: got %d calls", len(cascade.endForUserCalls))
	}
	if !idem.processed[consumerCascade+":"+eventID] {
		t.Fatalf("event must be marked processed even on a no-op cascade")
	}
}

// CASC-MR-03: delegator-side delegations are silently ended by the cascade service;
// the consumer's job is to correctly dispatch to EndForUser and trust the service.
func TestHandle_MembershipRevoked_DelegateSideEmitsEvent_DelegatorSideDoesNot(t *testing.T) {
	// This test exercises the consumer dispatch path. The asymmetric event logic
	// (delegate-side emits DelegationEnded, delegator-side is silent) lives in
	// CascadeService.EndForUser and is covered by cascade_service_test.go.
	// Here we verify the consumer always dispatches to EndForUser without
	// second-guessing which rows are affected.
	cascade := &fakeCascadeService{}
	idem := newFakeIdempotencyStore()
	c := NewCascadeConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}), noopLogger{})

	tenantID, userID := uuid.New(), uuid.New()
	env := mustEnvelope(t, uuid.New().String(), domain.EventMembershipRevoked, domain.MembershipRevokedPayload{
		TenantID: tenantID, UserID: userID, ActorID: uuid.New(),
	})

	if err := c.Handle(context.Background(), env); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(cascade.endForUserCalls) != 1 || cascade.endForUserCalls[0].userID != userID {
		t.Fatalf("consumer must dispatch EndForUser with the revoked userID; got %+v", cascade.endForUserCalls)
	}
}

// CASC-TO-02 / FM-CASC-02: TenantOffboarded → no DelegationEnded event emitted by consumer
// (events are the cascade service's responsibility; consumer only dispatches).
func TestHandle_TenantMembershipsPurged_ConsumerOnlyDispatches(t *testing.T) {
	cascade := &fakeCascadeService{}
	idem := newFakeIdempotencyStore()
	c := NewCascadeConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}), noopLogger{})

	tenantID := uuid.New()
	eventID := uuid.New().String()
	env := mustEnvelope(t, eventID, domain.EventTenantMembershipsPurged, domain.TenantMembershipsPurgedPayload{
		TenantID: tenantID, ActorID: uuid.New(),
	})

	if err := c.Handle(context.Background(), env); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if len(cascade.scrubTenantCalls) != 1 || cascade.scrubTenantCalls[0] != tenantID {
		t.Fatalf("consumer must dispatch ScrubTenant with tenantID; got %+v", cascade.scrubTenantCalls)
	}
	if len(cascade.endForUserCalls) != 0 {
		t.Fatalf("consumer must not call EndForUser for TenantMembershipsPurged")
	}
}

// FM-CASC-05: IsProcessed DB error → Handle returns error, cascade NOT applied
func TestHandle_IsProcessedError_ReturnsError(t *testing.T) {
	cascade := &fakeCascadeService{}
	idem := newFakeIdempotencyStore()
	idem.isProcessedErr = errors.New("valkey unavailable")
	c := NewCascadeConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}), noopLogger{})

	env := mustEnvelope(t, uuid.New().String(), domain.EventMembershipRevoked, domain.MembershipRevokedPayload{
		TenantID: uuid.New(), UserID: uuid.New(), ActorID: uuid.New(),
	})

	if err := c.Handle(context.Background(), env); err == nil {
		t.Fatalf("Handle must return an error when IsProcessed fails (idempotency check is mandatory)")
	}
	if len(cascade.endForUserCalls) != 0 {
		t.Fatalf("cascade must not proceed when idempotency check fails; got %d calls", len(cascade.endForUserCalls))
	}
}

// FM-CASC-06: malformed MembershipRevoked JSON payload → Handle returns error (routes to DLQ)
func TestHandle_MalformedPayload_ReturnsError(t *testing.T) {
	cascade := &fakeCascadeService{}
	idem := newFakeIdempotencyStore()
	c := NewCascadeConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}), noopLogger{})

	// valid envelope ID (UUID), valid type, but payload is malformed JSON
	env := events.Envelope[json.RawMessage]{
		ID:      uuid.New().String(),
		Type:    domain.EventMembershipRevoked,
		Payload: json.RawMessage(`{"not valid json`),
	}

	if err := c.Handle(context.Background(), env); err == nil {
		t.Fatalf("Handle must return an error for a malformed payload so SQS routes it to the DLQ")
	}
	if len(cascade.endForUserCalls) != 0 {
		t.Fatalf("cascade must not proceed with a malformed payload")
	}
}

// FM-CASC-10: MarkProcessed error propagates out of Handle
func TestHandle_MarkProcessedError_Propagates(t *testing.T) {
	cascade := &fakeCascadeService{}
	idem := newFakeIdempotencyStore()
	idem.markProcessedErr = errors.New("db error on mark")
	c := NewCascadeConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}), noopLogger{})

	tenantID, userID := uuid.New(), uuid.New()
	payload, _ := json.Marshal(domain.MembershipRevokedPayload{TenantID: tenantID, UserID: userID})
	env := events.Envelope[json.RawMessage]{
		ID:      uuid.New().String(),
		Type:    domain.EventMembershipRevoked,
		Payload: payload,
	}

	if err := c.Handle(context.Background(), env); err == nil {
		t.Fatal("Handle must propagate MarkProcessed error")
	}
	// The cascade DID run (dispatch happened before MarkProcessed)
	if len(cascade.endForUserCalls) == 0 {
		t.Fatal("EndForUser must be called before MarkProcessed fails")
	}
}

// FM-CASC-07: invalid/empty envelope ID → Handle acknowledges without dispatching
func TestHandle_InvalidEnvelopeID_AcknowledgesWithoutDispatch(t *testing.T) {
	cascade := &fakeCascadeService{}
	idem := newFakeIdempotencyStore()
	c := NewCascadeConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}), noopLogger{})

	env := events.Envelope[json.RawMessage]{
		ID:   "not-a-uuid",
		Type: domain.EventMembershipRevoked,
	}
	if err := c.Handle(context.Background(), env); err != nil {
		t.Fatalf("Handle with invalid envelope ID must ack (return nil), got: %v", err)
	}
	if len(cascade.endForUserCalls) != 0 {
		t.Fatalf("cascade must not be called for an envelope with an invalid ID")
	}
}

// FM-CASC-08: malformed TenantMembershipsPurged payload → Handle returns error
func TestHandle_TenantMembershipsPurged_MalformedPayload_ReturnsError(t *testing.T) {
	cascade := &fakeCascadeService{}
	idem := newFakeIdempotencyStore()
	c := NewCascadeConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}), noopLogger{})

	env := events.Envelope[json.RawMessage]{
		ID:      uuid.New().String(),
		Type:    domain.EventTenantMembershipsPurged,
		Payload: json.RawMessage(`{bad json`),
	}
	if err := c.Handle(context.Background(), env); err == nil {
		t.Fatal("Handle must return error for malformed TenantMembershipsPurged payload")
	}
	if len(cascade.scrubTenantCalls) != 0 {
		t.Fatalf("ScrubTenant must not be called on bad payload")
	}
}

// FM-CASC-09: ScrubTenant error propagates through Handle
func TestHandle_TenantMembershipsPurged_ScrubTenantError_Propagates(t *testing.T) {
	cascade := &fakeCascadeService{scrubTenantErr: errors.New("db error")}
	idem := newFakeIdempotencyStore()
	c := NewCascadeConsumer(cascade, idem, fakeGUCBinder(&[]gucBindCall{}), noopLogger{})

	tenantID := uuid.New()
	payload, _ := json.Marshal(domain.TenantMembershipsPurgedPayload{TenantID: tenantID})
	env := events.Envelope[json.RawMessage]{
		ID:      uuid.New().String(),
		Type:    domain.EventTenantMembershipsPurged,
		Payload: payload,
	}
	if err := c.Handle(context.Background(), env); err == nil {
		t.Fatal("Handle must propagate ScrubTenant error")
	}
}
