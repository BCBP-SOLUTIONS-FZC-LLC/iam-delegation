package postgres

// errortx_test.go — minimal pgx.Tx / pgx.Rows / pgx.Row fakes for
// error-path coverage of repository methods whose error branches live
// inside withPool's fn callback. Inject via WithTx(ctx, &errXxx{}).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

// ── fake pgx.Rows ────────────────────────────────────────────────────────

// scanErrRows returns Next()=true once, then Scan returns err.
type scanErrRows struct {
	called bool
	err    error
}

func (r *scanErrRows) Close()                                       {}
func (r *scanErrRows) Err() error                                   { return nil }
func (r *scanErrRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *scanErrRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *scanErrRows) Next() bool {
	if !r.called {
		r.called = true
		return true
	}
	return false
}
func (r *scanErrRows) Scan(_ ...any) error    { return r.err }
func (r *scanErrRows) Values() ([]any, error) { return nil, nil }
func (r *scanErrRows) RawValues() [][]byte    { return nil }
func (r *scanErrRows) Conn() *pgx.Conn        { return nil }

var _ pgx.Rows = (*scanErrRows)(nil)

// ── fake pgx.Row ─────────────────────────────────────────────────────────

type scanErrRow struct{ err error }

func (r scanErrRow) Scan(_ ...any) error { return r.err }

var _ pgx.Row = scanErrRow{}

// ── fake pgx.Tx variants ─────────────────────────────────────────────────

// errExecTx: Exec always returns err.
type errExecTx struct {
	pgx.Tx
	err error
}

func (t *errExecTx) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, t.err
}

// errQueryTx: Query always returns err.
type errQueryTx struct {
	pgx.Tx
	err error
}

func (t *errQueryTx) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, t.err
}

// scanErrQueryTx: Query returns Rows that succeed on Next but fail on Scan.
type scanErrQueryTx struct {
	pgx.Tx
	err error
}

func (t *scanErrQueryTx) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return &scanErrRows{err: t.err}, nil
}

// errQueryRowTx: QueryRow returns a Row whose Scan returns err (not ErrNoRows).
type errQueryRowTx struct {
	pgx.Tx
	err error
}

func (t *errQueryRowTx) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return scanErrRow{err: t.err}
}

// ── tests ─────────────────────────────────────────────────────────────────

var errFake = errors.New("injected error")

// ── delegation_repository ────────────────────────────────────────────────

func TestDelegationRepository_listWhere_QueryError(t *testing.T) {
	db := setupTestDB(t)
	repo := NewDelegationRepository(db.App)
	tenantID := uuid.New()
	ctx := WithTx(withTenant(context.Background(), tenantID), &errQueryTx{err: errFake})
	_, err := repo.List(ctx, tenantID)
	require.ErrorIs(t, err, errFake)
}

func TestDelegationRepository_listWhere_ScanError(t *testing.T) {
	db := setupTestDB(t)
	repo := NewDelegationRepository(db.App)
	tenantID := uuid.New()
	ctx := WithTx(withTenant(context.Background(), tenantID), &scanErrQueryTx{err: errFake})
	_, err := repo.List(ctx, tenantID)
	require.Error(t, err)
}

func TestDelegationRepository_FindByID_NonErrNoRowsScanError(t *testing.T) {
	db := setupTestDB(t)
	repo := NewDelegationRepository(db.App)
	tenantID := uuid.New()
	ctx := WithTx(withTenant(context.Background(), tenantID), &errQueryRowTx{err: errFake})
	_, err := repo.FindByID(ctx, tenantID, uuid.New())
	require.ErrorIs(t, err, errFake)
}

func TestDelegationRepository_FindActiveDeptDelegateForUser_ScanError(t *testing.T) {
	db := setupTestDB(t)
	repo := NewDelegationRepository(db.App)
	tenantID := uuid.New()
	ctx := WithTx(withTenant(context.Background(), tenantID), &errQueryRowTx{err: errFake})
	_, err := repo.FindActiveDeptDelegateForUser(ctx, tenantID, uuid.New(), uuid.New())
	require.ErrorIs(t, err, errFake)
}

func TestDelegationRepository_Insert_ExecScanError(t *testing.T) {
	db := setupTestDB(t)
	repo := NewDelegationRepository(db.App)
	tenantID := uuid.New()
	ctx := WithTx(withTenant(context.Background(), tenantID), &errQueryRowTx{err: errFake})
	_, err := repo.Insert(ctx, &domain.Delegation{
		TenantID:    tenantID,
		DelegatorID: uuid.New(),
		DelegateID:  uuid.New(),
		Scope:       domain.ScopeAll,
	})
	require.ErrorIs(t, err, errFake)
}

func TestDelegationRepository_End_ScanError(t *testing.T) {
	db := setupTestDB(t)
	repo := NewDelegationRepository(db.App)
	tenantID := uuid.New()
	ctx := WithTx(withTenant(context.Background(), tenantID), &errQueryRowTx{err: errFake})
	_, err := repo.End(ctx, tenantID, uuid.New(), domain.DelegationCancelled, 1)
	require.ErrorIs(t, err, errFake)
}

func TestDelegationRepository_Activate_ScanError(t *testing.T) {
	db := setupTestDB(t)
	repo := NewDelegationRepository(db.App)
	tenantID := uuid.New()
	ctx := WithTx(withTenant(context.Background(), tenantID), &errQueryRowTx{err: errFake})
	_, err := repo.Activate(ctx, tenantID, uuid.New(), 1)
	require.ErrorIs(t, err, errFake)
}

func TestDelegationRepository_ExtendReview_ScanError(t *testing.T) {
	db := setupTestDB(t)
	repo := NewDelegationRepository(db.App)
	tenantID := uuid.New()
	ctx := WithTx(withTenant(context.Background(), tenantID), &errQueryRowTx{err: errFake})
	_, err := repo.ExtendReview(ctx, tenantID, uuid.New(), 30, 1)
	require.ErrorIs(t, err, errFake)
}

// twoCallQueryRowTx: first QueryRow returns ErrNoRows (scan), second returns a non-ErrNoRows error.
// Used to test ExtendReview's probe scan-error branch (line 278).
type twoCallQueryRowTx struct {
	pgx.Tx
	calls int
	err   error
}

func (t *twoCallQueryRowTx) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	t.calls++
	if t.calls == 1 {
		return scanErrRow{err: pgx.ErrNoRows}
	}
	return scanErrRow{err: t.err}
}

func TestDelegationRepository_ExtendReview_ProbeScanError(t *testing.T) {
	db := setupTestDB(t)
	repo := NewDelegationRepository(db.App)
	tenantID := uuid.New()
	ctx := WithTx(withTenant(context.Background(), tenantID), &twoCallQueryRowTx{err: errFake})
	_, err := repo.ExtendReview(ctx, tenantID, uuid.New(), 30, 1)
	require.ErrorIs(t, err, errFake)
}

func TestDelegationRepository_probeVersionConflict_ScanError(t *testing.T) {
	db := setupTestDB(t)
	repo := NewDelegationRepository(db.App)
	fakeTx := &errQueryRowTx{err: errFake}
	err := repo.probeVersionConflict(context.Background(), fakeTx, uuid.New(), uuid.New())
	require.ErrorIs(t, err, errFake)
}

func TestDelegationRepository_MarkReviewWarned_ExecError(t *testing.T) {
	db := setupTestDB(t)
	repo := NewDelegationRepository(db.App)
	tenantID := uuid.New()
	ctx := WithTx(withTenant(context.Background(), tenantID), &errExecTx{err: errFake})
	err := repo.MarkReviewWarned(ctx, tenantID, uuid.New(), 3, 1)
	require.ErrorIs(t, err, errFake)
}

func TestDelegationRepository_EndForUser_QueryError(t *testing.T) {
	db := setupTestDB(t)
	repo := NewDelegationRepository(db.App)
	tenantID := uuid.New()
	ctx := WithTx(withTenant(context.Background(), tenantID), &errQueryTx{err: errFake})
	_, err := repo.EndForUser(ctx, tenantID, uuid.New())
	require.ErrorIs(t, err, errFake)
}

func TestDelegationRepository_EndForUser_ScanError(t *testing.T) {
	db := setupTestDB(t)
	repo := NewDelegationRepository(db.App)
	tenantID := uuid.New()
	ctx := WithTx(withTenant(context.Background(), tenantID), &scanErrQueryTx{err: errFake})
	_, err := repo.EndForUser(ctx, tenantID, uuid.New())
	require.Error(t, err)
}

func TestDelegationRepository_EndForDisabledDelegate_QueryError(t *testing.T) {
	db := setupTestDB(t)
	repo := NewDelegationRepository(db.App)
	tenantID := uuid.New()
	ctx := WithTx(withTenant(context.Background(), tenantID), &errQueryTx{err: errFake})
	_, err := repo.EndForDisabledDelegate(ctx, tenantID, uuid.New())
	require.ErrorIs(t, err, errFake)
}

func TestDelegationRepository_EndForDisabledDelegate_ScanError(t *testing.T) {
	db := setupTestDB(t)
	repo := NewDelegationRepository(db.App)
	tenantID := uuid.New()
	ctx := WithTx(withTenant(context.Background(), tenantID), &scanErrQueryTx{err: errFake})
	_, err := repo.EndForDisabledDelegate(ctx, tenantID, uuid.New())
	require.Error(t, err)
}

func TestDelegationRepository_HardPurgeSoftDeletedBefore_ExecError(t *testing.T) {
	db := setupTestDB(t)
	repo := NewDelegationRepository(db.App)
	ctx := WithTx(context.Background(), &errExecTx{err: errFake})
	_, err := repo.HardPurgeSoftDeletedBefore(ctx, time.Now(), 10)
	require.ErrorIs(t, err, errFake)
}

// ── gauge_repository ─────────────────────────────────────────────────────

func TestGaugeRepository_CountActiveByTenant_QueryError(t *testing.T) {
	db := setupTestDB(t)
	repo := NewGaugeRepository(db.Bypass)
	ctx := WithTx(context.Background(), &errQueryTx{err: errFake})
	_, err := repo.CountActiveByTenant(ctx)
	require.ErrorIs(t, err, errFake)
}

func TestGaugeRepository_CountActiveByTenant_ScanError(t *testing.T) {
	db := setupTestDB(t)
	repo := NewGaugeRepository(db.Bypass)
	ctx := WithTx(context.Background(), &scanErrQueryTx{err: errFake})
	_, err := repo.CountActiveByTenant(ctx)
	require.Error(t, err)
}

// ── processed_events ─────────────────────────────────────────────────────

func TestProcessedEventsRepository_Prune_ExecError(t *testing.T) {
	db := setupTestDB(t)
	repo := NewProcessedEventsRepository(db.Bypass)
	ctx := WithTx(context.Background(), &errExecTx{err: errFake})
	_, err := repo.Prune(ctx, 0, 10)
	require.ErrorIs(t, err, errFake)
}

// ── settings_repository ──────────────────────────────────────────────────

func TestSettingsRepository_Get_ScanError(t *testing.T) {
	db := setupTestDB(t)
	repo := NewSettingsRepository(db.App)
	tenantID := uuid.New()
	ctx := WithTx(withTenant(context.Background(), tenantID), &errQueryRowTx{err: errFake})
	_, err := repo.Get(ctx, tenantID)
	require.ErrorIs(t, err, errFake)
}

func TestSettingsRepository_Upsert_ScanError(t *testing.T) {
	db := setupTestDB(t)
	repo := NewSettingsRepository(db.App)
	tenantID := uuid.New()
	ctx := WithTx(withTenant(context.Background(), tenantID), &errQueryRowTx{err: errFake})
	_, err := repo.Upsert(ctx, tenantID, 90, 30)
	require.ErrorIs(t, err, errFake)
}
