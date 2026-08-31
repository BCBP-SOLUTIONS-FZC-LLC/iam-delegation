package postgres

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSettingsRepository_Get_DefaultsWhenMissing(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewSettingsRepository(db.App)
	tenantID := uuid.New()
	ctxA := withTenant(ctx, tenantID)

	got, err := repo.Get(ctxA, tenantID)
	require.NoError(t, err)
	assert.Equal(t, tenantID, got.TenantID)
	assert.Equal(t, 90, got.MaxDurationDays)
	assert.Equal(t, 90, got.ReviewWindowDays)
	assert.Zero(t, got.RecordVersion, "the 90/90 default is not a persisted row")
}

func TestSettingsRepository_Upsert_InsertThenUpdate_RoundTrip(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewSettingsRepository(db.App)
	tenantID := uuid.New()
	ctxA := withTenant(ctx, tenantID)

	created, err := repo.Upsert(ctxA, tenantID, 60, 45)
	require.NoError(t, err)
	assert.Equal(t, 60, created.MaxDurationDays)
	assert.Equal(t, 45, created.ReviewWindowDays)
	assert.Equal(t, int64(1), created.RecordVersion)

	fromGet, err := repo.Get(ctxA, tenantID)
	require.NoError(t, err)
	assert.Equal(t, created, fromGet)

	updated, err := repo.Upsert(ctxA, tenantID, 120, 30)
	require.NoError(t, err)
	assert.Equal(t, 120, updated.MaxDurationDays)
	assert.Equal(t, 30, updated.ReviewWindowDays)
	assert.Equal(t, int64(2), updated.RecordVersion, "touch_row trigger must bump record_version on a real change")
}

// TestSettingsRepository_TouchRowTrigger_NoOpUpdateDoesNotBumpVersion exercises
// TRG-3 via the ON CONFLICT DO UPDATE path: re-upserting identical values
// must not advance record_version (the trigger's WHEN OLD.* IS DISTINCT FROM
// NEW.* guard does not fire).
func TestSettingsRepository_TouchRowTrigger_NoOpUpdateDoesNotBumpVersion(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewSettingsRepository(db.App)
	tenantID := uuid.New()
	ctxA := withTenant(ctx, tenantID)

	created, err := repo.Upsert(ctxA, tenantID, 90, 90)
	require.NoError(t, err)
	require.Equal(t, int64(1), created.RecordVersion)

	sameAgain, err := repo.Upsert(ctxA, tenantID, 90, 90)
	require.NoError(t, err)
	assert.Equal(t, int64(1), sameAgain.RecordVersion, "no-op UPDATE must not bump record_version")
	assert.Equal(t, created.UpdatedAt, sameAgain.UpdatedAt)
}

func TestSettingsRepository_SoftDeleteTenant_DeletesRow(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := NewSettingsRepository(db.App)
	tenantID := uuid.New()
	ctxA := withTenant(ctx, tenantID)

	_, err := repo.Upsert(ctxA, tenantID, 60, 60)
	require.NoError(t, err)

	require.NoError(t, repo.SoftDeleteTenant(ctxA, tenantID))

	var count int
	require.NoError(t, db.Raw.QueryRow(ctx, `SELECT count(*) FROM delegation_tenant_settings WHERE tenant_id = $1`, tenantID).Scan(&count))
	assert.Equal(t, 0, count, "SoftDeleteTenant must hard-delete the policy row (no deleted_at column to mark)")

	// A subsequent Get lazily falls back to the 90/90 default — no error.
	got, err := repo.Get(ctxA, tenantID)
	require.NoError(t, err)
	assert.Equal(t, 90, got.MaxDurationDays)
}

func TestSettingsRepository_Get_CancelledContext_ReturnsError(t *testing.T) {
	db := setupTestDB(t)
	repo := NewSettingsRepository(db.App)
	tenantID := uuid.New()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := repo.Get(withTenant(ctx, tenantID), tenantID)
	require.Error(t, err)
}

func TestSettingsRepository_Upsert_CancelledContext_ReturnsError(t *testing.T) {
	db := setupTestDB(t)
	repo := NewSettingsRepository(db.App)
	tenantID := uuid.New()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := repo.Upsert(withTenant(ctx, tenantID), tenantID, 90, 30)
	require.Error(t, err)
}
