package service

import (
	"context"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSettingsService_Set_MaxDurationDaysOutOfRange(t *testing.T) {
	for _, days := range []int{0, -5, 181, 1000} {
		repo := &fakeSettingsRepository{}
		svc := NewSettingsService(repo)

		_, err := svc.Set(context.Background(), uuid.New(), days, 90)
		requireDomainCode(t, err, domain.ErrInvalidDelegationMaxDurationDays.Error())
		assert.Empty(t, repo.upsertCalls, "an invalid max_duration_days must never reach Upsert")
	}
}

func TestSettingsService_Set_ReviewWindowDaysOutOfRange(t *testing.T) {
	for _, days := range []int{0, -1, 181, 999} {
		repo := &fakeSettingsRepository{}
		svc := NewSettingsService(repo)

		_, err := svc.Set(context.Background(), uuid.New(), 90, days)
		requireDomainCode(t, err, domain.ErrInvalidDelegationReviewWindow.Error())
		assert.Empty(t, repo.upsertCalls, "an invalid review_window_days must never reach Upsert")
	}
}

func TestSettingsService_Set_HappyPathCallsUpsert(t *testing.T) {
	repo := &fakeSettingsRepository{}
	svc := NewSettingsService(repo)
	tenantID := uuid.New()

	got, err := svc.Set(context.Background(), tenantID, 45, 60)
	require.NoError(t, err)

	require.Len(t, repo.upsertCalls, 1)
	assert.Equal(t, tenantID, repo.upsertCalls[0].tenantID)
	assert.Equal(t, 45, repo.upsertCalls[0].maxDurationDays)
	assert.Equal(t, 60, repo.upsertCalls[0].reviewWindowDays)
	assert.Equal(t, 45, got.MaxDurationDays)
	assert.Equal(t, 60, got.ReviewWindowDays)
}

func TestSettingsService_Get_ReturnsDefaultWhenNoRow(t *testing.T) {
	repo := &fakeSettingsRepository{}
	svc := NewSettingsService(repo)
	tenantID := uuid.New()

	got, err := svc.Get(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Equal(t, domain.DefaultMaxDurationDays, got.MaxDurationDays)
	assert.Equal(t, domain.DefaultReviewWindowDays, got.ReviewWindowDays)
}
