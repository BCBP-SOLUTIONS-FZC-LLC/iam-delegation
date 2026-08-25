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

	// DEL-6 availability-first: call UP BEFORE any write. Clamp oooFrom to
	// now() when starts_at is in the past relative to UP (a past starts_at
	// is allowed here — the delegation is immediately active).
	oooFrom := starts
	if oooFrom.Before(now) {
		oooFrom = now
	}
	oooStatus := "ooo"
	if err := s.userProfile.SetAvailability(ctx, port.SetAvailabilityRequest{
		TenantID: tenantID, UserID: delegatorID,
		Status: &oooStatus, OOOFrom: &oooFrom, OOOUntil: req.EndsAt,
		DelegateID: &req.DelegateID, Note: req.Reason,
	}); err != nil {
		if errors.Is(err, port.ErrDependencyUnavailable) {
			return nil, domain.NewError(domain.ErrUserProfileUnavailable, "user profile unavailable")
		}
		if strings.Contains(err.Error(), "delegate_unavailable") {
			return nil, domain.NewError(domain.ErrDelegateUnavailable, "delegate is currently unavailable (OOO)")
		}
		return nil, domain.NewError(domain.ErrInvalidDelegate, "delegate validation failed via user profile")
	}

	scope := domain.DelegationScope(req.Scope)
	var created *domain.Delegation
	err = s.txRunner.RunInTx(ctx, func(txCtx context.Context) error {
		var reviewDueAt *time.Time
		if req.EndsAt == nil {
			due := starts.Add(time.Duration(settings.ReviewWindowDays) * 24 * time.Hour)
			reviewDueAt = &due
		}
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
			ReviewDueAt:           reviewDueAt,
			ReviewWindowDays:      &reviewWindowDays,
		})
		if err != nil {
			return err
		}
		created = out
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
		// best-effort compensating UP pointer clear — UP was already updated
		// before this tx; if tx failed, clear the stale OOO pointer
		//nolint:errcheck // best-effort: failure is logged by UP client; must not mask the original tx error
		_ = s.userProfile.SetAvailability(ctx, port.SetAvailabilityRequest{
			TenantID: tenantID, UserID: delegatorID, ClearDelegate: true,
		})
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

// Cancel is DLG-3. Pointer-clear-only, fail-open on UP (LLD §11.2): if UP
// is down, log and proceed — the expiry cron re-clears later (DEL-6).
func (s *DelegationService) Cancel(ctx context.Context, tenantID, id uuid.UUID, expectedVersion int64) (*domain.Delegation, error) {
	d, err := s.delegations.FindByID(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	//nolint:errcheck // fail-open by design (LLD §11.2): if UP is down, log
	// and proceed — the expiry cron re-clears the pointer later (DEL-6).
	_ = s.userProfile.SetAvailability(ctx, port.SetAvailabilityRequest{
		TenantID: tenantID, UserID: d.DelegatorID, ClearDelegate: true,
	})
	// use actual caller if available in context, fall back to delegator
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
				EndedReason: domain.EndReasonCancelled,
				ActorID:     actorID,
			})
	})
	if err != nil {
		return nil, err
	}
	if s.cache != nil {
		s.cache.InvalidateDelegatorList(ctx, tenantID, d.DelegatorID)
	}
	return ended, nil
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
	if _, err := s.Cancel(ctx, tenantID, id, expectedVersion); err != nil {
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
	var endsAt *time.Time
	if req.EndsAtProvided {
		endsAt = req.EndsAt
	}
	// else: reassign always creates a fresh open-ended-by-default grant
	// unless the caller specifies ends_at (DLG-D11's "starts now" reset).

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
