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
