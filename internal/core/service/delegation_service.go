// Package service holds the Delegation Service's use-case orchestration.
// Lifted near-verbatim from iam-org-membership's DelegationService (LLD §6
// "History"), adapted per ADR-0008 v2 / LLD v2.1: two concurrent Core
// membership checks replace the local composite-FK lookups (§7.6.2), the
// tenant policy read is local (delegation_tenant_settings, DLG-D2), create
// takes a mandatory Idempotency-Key (DLG-D8), and reassign takes the fuller
// body (DLG-D11).
package service

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/pkg/requestctx"
	"github.com/google/uuid"
)

// skewTolerance allows clients with minor clock drift to pass starts_at ==
// now() without being rejected (LLD §13.3).
const skewTolerance = 5 * time.Second

// maxFutureStart bounds starts_at to at most 1 year ahead (DEL-14).
const maxFutureStart = 365 * 24 * time.Hour

// DelegationService owns DLG-1..5 (LLD §8.4).
type DelegationService struct {
	delegations     port.DelegationRepository
	settings        port.SettingsRepository
	membershipCheck port.MembershipCheckClient
	userProfile     port.UserProfileClient
	idempotency     port.IdempotencyStore
	cache           port.Cache
	txRunner        port.TxRunner
	metrics         Metrics
}

// NewDelegationService builds a DelegationService.
func NewDelegationService(
	d port.DelegationRepository,
	settings port.SettingsRepository,
	membershipCheck port.MembershipCheckClient,
	up port.UserProfileClient,
	idem port.IdempotencyStore,
	cache port.Cache,
	txRunner port.TxRunner,
) *DelegationService {
	return &DelegationService{
		delegations: d, settings: settings, membershipCheck: membershipCheck,
		userProfile: up, idempotency: idem, cache: cache, txRunner: txRunner,
	}
}

// WithMetrics attaches an optional recorder. Nil is valid (no-op).
func (s *DelegationService) WithMetrics(m Metrics) *DelegationService {
	s.metrics = m
	return s
}

// List is DLG-1 — the caller's own active delegations (LLD §9.1), cache
// read-through, advisory (correctness never depends on the cache, §9.3).
func (s *DelegationService) List(ctx context.Context, tenantID, delegatorID uuid.UUID) ([]domain.Delegation, error) {
	if s.cache != nil {
		if cached, hit := s.cache.GetDelegatorList(ctx, tenantID, delegatorID); hit {
			return cached, nil
		}
	}
	list, err := s.delegations.ListByDelegator(ctx, tenantID, delegatorID)
	if err != nil {
		return nil, err
	}
	if s.cache != nil {
		s.cache.SetDelegatorList(ctx, tenantID, delegatorID, list)
	}
	return list, nil
}

// CreateInput is DLG-2's validated request shape. Reason is the wire
// `ooo_note` field (DEL-10): stored in delegations.reason (audit-only) and
// forwarded to User Profile as the display note.
type CreateInput struct {
	DelegateID uuid.UUID
	Scope      string
	ScopeID    *uuid.UUID
	Reason     string
	StartsAt   *time.Time
	EndsAt     *time.Time
}

// Create is DLG-2. Ordering (LLD §7.6.4/§11.1): idempotency check ->
// pre-flight -> two concurrent Core membership checks -> UP
// SetAvailability (wait for 200) -> RunInTx{Insert; outbox DelegationStarted}.
func (s *DelegationService) Create(ctx context.Context, tenantID, delegatorID uuid.UUID, idempotencyKey string, req CreateInput) (*domain.Delegation, error) {
	// DLG-D8/§9.2: a repeated Idempotency-Key within 24h returns the
	// original delegation rather than creating a second one.
	if idempotencyKey != "" && s.idempotency != nil {
		if rec, found, err := s.idempotency.Get(ctx, tenantID, idempotencyKey); err == nil && found {
			if existing, ferr := s.delegations.FindByID(ctx, tenantID, rec.DelegationID); ferr == nil {
				if s.metrics != nil {
					s.metrics.RecordIdempotencyHit()
				}
				return existing, nil
			}
		}
	}

	if err := validateCreateInput(delegatorID, req); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	starts := now
	if req.StartsAt != nil {
		starts = *req.StartsAt
	}
	if req.StartsAt != nil && starts.Before(now.Add(-skewTolerance)) {
		return nil, domain.NewError(domain.ErrDelegationStartInPast, "starts_at must not be in the past")
	}
	if req.StartsAt != nil && starts.After(now.Add(maxFutureStart)) {
		return nil, domain.NewError(domain.ErrDelegationStartTooFarFuture, "starts_at must be within 1 year from now")
	}
	if req.EndsAt != nil && !req.EndsAt.After(starts) {
		return nil, domain.NewError(domain.ErrDelegationWindowInverted, "ends_at must be after starts_at")
	}

	// DEL-14: bounds read in-process from delegation_tenant_settings — no
	// cross-service call (DLG-D2).
	settings, err := s.settings.Get(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if req.EndsAt != nil {
		maxSpan := time.Duration(settings.MaxDurationDays) * 24 * time.Hour
		if req.EndsAt.Sub(starts) > maxSpan {
			return nil, domain.NewError(domain.ErrDelegationWindowTooLong,
				"delegation span exceeds the tenant's max_duration_days").
				WithDetails(map[string]any{"max_duration_days": settings.MaxDurationDays})
		}
	}

	// §7.6.2/DLG-D3: two concurrent grant-time membership checks, replacing
	// the lost composite FKs. Fail closed on either check erroring.
	delegatorMembershipID, delegateMembershipID, err := s.checkBothMemberships(ctx, tenantID, delegatorID, req.DelegateID)
	if err != nil {
		return nil, err
	}

	// DLG-D25 (cross-service future-OOO bug fix): a delegation whose
	// starts_at is genuinely in the future must not call User Profile or
	// emit DelegationStarted yet — that would show the delegator as OOO,
	// and route work to the delegate, before the actual leave begins. Such
	// a delegation is created 'scheduled' instead of 'active'; the
	// delegation-activation reconciler job calls User Profile and flips it
	// to 'active' (emitting DelegationStarted then) once starts_at is
	// reached. starts is never before now by more than skewTolerance (see
	// the validation above), so this only defers a caller-requested future
	// starts_at — the default "no starts_at" (starts == now) case, and an
	// explicit starts_at within clock-skew tolerance of now, both still
	// activate immediately with no behavior change.
	isScheduled := starts.After(now)

	// reviewDueAt applies only to open-ended delegations (EndsAt == nil,
	// DEL-8) and is computed here — before the UP call below, not inside
	// RunInTx as previously — because it now doubles as the bound sent to
	// User Profile's OOO window (see the OOOUntil fallback immediately
	// below): User Profile's own model requires status=ooo to carry a
	// bounded ooo_until (ValidateOOOWindow, ≤180 days), but this service's
	// open-ended delegations have no ends_at at all. Without this, every
	// open-ended Create call was rejected by User Profile with 422 — a
	// confirmed cross-service contract bug, not a hypothetical: open-ended
	// delegations are DLG-4 Extend's and DLG-I2's entire reason for
	// existing, not an edge case. review_due_at is this service's own
	// tenant-configured "how long can this go unreviewed" bound
	// (delegation_tenant_settings.review_window_days, 1..180 — always
	// within User Profile's 180-day cap), so it is the natural proxy for
	// "open-ended" in a service that has no concept of open-ended at all.
	// Re-synced on every Extend (below) for the same reason — otherwise
	// User Profile's own expiry sweep resets the user to available once
	// this original bound passes, even though the delegation was extended.
	var reviewDueAt *time.Time
	if req.EndsAt == nil {
		due := starts.Add(time.Duration(settings.ReviewWindowDays) * 24 * time.Hour)
		reviewDueAt = &due
	}

	// DEL-6 availability-first: call UP BEFORE any write, unless deferred.
	// Clamp oooFrom to now() when starts_at is in the past relative to UP (a
	// past starts_at is allowed here — the delegation is immediately active).
	if !isScheduled {
		oooFrom := starts
		if oooFrom.Before(now) {
			oooFrom = now
		}
		oooUntil := req.EndsAt
		if oooUntil == nil {
			oooUntil = reviewDueAt
		}
		oooStatus := "ooo"
		if err := s.userProfile.SetAvailability(ctx, port.SetAvailabilityRequest{
			TenantID: tenantID, UserID: delegatorID,
			Status: &oooStatus, OOOFrom: &oooFrom, OOOUntil: oooUntil,
			DelegateID: &req.DelegateID, Note: req.Reason,
		}); err != nil {
			if s.metrics != nil {
				s.metrics.RecordUPAvailabilityFailure("create")
			}
			if errors.Is(err, port.ErrDependencyUnavailable) {
				return nil, domain.NewError(domain.ErrUserProfileUnavailable, "user profile unavailable")
			}
			if strings.Contains(err.Error(), "delegate_unavailable") {
				return nil, domain.NewError(domain.ErrDelegateUnavailable, "delegate is currently unavailable (OOO)")
			}
			return nil, domain.NewError(domain.ErrInvalidDelegate, "delegate validation failed via user profile")
		}
	}

	initialStatus := domain.DelegationActive
	if isScheduled {
		initialStatus = domain.DelegationScheduled
	}

	scope := domain.DelegationScope(req.Scope)
	var created *domain.Delegation
	err = s.txRunner.RunInTx(ctx, func(txCtx context.Context) error {
		// reviewDueAt was computed above, before the UP call — reused here
		// rather than recomputed.
		reviewWindowDays := settings.ReviewWindowDays
		out, err := s.delegations.Insert(txCtx, &domain.Delegation{
			TenantID:              tenantID,
			DelegatorID:           delegatorID,
			DelegateID:            req.DelegateID,
			DelegatorMembershipID: delegatorMembershipID,
			DelegateMembershipID:  delegateMembershipID,
			Scope:                 scope,
			ScopeID:               req.ScopeID,
			Reason:                req.Reason,
			StartsAt:              starts,
			EndsAt:                req.EndsAt,
			Status:                initialStatus,
			ReviewDueAt:           reviewDueAt,
			ReviewWindowDays:      &reviewWindowDays,
		})
		if err != nil {
			return err
		}
		created = out
		if isScheduled {
			// No event yet — delegation-activation enqueues DelegationStarted
			// when it actually activates this row at starts_at.
			return nil
		}
		return enqueue(txCtx, domain.EventDelegationStarted, tenantID, out.ID.String(), delegatorID.String(),
			domain.DelegationStartedPayload{
				DelegationID: out.ID, TenantID: tenantID,
				DelegatorID: delegatorID, DelegateID: req.DelegateID,
				Scope: scope, ScopeID: req.ScopeID,
				StartsAt: starts, EndsAt: req.EndsAt,
				ActorID: delegatorID,
			})
	})
	if err != nil {
		if !isScheduled {
			// best-effort compensating UP pointer clear — UP was already
			// updated before this tx; if tx failed, clear the stale OOO
			// pointer. Not needed when isScheduled: UP was never called.
			//nolint:errcheck // best-effort: failure is logged by UP client; must not mask the original tx error
			_ = s.userProfile.SetAvailability(ctx, port.SetAvailabilityRequest{
				TenantID: tenantID, UserID: delegatorID, ClearDelegate: true,
			})
		}
		return nil, err
	}

	if s.idempotency != nil && idempotencyKey != "" {
		//nolint:errcheck // best-effort by design (§9.2): a Save failure
		// degrades create-idempotency, it must never fail an already-
		// committed create.
		_ = s.idempotency.Save(ctx, tenantID, idempotencyKey, port.IdempotencyRecord{
			DelegationID: created.ID, Status: "created",
		})
	}
	if s.cache != nil {
		s.cache.InvalidateDelegatorList(ctx, tenantID, delegatorID)
	}
	if s.metrics != nil {
		s.metrics.RecordCreated(string(created.Scope))
	}
	return created, nil
}

func validateCreateInput(delegatorID uuid.UUID, req CreateInput) error {
	if delegatorID == req.DelegateID {
		return domain.NewError(domain.ErrSelfDelegation, "cannot delegate to yourself")
	}
	scope := domain.DelegationScope(req.Scope)
	if scope != domain.ScopeAll && scope != domain.ScopeDepartment && scope != domain.ScopeTender {
		return domain.NewError(domain.ErrInvalidDelegationScope, "invalid scope")
	}
	if scope != domain.ScopeAll && req.ScopeID == nil {
		return domain.NewError(domain.ErrScopeIDRequired, "scope_id is required for department/tender scope")
	}
	if scope == domain.ScopeAll && req.ScopeID != nil {
		return domain.NewError(domain.ErrInvalidScopeID, "scope_id must be omitted when scope is all")
	}
	if utf8.RuneCountInString(req.Reason) > 500 {
		return domain.NewError(domain.ErrReasonTooLong, "reason must not exceed 500 characters")
	}
	return nil
}

// checkBothMemberships issues both membership-existence checks concurrently
// (LLD §7.6.4: "no cross-service HTTP inside a transaction", and both
// checks are on the write path only, ≤50ms p99 each).
func (s *DelegationService) checkBothMemberships(ctx context.Context, tenantID, delegatorID, delegateID uuid.UUID) (delegatorMembershipID, delegateMembershipID uuid.UUID, err error) {
	type result struct {
		active bool
		id     uuid.UUID
		err    error
	}
	delegatorCh := make(chan result, 1)
	delegateCh := make(chan result, 1)
	go func() {
		active, id, err := s.membershipCheck.Exists(ctx, tenantID, delegatorID)
		delegatorCh <- result{active, id, err}
	}()
	go func() {
		active, id, err := s.membershipCheck.Exists(ctx, tenantID, delegateID)
		delegateCh <- result{active, id, err}
	}()
	dgt, dt := <-delegatorCh, <-delegateCh

	if dgt.err != nil || dt.err != nil {
		return uuid.Nil, uuid.Nil, domain.NewError(domain.ErrOrgMembershipUnavailable, "membership check unavailable")
	}
	if !dgt.active || !dt.active {
		return uuid.Nil, uuid.Nil, domain.NewError(domain.ErrInvalidDelegate, "delegator or delegate is not an active member")
	}
	return dgt.id, dt.id, nil
}

// cancelInternal is the shared end-path for Cancel (reason=cancelled) and
// Reassign (reason=reassigned). UP pointer-clear is always fail-open.
func (s *DelegationService) cancelInternal(ctx context.Context, tenantID, id uuid.UUID, expectedVersion int64, endReason domain.EndReason) (*domain.Delegation, error) {
	d, err := s.delegations.FindByID(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	// fail-open by design (LLD §11.2): if UP is down, log
	// and proceed — the expiry cron re-clears the pointer later (DEL-6).
	if err := s.userProfile.SetAvailability(ctx, port.SetAvailabilityRequest{
		TenantID: tenantID, UserID: d.DelegatorID, ClearDelegate: true,
	}); err != nil && s.metrics != nil {
		s.metrics.RecordUPAvailabilityFailure("cancel")
	}
	actorID := d.DelegatorID
	if rc, ok := requestctx.FromContext(ctx); ok {
		if parsed, err := uuid.Parse(rc.UserID); err == nil {
			actorID = parsed
		}
	}

	var ended *domain.Delegation
	err = s.txRunner.RunInTx(ctx, func(txCtx context.Context) error {
		out, err := s.delegations.End(txCtx, tenantID, id, domain.DelegationCancelled, expectedVersion)
		if err != nil {
			return err
		}
		ended = out
		return enqueue(txCtx, domain.EventDelegationEnded, tenantID, id.String(), d.DelegatorID.String(),
			domain.DelegationEndedPayload{
				DelegationID: id, TenantID: tenantID,
				DelegatorID: d.DelegatorID, DelegateID: d.DelegateID,
				Scope: d.Scope, ScopeID: d.ScopeID,
				EndedReason: endReason,
				ActorID:     actorID,
			})
	})
	if err != nil {
		return nil, err
	}
	if s.cache != nil {
		s.cache.InvalidateDelegatorList(ctx, tenantID, d.DelegatorID)
	}
	if s.metrics != nil {
		s.metrics.RecordEnded(string(domain.EndReasonCancelled))
	}
	return ended, nil
}

// Cancel is DLG-3. Pointer-clear-only, fail-open on UP (LLD §11.2): if UP
// is down, log and proceed — the expiry cron re-clears later (DEL-6).
func (s *DelegationService) Cancel(ctx context.Context, tenantID, id uuid.UUID, expectedVersion int64) (*domain.Delegation, error) {
	return s.cancelInternal(ctx, tenantID, id, expectedVersion, domain.EndReasonCancelled)
}

// Extend is DLG-4. Open-ended only; resets ReviewLastWarnedBucket to nil,
// re-arming both warnings (DLG-EVT-5). Priority for the window used:
// caller-supplied extendDays > per-delegation ReviewWindowDays override >
// tenant delegation_tenant_settings default.
func (s *DelegationService) Extend(ctx context.Context, tenantID, id uuid.UUID, extendDays *int, expectedVersion int64) (*domain.Delegation, error) {
	if extendDays != nil && (*extendDays < domain.PolicyDaysMin || *extendDays > domain.PolicyDaysMax) {
		return nil, domain.NewError(domain.ErrExtendDaysOutOfRange, "extend_days must be between 1 and 180")
	}
	d, err := s.delegations.FindByID(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if d.Status != domain.DelegationActive {
		return nil, domain.NewError(domain.ErrDelegationNotFound, "delegation not found")
	}
	if d.EndsAt != nil {
		return nil, domain.NewError(domain.ErrNotReviewTracked, "extend only applies to open-ended delegations")
	}
	settings, err := s.settings.Get(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	windowDays := settings.ReviewWindowDays
	if d.ReviewWindowDays != nil {
		windowDays = *d.ReviewWindowDays
	}
	if extendDays != nil {
		windowDays = *extendDays
	}
	out, err := s.delegations.ExtendReview(ctx, tenantID, id, windowDays, expectedVersion)
	if err != nil {
		return nil, err
	}
	if s.cache != nil {
		s.cache.InvalidateDelegatorList(ctx, tenantID, out.DelegatorID)
	}
	// Re-sync User Profile's ooo_until to the new review_due_at. Required
	// because Create (above) now sends review_due_at as the OOO window's
	// bound for open-ended delegations — User Profile has no concept of
	// "open-ended," only a bounded window it expires on its own schedule.
	// Without this, extending the delegation here would leave User
	// Profile's ooo_until at the OLD (soon-to-lapse) value; once that
	// original bound passed, User Profile's own expiry sweep would reset
	// the delegator to 'available' even though the delegation is still
	// active — reintroducing the same class of cross-service desync this
	// fix exists to close. Fail-open (best-effort), matching Cancel's
	// existing DEL-6 pattern: unlike the expiry-cron's stale-pointer case,
	// there is no later self-healing retry for this specific field if the
	// call fails here — accepted as a rare-outage residual risk rather
	// than blocking the user's own extend action on a transient UP hiccup.
	oooStatus := "ooo"
	if err := s.userProfile.SetAvailability(ctx, port.SetAvailabilityRequest{
		TenantID: tenantID, UserID: out.DelegatorID,
		Status: &oooStatus, OOOFrom: &out.StartsAt, OOOUntil: out.ReviewDueAt,
		DelegateID: &out.DelegateID, Note: out.Reason,
	}); err != nil && s.metrics != nil {
		s.metrics.RecordUPAvailabilityFailure("extend")
	}
	return out, nil
}

// ReassignInput is DLG-5's fuller body (DLG-D11) — every field optional,
// each defaulting to the current delegation's value. EndsAtProvided
// distinguishes "field omitted" (keep existing ends_at) from "field
// present, possibly null" (use EndsAt, nil meaning open-ended); the HTTP
// DTO layer sets it from raw-body key presence.
type ReassignInput struct {
	NewDelegateID  *uuid.UUID
	Scope          *string
	ScopeID        *uuid.UUID
	ScopeIDSet     bool
	EndsAt         *time.Time
	EndsAtProvided bool
	Reason         *string
}

// Reassign is DLG-5 — ends the current delegation then creates a new one
// (reuses Cancel + Create verbatim). On a Create failure after Cancel
// succeeds, the old delegation is NOT resurrected (DLG-D11): the caller
// sees the Create error and must issue a fresh request.
func (s *DelegationService) Reassign(ctx context.Context, tenantID, id uuid.UUID, expectedVersion int64, req ReassignInput) (*domain.Delegation, error) {
	existing, err := s.delegations.FindByID(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if existing.Status != domain.DelegationActive {
		return nil, domain.NewError(domain.ErrDelegationNotFound, "delegation not found")
	}
	if _, err := s.cancelInternal(ctx, tenantID, id, expectedVersion, domain.EndReasonReassigned); err != nil {
		return nil, err
	}

	newDelegateID := existing.DelegateID
	if req.NewDelegateID != nil {
		newDelegateID = *req.NewDelegateID
	}
	scope := string(existing.Scope)
	if req.Scope != nil {
		scope = *req.Scope
	}
	scopeID := existing.ScopeID
	if req.ScopeIDSet {
		scopeID = req.ScopeID
	}
	reason := existing.Reason
	if req.Reason != nil {
		reason = *req.Reason
	}
	// ends_at: null (explicitly provided) makes the new delegation
	// open-ended; omitting the key preserves the existing ends_at — same
	// rule as scope_id above (LLD §8.4 DLG-5).
	endsAt := existing.EndsAt
	if req.EndsAtProvided {
		endsAt = req.EndsAt
	}

	return s.Create(ctx, tenantID, existing.DelegatorID, "", CreateInput{
		DelegateID: newDelegateID,
		Scope:      scope,
		ScopeID:    scopeID,
		Reason:     reason,
		StartsAt:   nil, // starts now
		EndsAt:     endsAt,
	})
}

// enqueue is a small helper so every Create/Cancel/Extend/cron call site
// doesn't repeat the requestctx-lookup + EventPublisherFromContext dance.
func enqueue(ctx context.Context, eventType string, tenantID uuid.UUID, subject, actor string, data any) error {
	pub, ok := port.EventPublisherFromContext(ctx)
	if !ok || pub == nil {
		return nil
	}
	evt := &domain.DomainEvent{
		Type: eventType, TenantID: tenantID, Subject: subject, Actor: actor, Data: data,
		OccurredAt: time.Now().UTC(),
	}
	if rc, ok := requestctx.FromContext(ctx); ok {
		evt.IPAddress = rc.ClientIP
		evt.UserAgent = rc.UserAgent
	}
	return pub.EnqueueCtx(ctx, evt)
}
