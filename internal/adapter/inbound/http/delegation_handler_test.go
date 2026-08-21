package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/pkg/requestctx"
)

func selfRC(userID, tenantID uuid.UUID) *requestctx.Context {
	return &requestctx.Context{UserID: userID.String(), TenantID: tenantID.String()}
}

func adminRC(userID, tenantID uuid.UUID) *requestctx.Context {
	return &requestctx.Context{UserID: userID.String(), TenantID: tenantID.String(), Roles: []string{"tenant_admin"}}
}

// ── List (DLG-1) ────────────────────────────────────────────────────────

func TestDelegationHandler_List(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()

	t.Run("missing identity -> 401", func(t *testing.T) {
		h := NewDelegationHandler(&fakeDelegationService{}, &fakeDelegationReader{})
		c, w := newRequestWithIdentity(http.MethodGet, "/api/v1/delegations", nil, nil)
		h.List(c)
		require.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("invalid tenant id -> 401", func(t *testing.T) {
		h := NewDelegationHandler(&fakeDelegationService{}, &fakeDelegationReader{})
		rc := &requestctx.Context{UserID: userID.String(), TenantID: "not-a-uuid"}
		c, w := newRequestWithIdentity(http.MethodGet, "/api/v1/delegations", nil, rc)
		h.List(c)
		require.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("invalid user id -> 401", func(t *testing.T) {
		h := NewDelegationHandler(&fakeDelegationService{}, &fakeDelegationReader{})
		rc := &requestctx.Context{UserID: "not-a-uuid", TenantID: tenantID.String()}
		c, w := newRequestWithIdentity(http.MethodGet, "/api/v1/delegations", nil, rc)
		h.List(c)
		require.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("service error mapped", func(t *testing.T) {
		svc := &fakeDelegationService{
			listFn: func(ctx context.Context, tenantID, delegatorID uuid.UUID) ([]domain.Delegation, error) {
				return nil, domain.NewError(domain.ErrDelegationNotFound, "gone")
			},
		}
		h := NewDelegationHandler(svc, &fakeDelegationReader{})
		c, w := newRequestWithIdentity(http.MethodGet, "/api/v1/delegations", nil, selfRC(userID, tenantID))
		h.List(c)
		require.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("happy path", func(t *testing.T) {
		d := domain.Delegation{ID: uuid.New(), TenantID: tenantID, DelegatorID: userID, DelegateID: uuid.New(),
			Scope: domain.ScopeAll, Status: domain.DelegationActive, StartsAt: time.Now(), RecordVersion: 1}
		svc := &fakeDelegationService{
			listFn: func(ctx context.Context, gotTenant, gotDelegator uuid.UUID) ([]domain.Delegation, error) {
				require.Equal(t, tenantID, gotTenant)
				require.Equal(t, userID, gotDelegator)
				return []domain.Delegation{d}, nil
			},
		}
		h := NewDelegationHandler(svc, &fakeDelegationReader{})
		c, w := newRequestWithIdentity(http.MethodGet, "/api/v1/delegations", nil, selfRC(userID, tenantID))
		h.List(c)
		require.Equal(t, http.StatusOK, w.Code)
		var resp DelegationListResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		require.Len(t, resp.Items, 1)
		require.Equal(t, d.ID, resp.Items[0].DelegationID)
		require.Equal(t, "all", resp.Items[0].Scope)
	})
}

// ── Create (DLG-2) ──────────────────────────────────────────────────────

func TestDelegationHandler_Create(t *testing.T) {
	tenantID, userID, delegateID := uuid.New(), uuid.New(), uuid.New()

	t.Run("missing identity -> 401", func(t *testing.T) {
		h := NewDelegationHandler(&fakeDelegationService{}, &fakeDelegationReader{})
		c, w := newRequestWithIdentity(http.MethodPost, "/api/v1/delegations", bytes.NewReader([]byte(`{}`)), nil)
		h.Create(c)
		require.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("malformed JSON body -> 400", func(t *testing.T) {
		h := NewDelegationHandler(&fakeDelegationService{}, &fakeDelegationReader{})
		c, w := newRequestWithIdentity(http.MethodPost, "/api/v1/delegations", bytes.NewReader([]byte(`{not json`)), selfRC(userID, tenantID))
		h.Create(c)
		require.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("domain error from service mapped", func(t *testing.T) {
		svc := &fakeDelegationService{
			createFn: func(ctx context.Context, tenantID, delegatorID uuid.UUID, idempotencyKey string, req service.CreateInput) (*domain.Delegation, error) {
				return nil, domain.NewError(domain.ErrSelfDelegation, "no")
			},
		}
		h := NewDelegationHandler(svc, &fakeDelegationReader{})
		body := DelegationCreateRequest{DelegateID: delegateID, Scope: "all"}
		raw, _ := json.Marshal(body)
		c, w := newRequestWithIdentity(http.MethodPost, "/api/v1/delegations", bytes.NewReader(raw), selfRC(userID, tenantID))
		h.Create(c)
		require.Equal(t, http.StatusUnprocessableEntity, w.Code)
	})

	t.Run("happy path -> 201 with exact shape, idempotency key forwarded", func(t *testing.T) {
		now := time.Now().UTC()
		created := domain.Delegation{
			ID: uuid.New(), TenantID: tenantID, DelegatorID: userID, DelegateID: delegateID,
			Scope: domain.ScopeAll, Status: domain.DelegationActive, StartsAt: now, RecordVersion: 1,
		}
		var gotKey string
		var gotReq service.CreateInput
		svc := &fakeDelegationService{
			createFn: func(ctx context.Context, gotTenant, gotDelegator uuid.UUID, idempotencyKey string, req service.CreateInput) (*domain.Delegation, error) {
				require.Equal(t, tenantID, gotTenant)
				require.Equal(t, userID, gotDelegator)
				gotKey = idempotencyKey
				gotReq = req
				return &created, nil
			},
		}
		h := NewDelegationHandler(svc, &fakeDelegationReader{})
		body := DelegationCreateRequest{DelegateID: delegateID, Scope: "all", OOONote: "on leave"}
		raw, _ := json.Marshal(body)
		c, w := newRequestWithIdentity(http.MethodPost, "/api/v1/delegations", bytes.NewReader(raw), selfRC(userID, tenantID))
		c.Request.Header.Set("Idempotency-Key", "abc-123")
		h.Create(c)
		require.Equal(t, http.StatusCreated, w.Code)
		require.Equal(t, "abc-123", gotKey)
		require.Equal(t, delegateID, gotReq.DelegateID)
		require.Equal(t, "on leave", gotReq.Reason)

		var resp DelegationResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		require.Equal(t, created.ID, resp.DelegationID)
		require.Equal(t, created.DelegateID, resp.DelegateID)
		require.Equal(t, "active", resp.Status)
		require.Equal(t, int64(1), resp.RecordVersion)
	})
}

// ── Cancel (DLG-3) ──────────────────────────────────────────────────────

func TestDelegationHandler_Cancel(t *testing.T) {
	tenantID, delegatorID, otherID := uuid.New(), uuid.New(), uuid.New()
	delegationID := uuid.New()

	t.Run("invalid id param -> 400", func(t *testing.T) {
		h := NewDelegationHandler(&fakeDelegationService{}, &fakeDelegationReader{})
		c, w := newRequestWithIdentity(http.MethodDelete, "/api/v1/delegations/nope", nil, selfRC(delegatorID, tenantID))
		setPathParam(c, "id", "nope")
		h.Cancel(c)
		require.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("missing identity -> 401", func(t *testing.T) {
		h := NewDelegationHandler(&fakeDelegationService{}, &fakeDelegationReader{})
		c, w := newRequestWithIdentity(http.MethodDelete, "/api/v1/delegations/"+delegationID.String(), nil, nil)
		setPathParam(c, "id", delegationID.String())
		h.Cancel(c)
		require.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("reader error mapped", func(t *testing.T) {
		reader := &fakeDelegationReader{
			findByIDFn: func(ctx context.Context, tenantID, id uuid.UUID) (*domain.Delegation, error) {
				return nil, domain.NewError(domain.ErrDelegationNotFound, "gone")
			},
		}
		h := NewDelegationHandler(&fakeDelegationService{}, reader)
		c, w := newRequestWithIdentity(http.MethodDelete, "/api/v1/delegations/"+delegationID.String(), nil, selfRC(delegatorID, tenantID))
		setPathParam(c, "id", delegationID.String())
		h.Cancel(c)
		require.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("neither self nor admin -> 403", func(t *testing.T) {
		reader := &fakeDelegationReader{
			findByIDFn: func(ctx context.Context, tenantID, id uuid.UUID) (*domain.Delegation, error) {
				return &domain.Delegation{ID: id, TenantID: tenantID, DelegatorID: delegatorID}, nil
			},
		}
		h := NewDelegationHandler(&fakeDelegationService{}, reader)
		c, w := newRequestWithIdentity(http.MethodDelete, "/api/v1/delegations/"+delegationID.String(), nil, selfRC(otherID, tenantID))
		setPathParam(c, "id", delegationID.String())
		h.Cancel(c)
		require.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("self can cancel own delegation", func(t *testing.T) {
		reader := &fakeDelegationReader{
			findByIDFn: func(ctx context.Context, tenantID, id uuid.UUID) (*domain.Delegation, error) {
				return &domain.Delegation{ID: id, TenantID: tenantID, DelegatorID: delegatorID}, nil
			},
		}
		var gotVersion int64
		svc := &fakeDelegationService{
			cancelFn: func(ctx context.Context, tenantID, id uuid.UUID, expectedVersion int64) (*domain.Delegation, error) {
				gotVersion = expectedVersion
				return &domain.Delegation{ID: id, TenantID: tenantID, DelegatorID: delegatorID, Status: domain.DelegationCancelled, RecordVersion: expectedVersion + 1}, nil
			},
		}
		h := NewDelegationHandler(svc, reader)
		c, w := newRequestWithIdentity(http.MethodDelete, "/api/v1/delegations/"+delegationID.String()+"?record_version=5", nil, selfRC(delegatorID, tenantID))
		setPathParam(c, "id", delegationID.String())
		h.Cancel(c)
		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, int64(5), gotVersion)
		var resp DelegationResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		require.Equal(t, "cancelled", resp.Status)
	})

	t.Run("admin can cancel someone else's delegation", func(t *testing.T) {
		reader := &fakeDelegationReader{
			findByIDFn: func(ctx context.Context, tenantID, id uuid.UUID) (*domain.Delegation, error) {
				return &domain.Delegation{ID: id, TenantID: tenantID, DelegatorID: delegatorID}, nil
			},
		}
		svc := &fakeDelegationService{}
		h := NewDelegationHandler(svc, reader)
		c, w := newRequestWithIdentity(http.MethodDelete, "/api/v1/delegations/"+delegationID.String(), nil, adminRC(otherID, tenantID))
		setPathParam(c, "id", delegationID.String())
		h.Cancel(c)
		require.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("service error mapped (optimistic lock)", func(t *testing.T) {
		reader := &fakeDelegationReader{
			findByIDFn: func(ctx context.Context, tenantID, id uuid.UUID) (*domain.Delegation, error) {
				return &domain.Delegation{ID: id, TenantID: tenantID, DelegatorID: delegatorID}, nil
			},
		}
		svc := &fakeDelegationService{
			cancelFn: func(ctx context.Context, tenantID, id uuid.UUID, expectedVersion int64) (*domain.Delegation, error) {
				return nil, domain.NewError(domain.ErrOptimisticLockConflict, "stale")
			},
		}
		h := NewDelegationHandler(svc, reader)
		c, w := newRequestWithIdentity(http.MethodDelete, "/api/v1/delegations/"+delegationID.String(), nil, selfRC(delegatorID, tenantID))
		setPathParam(c, "id", delegationID.String())
		h.Cancel(c)
		require.Equal(t, http.StatusConflict, w.Code)
	})
}

// ── Extend (DLG-4) ──────────────────────────────────────────────────────

func TestDelegationHandler_Extend(t *testing.T) {
	tenantID, delegatorID, otherID := uuid.New(), uuid.New(), uuid.New()
	delegationID := uuid.New()

	readerFor := func(delegator uuid.UUID) *fakeDelegationReader {
		return &fakeDelegationReader{
			findByIDFn: func(ctx context.Context, tenantID, id uuid.UUID) (*domain.Delegation, error) {
				return &domain.Delegation{ID: id, TenantID: tenantID, DelegatorID: delegator}, nil
			},
		}
	}

	t.Run("invalid id param -> 400", func(t *testing.T) {
		h := NewDelegationHandler(&fakeDelegationService{}, &fakeDelegationReader{})
		c, w := newRequestWithIdentity(http.MethodPost, "/api/v1/delegations/nope/extend", bytes.NewReader([]byte(`{}`)), selfRC(delegatorID, tenantID))
		setPathParam(c, "id", "nope")
		h.Extend(c)
		require.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("neither self nor admin -> 403", func(t *testing.T) {
		h := NewDelegationHandler(&fakeDelegationService{}, readerFor(delegatorID))
		c, w := newRequestWithIdentity(http.MethodPost, "/api/v1/delegations/"+delegationID.String()+"/extend", bytes.NewReader([]byte(`{}`)), selfRC(otherID, tenantID))
		setPathParam(c, "id", delegationID.String())
		h.Extend(c)
		require.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("malformed JSON body -> 400", func(t *testing.T) {
		h := NewDelegationHandler(&fakeDelegationService{}, readerFor(delegatorID))
		c, w := newRequestWithIdentity(http.MethodPost, "/api/v1/delegations/"+delegationID.String()+"/extend", bytes.NewReader([]byte(`{bad`)), selfRC(delegatorID, tenantID))
		setPathParam(c, "id", delegationID.String())
		h.Extend(c)
		require.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("happy path -> 200 narrow response shape", func(t *testing.T) {
		due := time.Now().Add(90 * 24 * time.Hour)
		var gotDays *int
		var gotVersion int64
		svc := &fakeDelegationService{
			extendFn: func(ctx context.Context, tenantID, id uuid.UUID, extendDays *int, expectedVersion int64) (*domain.Delegation, error) {
				gotDays = extendDays
				gotVersion = expectedVersion
				return &domain.Delegation{ID: id, TenantID: tenantID, DelegatorID: delegatorID, ReviewDueAt: &due}, nil
			},
		}
		h := NewDelegationHandler(svc, readerFor(delegatorID))
		days := 30
		reqBody, _ := json.Marshal(DelegationExtendRequest{ExtendDays: &days, RecordVersion: 7})
		c, w := newRequestWithIdentity(http.MethodPost, "/api/v1/delegations/"+delegationID.String()+"/extend", bytes.NewReader(reqBody), selfRC(delegatorID, tenantID))
		setPathParam(c, "id", delegationID.String())
		h.Extend(c)
		require.Equal(t, http.StatusOK, w.Code)
		require.NotNil(t, gotDays)
		require.Equal(t, 30, *gotDays)
		require.Equal(t, int64(7), gotVersion)

		var resp DelegationExtendResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		require.Equal(t, delegationID, resp.DelegationID)
		require.NotNil(t, resp.ReviewDueAt)
	})

	t.Run("service error mapped (not_review_tracked)", func(t *testing.T) {
		svc := &fakeDelegationService{
			extendFn: func(ctx context.Context, tenantID, id uuid.UUID, extendDays *int, expectedVersion int64) (*domain.Delegation, error) {
				return nil, domain.NewError(domain.ErrNotReviewTracked, "fixed end")
			},
		}
		h := NewDelegationHandler(svc, readerFor(delegatorID))
		reqBody, _ := json.Marshal(DelegationExtendRequest{})
		c, w := newRequestWithIdentity(http.MethodPost, "/api/v1/delegations/"+delegationID.String()+"/extend", bytes.NewReader(reqBody), selfRC(delegatorID, tenantID))
		setPathParam(c, "id", delegationID.String())
		h.Extend(c)
		require.Equal(t, http.StatusUnprocessableEntity, w.Code)
	})
}

// ── Reassign (DLG-5) ────────────────────────────────────────────────────

func TestDelegationHandler_Reassign(t *testing.T) {
	tenantID, delegatorID, otherID, newDelegateID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	delegationID := uuid.New()

	readerFor := func(delegator uuid.UUID) *fakeDelegationReader {
		return &fakeDelegationReader{
			findByIDFn: func(ctx context.Context, tenantID, id uuid.UUID) (*domain.Delegation, error) {
				return &domain.Delegation{ID: id, TenantID: tenantID, DelegatorID: delegator}, nil
			},
		}
	}

	t.Run("invalid id param -> 400", func(t *testing.T) {
		h := NewDelegationHandler(&fakeDelegationService{}, &fakeDelegationReader{})
		c, w := newRequestWithIdentity(http.MethodPost, "/api/v1/delegations/nope/reassign", bytes.NewReader([]byte(`{}`)), selfRC(delegatorID, tenantID))
		setPathParam(c, "id", "nope")
		h.Reassign(c)
		require.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("neither self nor admin -> 403", func(t *testing.T) {
		h := NewDelegationHandler(&fakeDelegationService{}, readerFor(delegatorID))
		c, w := newRequestWithIdentity(http.MethodPost, "/api/v1/delegations/"+delegationID.String()+"/reassign", bytes.NewReader([]byte(`{}`)), selfRC(otherID, tenantID))
		setPathParam(c, "id", delegationID.String())
		h.Reassign(c)
		require.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("malformed JSON body -> 400", func(t *testing.T) {
		h := NewDelegationHandler(&fakeDelegationService{}, readerFor(delegatorID))
		c, w := newRequestWithIdentity(http.MethodPost, "/api/v1/delegations/"+delegationID.String()+"/reassign", bytes.NewReader([]byte(`{not json`)), selfRC(delegatorID, tenantID))
		setPathParam(c, "id", delegationID.String())
		h.Reassign(c)
		require.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("service error mapped", func(t *testing.T) {
		svc := &fakeDelegationService{
			reassignFn: func(ctx context.Context, tenantID, id uuid.UUID, expectedVersion int64, req service.ReassignInput) (*domain.Delegation, error) {
				return nil, domain.NewError(domain.ErrDelegateUnavailable, "ooo")
			},
		}
		h := NewDelegationHandler(svc, readerFor(delegatorID))
		c, w := newRequestWithIdentity(http.MethodPost, "/api/v1/delegations/"+delegationID.String()+"/reassign", bytes.NewReader([]byte(`{}`)), selfRC(delegatorID, tenantID))
		setPathParam(c, "id", delegationID.String())
		h.Reassign(c)
		require.Equal(t, http.StatusUnprocessableEntity, w.Code)
	})

	t.Run("happy path -> 201", func(t *testing.T) {
		svc := &fakeDelegationService{
			reassignFn: func(ctx context.Context, tenantID, id uuid.UUID, expectedVersion int64, req service.ReassignInput) (*domain.Delegation, error) {
				return &domain.Delegation{ID: uuid.New(), TenantID: tenantID, DelegatorID: delegatorID, DelegateID: *req.NewDelegateID, Status: domain.DelegationActive, RecordVersion: 1}, nil
			},
		}
		h := NewDelegationHandler(svc, readerFor(delegatorID))
		body, _ := json.Marshal(DelegationReassignRequest{NewDelegateID: &newDelegateID, RecordVersion: 3})
		c, w := newRequestWithIdentity(http.MethodPost, "/api/v1/delegations/"+delegationID.String()+"/reassign", bytes.NewReader(body), selfRC(delegatorID, tenantID))
		setPathParam(c, "id", delegationID.String())
		h.Reassign(c)
		require.Equal(t, http.StatusCreated, w.Code)
		var resp DelegationResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		require.Equal(t, newDelegateID, resp.DelegateID)
	})

	// ── raw-JSON presence detection (LLD §8.4 DLG-5) ──────────────────────

	t.Run("ends_at explicit null -> EndsAtProvided true, EndsAt nil", func(t *testing.T) {
		var captured service.ReassignInput
		svc := &fakeDelegationService{
			reassignFn: func(ctx context.Context, tenantID, id uuid.UUID, expectedVersion int64, req service.ReassignInput) (*domain.Delegation, error) {
				captured = req
				return &domain.Delegation{}, nil
			},
		}
		h := NewDelegationHandler(svc, readerFor(delegatorID))
		c, w := newRequestWithIdentity(http.MethodPost, "/api/v1/delegations/"+delegationID.String()+"/reassign",
			bytes.NewReader([]byte(`{"ends_at": null}`)), selfRC(delegatorID, tenantID))
		setPathParam(c, "id", delegationID.String())
		h.Reassign(c)
		require.Equal(t, http.StatusCreated, w.Code)
		require.True(t, captured.EndsAtProvided)
		require.Nil(t, captured.EndsAt)
	})

	t.Run("ends_at present with value -> EndsAtProvided true, EndsAt set", func(t *testing.T) {
		var captured service.ReassignInput
		svc := &fakeDelegationService{
			reassignFn: func(ctx context.Context, tenantID, id uuid.UUID, expectedVersion int64, req service.ReassignInput) (*domain.Delegation, error) {
				captured = req
				return &domain.Delegation{}, nil
			},
		}
		h := NewDelegationHandler(svc, readerFor(delegatorID))
		c, w := newRequestWithIdentity(http.MethodPost, "/api/v1/delegations/"+delegationID.String()+"/reassign",
			bytes.NewReader([]byte(`{"ends_at": "2026-01-01T00:00:00Z"}`)), selfRC(delegatorID, tenantID))
		setPathParam(c, "id", delegationID.String())
		h.Reassign(c)
		require.Equal(t, http.StatusCreated, w.Code)
		require.True(t, captured.EndsAtProvided)
		require.NotNil(t, captured.EndsAt)
		want, _ := time.Parse(time.RFC3339, "2026-01-01T00:00:00Z")
		require.True(t, want.Equal(*captured.EndsAt))
	})

	t.Run("ends_at omitted entirely -> EndsAtProvided false", func(t *testing.T) {
		var captured service.ReassignInput
		svc := &fakeDelegationService{
			reassignFn: func(ctx context.Context, tenantID, id uuid.UUID, expectedVersion int64, req service.ReassignInput) (*domain.Delegation, error) {
				captured = req
				return &domain.Delegation{}, nil
			},
		}
		h := NewDelegationHandler(svc, readerFor(delegatorID))
		c, w := newRequestWithIdentity(http.MethodPost, "/api/v1/delegations/"+delegationID.String()+"/reassign",
			bytes.NewReader([]byte(`{"reason": "handoff"}`)), selfRC(delegatorID, tenantID))
		setPathParam(c, "id", delegationID.String())
		h.Reassign(c)
		require.Equal(t, http.StatusCreated, w.Code)
		require.False(t, captured.EndsAtProvided)
		require.Nil(t, captured.EndsAt)
	})

	t.Run("scope_id present and null -> ScopeIDSet true, ScopeID nil", func(t *testing.T) {
		var captured service.ReassignInput
		svc := &fakeDelegationService{
			reassignFn: func(ctx context.Context, tenantID, id uuid.UUID, expectedVersion int64, req service.ReassignInput) (*domain.Delegation, error) {
				captured = req
				return &domain.Delegation{}, nil
			},
		}
		h := NewDelegationHandler(svc, readerFor(delegatorID))
		c, w := newRequestWithIdentity(http.MethodPost, "/api/v1/delegations/"+delegationID.String()+"/reassign",
			bytes.NewReader([]byte(`{"scope_id": null}`)), selfRC(delegatorID, tenantID))
		setPathParam(c, "id", delegationID.String())
		h.Reassign(c)
		require.Equal(t, http.StatusCreated, w.Code)
		require.True(t, captured.ScopeIDSet)
		require.Nil(t, captured.ScopeID)
	})

	t.Run("scope_id present with value -> ScopeIDSet true, ScopeID set", func(t *testing.T) {
		scopeID := uuid.New()
		var captured service.ReassignInput
		svc := &fakeDelegationService{
			reassignFn: func(ctx context.Context, tenantID, id uuid.UUID, expectedVersion int64, req service.ReassignInput) (*domain.Delegation, error) {
				captured = req
				return &domain.Delegation{}, nil
			},
		}
		h := NewDelegationHandler(svc, readerFor(delegatorID))
		body, _ := json.Marshal(map[string]any{"scope_id": scopeID.String()})
		c, w := newRequestWithIdentity(http.MethodPost, "/api/v1/delegations/"+delegationID.String()+"/reassign",
			bytes.NewReader(body), selfRC(delegatorID, tenantID))
		setPathParam(c, "id", delegationID.String())
		h.Reassign(c)
		require.Equal(t, http.StatusCreated, w.Code)
		require.True(t, captured.ScopeIDSet)
		require.NotNil(t, captured.ScopeID)
		require.Equal(t, scopeID, *captured.ScopeID)
	})

	t.Run("scope_id omitted entirely -> ScopeIDSet false", func(t *testing.T) {
		var captured service.ReassignInput
		svc := &fakeDelegationService{
			reassignFn: func(ctx context.Context, tenantID, id uuid.UUID, expectedVersion int64, req service.ReassignInput) (*domain.Delegation, error) {
				captured = req
				return &domain.Delegation{}, nil
			},
		}
		h := NewDelegationHandler(svc, readerFor(delegatorID))
		c, w := newRequestWithIdentity(http.MethodPost, "/api/v1/delegations/"+delegationID.String()+"/reassign",
			bytes.NewReader([]byte(`{"reason": "handoff"}`)), selfRC(delegatorID, tenantID))
		setPathParam(c, "id", delegationID.String())
		h.Reassign(c)
		require.Equal(t, http.StatusCreated, w.Code)
		require.False(t, captured.ScopeIDSet)
		require.Nil(t, captured.ScopeID)
	})
}
