package service

import (
	"context"
	"sync"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
	"github.com/google/uuid"
)

// ── callRecorder ─────────────────────────────────────────────────────────
//
// callRecorder is a shared, mutex-guarded call log every fake in this file
// can optionally write into, so tests can assert cross-fake call ordering
// (e.g. "both membership checks happen before the User Profile call, which
// happens before the repository write") without depending on wall-clock
// timing.

type callRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *callRecorder) record(name string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, name)
}

func (r *callRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	copy(out, r.calls)
	return out
}

// firstIndex returns the index of the first entry with the given prefix, or
// -1 if none match.
func firstIndex(calls []string, prefix string) int {
	for i, c := range calls {
		if len(c) >= len(prefix) && c[:len(prefix)] == prefix {
			return i
		}
	}
	return -1
}

// countPrefix counts entries with the given prefix.
func countPrefix(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if len(c) >= len(prefix) && c[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

// ── fakeDelegationRepository ────────────────────────────────────────────

type delegationEndCall struct {
	tenantID        uuid.UUID
	id              uuid.UUID
	status          domain.DelegationStatus
	expectedVersion int64
}

type delegationExtendCall struct {
	tenantID        uuid.UUID
	id              uuid.UUID
	windowDays      int
	expectedVersion int64
}

type delegationActivateCall struct {
	tenantID        uuid.UUID
	id              uuid.UUID
	expectedVersion int64
}

type fakeDelegationRepository struct {
	rec *callRecorder

	mu    sync.Mutex
	store map[uuid.UUID]domain.Delegation

	insertCalls []domain.Delegation
	insertErr   error

	findByIDCalls int
	findByIDFn    func(ctx context.Context, tenantID, id uuid.UUID) (*domain.Delegation, error)

	endCalls  []delegationEndCall
	endResult *domain.Delegation
	endErr    error

	activateCalls  []delegationActivateCall
	activateResult *domain.Delegation
	activateErr    error

	extendCalls  []delegationExtendCall
	extendResult *domain.Delegation
	extendErr    error

	listByDelegatorErr error

	endForUserResult []domain.Delegation
	endForUserErr    error

	endForDisabledDelegateResult []domain.Delegation
	endForDisabledDelegateErr    error

	softDeleteTenantCalls int
	softDeleteTenantErr   error
}

func newFakeDelegationRepository(rec *callRecorder) *fakeDelegationRepository {
	return &fakeDelegationRepository{rec: rec, store: map[uuid.UUID]domain.Delegation{}}
}

// seed inserts d directly into the backing store (assigning an ID/Status if
// unset) without going through Insert, for tests that need FindByID to
// return a pre-existing row.
func (f *fakeDelegationRepository) seed(d domain.Delegation) domain.Delegation {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	if d.Status == "" {
		d.Status = domain.DelegationActive
	}
	f.mu.Lock()
	f.store[d.ID] = d
	f.mu.Unlock()
	return d
}

func (f *fakeDelegationRepository) List(ctx context.Context, tenantID uuid.UUID) ([]domain.Delegation, error) {
	f.rec.record("delegationRepo.List")
	return nil, nil
}

func (f *fakeDelegationRepository) ListByDelegator(ctx context.Context, tenantID, delegatorID uuid.UUID) ([]domain.Delegation, error) {
	f.rec.record("delegationRepo.ListByDelegator")
	if f.listByDelegatorErr != nil {
		return nil, f.listByDelegatorErr
	}
	return nil, nil
}

func (f *fakeDelegationRepository) FindByID(ctx context.Context, tenantID, id uuid.UUID) (*domain.Delegation, error) {
	f.rec.record("delegationRepo.FindByID")
	f.mu.Lock()
	f.findByIDCalls++
	f.mu.Unlock()
	if f.findByIDFn != nil {
		return f.findByIDFn(ctx, tenantID, id)
	}
	f.mu.Lock()
	d, ok := f.store[id]
	f.mu.Unlock()
	if !ok {
		return nil, domain.NewError(domain.ErrDelegationNotFound, "delegation not found")
	}
	cp := d
	return &cp, nil
}

func (f *fakeDelegationRepository) Insert(ctx context.Context, d *domain.Delegation) (*domain.Delegation, error) {
	f.rec.record("delegationRepo.Insert")
	snap := *d
	f.mu.Lock()
	f.insertCalls = append(f.insertCalls, snap)
	f.mu.Unlock()
	if f.insertErr != nil {
		return nil, f.insertErr
	}
	out := *d
	if out.ID == uuid.Nil {
		out.ID = uuid.New()
	}
	if out.Status == "" {
		out.Status = domain.DelegationActive
	}
	if out.RecordVersion == 0 {
		out.RecordVersion = 1
	}
	f.mu.Lock()
	f.store[out.ID] = out
	f.mu.Unlock()
	return &out, nil
}

func (f *fakeDelegationRepository) End(ctx context.Context, tenantID, id uuid.UUID, status domain.DelegationStatus, expectedVersion int64) (*domain.Delegation, error) {
	f.rec.record("delegationRepo.End")
	f.mu.Lock()
	f.endCalls = append(f.endCalls, delegationEndCall{tenantID: tenantID, id: id, status: status, expectedVersion: expectedVersion})
	f.mu.Unlock()
	if f.endErr != nil {
		return nil, f.endErr
	}
	if f.endResult != nil {
		return f.endResult, nil
	}
	f.mu.Lock()
	d, ok := f.store[id]
	if ok {
		d.Status = status
		d.RecordVersion++
		f.store[id] = d
	}
	f.mu.Unlock()
	if !ok {
		return nil, domain.NewError(domain.ErrDelegationNotFound, "delegation not found")
	}
	cp := d
	return &cp, nil
}

func (f *fakeDelegationRepository) Activate(ctx context.Context, tenantID, id uuid.UUID, expectedVersion int64) (*domain.Delegation, error) {
	f.rec.record("delegationRepo.Activate")
	f.mu.Lock()
	f.activateCalls = append(f.activateCalls, delegationActivateCall{tenantID: tenantID, id: id, expectedVersion: expectedVersion})
	f.mu.Unlock()
	if f.activateErr != nil {
		return nil, f.activateErr
	}
	if f.activateResult != nil {
		return f.activateResult, nil
	}
	f.mu.Lock()
	d, ok := f.store[id]
	if ok && d.Status == domain.DelegationScheduled {
		d.Status = domain.DelegationActive
		d.RecordVersion++
		f.store[id] = d
	} else {
		ok = false
	}
	f.mu.Unlock()
	if !ok {
		// Matches the real repository: no error, just "nothing to activate".
		return nil, nil
	}
	cp := d
	return &cp, nil
}

func (f *fakeDelegationRepository) ExtendReview(ctx context.Context, tenantID, id uuid.UUID, windowDays int, expectedVersion int64) (*domain.Delegation, error) {
	f.rec.record("delegationRepo.ExtendReview")
	f.mu.Lock()
	f.extendCalls = append(f.extendCalls, delegationExtendCall{tenantID: tenantID, id: id, windowDays: windowDays, expectedVersion: expectedVersion})
	f.mu.Unlock()
	if f.extendErr != nil {
		return nil, f.extendErr
	}
	if f.extendResult != nil {
		return f.extendResult, nil
	}
	f.mu.Lock()
	d, ok := f.store[id]
	f.mu.Unlock()
	if !ok {
		return nil, domain.NewError(domain.ErrDelegationNotFound, "delegation not found")
	}
	cp := d
	return &cp, nil
}

func (f *fakeDelegationRepository) ListExpiringBefore(ctx context.Context, before time.Time, limit int) ([]domain.Delegation, error) {
	f.rec.record("delegationRepo.ListExpiringBefore")
	return nil, nil
}

func (f *fakeDelegationRepository) ListScheduledBefore(ctx context.Context, before time.Time, limit int) ([]domain.Delegation, error) {
	f.rec.record("delegationRepo.ListScheduledBefore")
	return nil, nil
}

func (f *fakeDelegationRepository) FindActiveDeptDelegateForUser(ctx context.Context, tenantID, userID, deptID uuid.UUID) (*domain.Delegation, error) {
	f.rec.record("delegationRepo.FindActiveDeptDelegateForUser")
	return nil, nil
}

func (f *fakeDelegationRepository) FindActiveByDelegator(ctx context.Context, tenantID, delegatorID uuid.UUID) ([]domain.Delegation, error) {
	f.rec.record("delegationRepo.FindActiveByDelegator")
	return nil, nil
}

func (f *fakeDelegationRepository) FindDueForDailyWarn(ctx context.Context, now time.Time, limit int) ([]domain.Delegation, error) {
	f.rec.record("delegationRepo.FindDueForDailyWarn")
	return nil, nil
}

func (f *fakeDelegationRepository) FindDueForAutoEnd(ctx context.Context, now time.Time, limit int) ([]domain.Delegation, error) {
	f.rec.record("delegationRepo.FindDueForAutoEnd")
	return nil, nil
}

func (f *fakeDelegationRepository) MarkReviewWarned(ctx context.Context, tenantID, id uuid.UUID, bucket int, expectedVersion int64) error {
	f.rec.record("delegationRepo.MarkReviewWarned")
	return nil
}

func (f *fakeDelegationRepository) EndForUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.Delegation, error) {
	f.rec.record("delegationRepo.EndForUser")
	return f.endForUserResult, f.endForUserErr
}

func (f *fakeDelegationRepository) EndForDisabledDelegate(ctx context.Context, tenantID, delegateID uuid.UUID) ([]domain.Delegation, error) {
	f.rec.record("delegationRepo.EndForDisabledDelegate")
	return f.endForDisabledDelegateResult, f.endForDisabledDelegateErr
}

func (f *fakeDelegationRepository) SoftDeleteTenant(ctx context.Context, tenantID uuid.UUID) error {
	f.rec.record("delegationRepo.SoftDeleteTenant")
	f.mu.Lock()
	f.softDeleteTenantCalls++
	f.mu.Unlock()
	return f.softDeleteTenantErr
}

func (f *fakeDelegationRepository) HardPurgeSoftDeletedBefore(ctx context.Context, before time.Time, limit int) (int, error) {
	f.rec.record("delegationRepo.HardPurgeSoftDeletedBefore")
	return 0, nil
}

var _ port.DelegationRepository = (*fakeDelegationRepository)(nil)

// ── fakeSettingsRepository ──────────────────────────────────────────────

type settingsUpsertCall struct {
	tenantID         uuid.UUID
	maxDurationDays  int
	reviewWindowDays int
}

type fakeSettingsRepository struct {
	rec *callRecorder

	mu sync.Mutex

	getResult *domain.DelegationTenantSettings
	getErr    error
	getCalls  int

	upsertCalls  []settingsUpsertCall
	upsertResult *domain.DelegationTenantSettings
	upsertErr    error

	softDeleteTenantCalls int
	softDeleteTenantErr   error
}

func (f *fakeSettingsRepository) Get(ctx context.Context, tenantID uuid.UUID) (domain.DelegationTenantSettings, error) {
	f.rec.record("settingsRepo.Get")
	f.mu.Lock()
	f.getCalls++
	f.mu.Unlock()
	if f.getErr != nil {
		return domain.DelegationTenantSettings{}, f.getErr
	}
	if f.getResult != nil {
		return *f.getResult, nil
	}
	return domain.DefaultDelegationTenantSettings(tenantID), nil
}

func (f *fakeSettingsRepository) Upsert(ctx context.Context, tenantID uuid.UUID, maxDurationDays, reviewWindowDays int) (domain.DelegationTenantSettings, error) {
	f.rec.record("settingsRepo.Upsert")
	f.mu.Lock()
	f.upsertCalls = append(f.upsertCalls, settingsUpsertCall{tenantID: tenantID, maxDurationDays: maxDurationDays, reviewWindowDays: reviewWindowDays})
	f.mu.Unlock()
	if f.upsertErr != nil {
		return domain.DelegationTenantSettings{}, f.upsertErr
	}
	if f.upsertResult != nil {
		return *f.upsertResult, nil
	}
	return domain.DelegationTenantSettings{
		TenantID:         tenantID,
		MaxDurationDays:  maxDurationDays,
		ReviewWindowDays: reviewWindowDays,
		RecordVersion:    1,
	}, nil
}

func (f *fakeSettingsRepository) SoftDeleteTenant(ctx context.Context, tenantID uuid.UUID) error {
	f.rec.record("settingsRepo.SoftDeleteTenant")
	f.mu.Lock()
	f.softDeleteTenantCalls++
	f.mu.Unlock()
	return f.softDeleteTenantErr
}

var _ port.SettingsRepository = (*fakeSettingsRepository)(nil)

// ── fakeMembershipCheckClient ────────────────────────────────────────────

type membershipResult struct {
	active bool
	id     uuid.UUID
	err    error
}

type fakeMembershipCheckClient struct {
	rec *callRecorder

	mu      sync.Mutex
	results map[uuid.UUID]membershipResult
	calls   []uuid.UUID
}

func newFakeMembershipCheckClient(rec *callRecorder) *fakeMembershipCheckClient {
	return &fakeMembershipCheckClient{rec: rec, results: map[uuid.UUID]membershipResult{}}
}

func (f *fakeMembershipCheckClient) Exists(ctx context.Context, tenantID, userID uuid.UUID) (bool, uuid.UUID, error) {
	f.mu.Lock()
	f.calls = append(f.calls, userID)
	res, ok := f.results[userID]
	f.mu.Unlock()
	f.rec.record("membership.Exists:" + userID.String())
	if !ok {
		// Safe default: an unconfigured user is reported inactive so a
		// forgotten setup fails loudly (invalid_delegate) rather than
		// silently succeeding.
		return false, uuid.Nil, nil
	}
	return res.active, res.id, res.err
}

func (f *fakeMembershipCheckClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

var _ port.MembershipCheckClient = (*fakeMembershipCheckClient)(nil)

// ── fakeUserProfileClient ────────────────────────────────────────────────

type fakeUserProfileClient struct {
	rec *callRecorder

	mu                sync.Mutex
	calls             []port.SetAvailabilityRequest
	clearCalls        []uuid.UUID // userIDs passed to ClearDelegatePointer
	fn                func(ctx context.Context, req port.SetAvailabilityRequest) error
	clearFn           func(ctx context.Context, tenantID, userID uuid.UUID) error
	getAvailabilityFn func(ctx context.Context, tenantID, userID uuid.UUID) (*port.AvailabilitySnapshot, error)
}

func (f *fakeUserProfileClient) GetAvailability(ctx context.Context, tenantID, userID uuid.UUID) (*port.AvailabilitySnapshot, error) {
	f.rec.record("userprofile.GetAvailability")
	if f.getAvailabilityFn != nil {
		return f.getAvailabilityFn(ctx, tenantID, userID)
	}
	return &port.AvailabilitySnapshot{Status: "available"}, nil
}

func (f *fakeUserProfileClient) SetAvailability(ctx context.Context, req port.SetAvailabilityRequest) error {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()
	f.rec.record("userprofile.SetAvailability")
	if f.fn != nil {
		return f.fn(ctx, req)
	}
	return nil
}

func (f *fakeUserProfileClient) ClearDelegatePointer(ctx context.Context, tenantID, userID uuid.UUID) error {
	f.mu.Lock()
	f.clearCalls = append(f.clearCalls, userID)
	f.mu.Unlock()
	f.rec.record("userprofile.ClearDelegatePointer")
	if f.clearFn != nil {
		return f.clearFn(ctx, tenantID, userID)
	}
	return nil
}

func (f *fakeUserProfileClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

var _ port.UserProfileClient = (*fakeUserProfileClient)(nil)

// ── fakeIdempotencyStore ─────────────────────────────────────────────────

type fakeIdempotencyStore struct {
	mu sync.Mutex

	store        map[string]port.IdempotencyRecord
	getCalls     int
	saveCalls    int
	reserveCalls int
	releaseCalls int
	getErr       error
	saveErr      error
	reserveErr   error
}

func idemKey(tenantID uuid.UUID, key string) string {
	return tenantID.String() + "|" + key
}

func (f *fakeIdempotencyStore) Get(ctx context.Context, tenantID uuid.UUID, key string) (port.IdempotencyRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	if f.getErr != nil {
		return port.IdempotencyRecord{}, false, f.getErr
	}
	rec, ok := f.store[idemKey(tenantID, key)]
	return rec, ok, nil
}

func (f *fakeIdempotencyStore) Save(ctx context.Context, tenantID uuid.UUID, key string, rec port.IdempotencyRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saveCalls++
	if f.saveErr != nil {
		return f.saveErr
	}
	if f.store == nil {
		f.store = map[string]port.IdempotencyRecord{}
	}
	f.store[idemKey(tenantID, key)] = rec
	return nil
}

func (f *fakeIdempotencyStore) Reserve(ctx context.Context, tenantID uuid.UUID, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reserveCalls++
	if f.reserveErr != nil {
		return false, f.reserveErr
	}
	k := idemKey(tenantID, key)
	if _, exists := f.store[k]; exists {
		return false, nil
	}
	if f.store == nil {
		f.store = map[string]port.IdempotencyRecord{}
	}
	f.store[k] = port.IdempotencyRecord{Status: "pending"}
	return true, nil
}

func (f *fakeIdempotencyStore) Release(ctx context.Context, tenantID uuid.UUID, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseCalls++
	delete(f.store, idemKey(tenantID, key))
	return nil
}

var _ port.IdempotencyStore = (*fakeIdempotencyStore)(nil)

// ── fakeCache ────────────────────────────────────────────────────────────

type fakeCache struct {
	mu              sync.Mutex
	lists           map[string][]domain.Delegation
	invalidateCalls int
}

func cacheKey(tenantID, delegatorID uuid.UUID) string {
	return tenantID.String() + "|" + delegatorID.String()
}

func (c *fakeCache) GetDelegatorList(ctx context.Context, tenantID, delegatorID uuid.UUID) ([]domain.Delegation, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	list, ok := c.lists[cacheKey(tenantID, delegatorID)]
	return list, ok
}

func (c *fakeCache) SetDelegatorList(ctx context.Context, tenantID, delegatorID uuid.UUID, list []domain.Delegation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lists == nil {
		c.lists = map[string][]domain.Delegation{}
	}
	c.lists[cacheKey(tenantID, delegatorID)] = list
}

func (c *fakeCache) InvalidateDelegatorList(ctx context.Context, tenantID, delegatorID uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.invalidateCalls++
}

var _ port.Cache = (*fakeCache)(nil)

// ── fakeEventPublisher / fakeTxRunner ────────────────────────────────────

// fakeEventPublisher records every event enqueued through it. Bound into
// ctx by fakeTxRunner.RunInTx via port.WithEventPublisher, mirroring how the
// real postgres TxRunner binds a tx-scoped outbox publisher.
type fakeEventPublisher struct {
	mu     sync.Mutex
	events []*domain.DomainEvent
	err    error
}

func (p *fakeEventPublisher) EnqueueCtx(ctx context.Context, evt *domain.DomainEvent) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.events = append(p.events, evt)
	return nil
}

func (p *fakeEventPublisher) snapshot() []*domain.DomainEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*domain.DomainEvent, len(p.events))
	copy(out, p.events)
	return out
}

var _ port.EventPublisher = (*fakeEventPublisher)(nil)

// fakeTxRunner just calls fn(ctx) directly — no real transaction — after
// optionally binding a fake EventPublisher into ctx so service code's
// enqueue() calls are observable.
type fakeTxRunner struct {
	rec       *callRecorder
	publisher port.EventPublisher
}

func (t *fakeTxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	t.rec.record("txRunner.RunInTx")
	if t.publisher != nil {
		ctx = port.WithEventPublisher(ctx, t.publisher)
	}
	return fn(ctx)
}

var _ port.TxRunner = (*fakeTxRunner)(nil)

// ── fakeMetrics ──────────────────────────────────────────────────────────

type fakeMetrics struct {
	mu                     sync.Mutex
	created                []string
	ended                  []string
	idempotencyHits        int
	upAvailabilityFailures []string
}

func (m *fakeMetrics) RecordCreated(scope string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.created = append(m.created, scope)
}

func (m *fakeMetrics) RecordEnded(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ended = append(m.ended, reason)
}

func (m *fakeMetrics) RecordIdempotencyHit() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.idempotencyHits++
}

func (m *fakeMetrics) RecordUPAvailabilityFailure(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.upAvailabilityFailures = append(m.upAvailabilityFailures, path)
}

var _ Metrics = (*fakeMetrics)(nil)
