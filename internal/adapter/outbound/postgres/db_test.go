package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	pgcommon "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
)

// ── WithTenantGUC ────────────────────────────────────────────────────────────

func TestWithTenantGUC_SetsTenantAndUserOnContext(t *testing.T) {
	tenantID := uuid.New()
	ctx := WithTenantGUC(context.Background(), tenantID, "user-abc")

	g, ok := pgcommon.GUCSetFromContext(ctx)
	require.True(t, ok)
	require.Equal(t, tenantID.String(), g.TenantID)
	require.Equal(t, "user-abc", g.UserID)
}

func TestWithTenantGUC_EmptyUserID_NotOverwritten(t *testing.T) {
	tenantID := uuid.New()
	ctx := WithTenantGUC(context.Background(), tenantID, "")

	g, ok := pgcommon.GUCSetFromContext(ctx)
	require.True(t, ok)
	require.Equal(t, tenantID.String(), g.TenantID)
	require.Empty(t, g.UserID)
}

// ── wrapConnErr ──────────────────────────────────────────────────────────────

func TestWrapConnErr_Nil(t *testing.T) {
	require.NoError(t, wrapConnErr(nil))
}

func TestWrapConnErr_DomainError_PassesThrough(t *testing.T) {
	de := domain.NewError(domain.ErrDelegationNotFound, "not found")
	got := wrapConnErr(de)
	require.Equal(t, de, got)
}

func TestWrapConnErr_PgError_PassesThrough(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "23505"}
	got := wrapConnErr(pgErr)
	require.ErrorAs(t, got, &pgErr)
}

func TestWrapConnErr_ErrNoRows_PassesThrough(t *testing.T) {
	got := wrapConnErr(pgx.ErrNoRows)
	require.ErrorIs(t, got, pgx.ErrNoRows)
}

func TestWrapConnErr_ContextCanceled_PassesThrough(t *testing.T) {
	got := wrapConnErr(context.Canceled)
	require.ErrorIs(t, got, context.Canceled)
}

func TestWrapConnErr_DeadlineExceeded_PassesThrough(t *testing.T) {
	got := wrapConnErr(context.DeadlineExceeded)
	require.ErrorIs(t, got, context.DeadlineExceeded)
}

func TestWrapConnErr_GenericErr_BecomesUnavailable(t *testing.T) {
	got := wrapConnErr(errors.New("connection refused: host unreachable"))
	require.ErrorIs(t, got, port.ErrDependencyUnavailable)
}

// ── EnqueueCtx error paths ───────────────────────────────────────────────────

// fakePayloadValidator implements PayloadValidator and returns a configurable error.
type fakePayloadValidator struct{ err error }

func (f *fakePayloadValidator) Validate(_ context.Context, _ string, _ json.RawMessage) error {
	return f.err
}

// TestEnqueueCtx_ValidatorError covers the validator-returns-error path in
// txBoundPublisher.EnqueueCtx (line 179 of db.go).
func TestEnqueueCtx_ValidatorError(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	tenantID := uuid.New()
	gucCtx := withTenant(ctx, tenantID)

	wantErr := errors.New("schema validation failed")
	runner := NewTxRunner(db.App, &fakePayloadValidator{err: wantErr})

	err := runner.RunInTx(gucCtx, func(txCtx context.Context) error {
		pub, ok := port.EventPublisherFromContext(txCtx)
		require.True(t, ok)
		return pub.EnqueueCtx(txCtx, &domain.DomainEvent{
			Type:     domain.EventDelegationStarted,
			TenantID: tenantID,
			Subject:  uuid.New().String(),
			Actor:    uuid.New().String(),
			Data:     domain.DelegationStartedPayload{DelegationID: uuid.New(), TenantID: tenantID},
		})
	})
	// The validator error is wrapped by wrapConnErr as ErrDependencyUnavailable since
	// it is not a domain.Error, pgconn.PgError, pgx.ErrNoRows, or ctx cancellation.
	require.Error(t, err)
	require.ErrorIs(t, err, port.ErrDependencyUnavailable)
	require.Contains(t, err.Error(), "schema validation failed")
}
