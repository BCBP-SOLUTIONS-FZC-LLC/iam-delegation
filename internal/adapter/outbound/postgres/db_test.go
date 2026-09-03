package postgres

import (
	"context"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func TestWithTenantGUC_BindsTenantAndUser(t *testing.T) {
	tenantID := uuid.New()
	ctx := WithTenantGUC(context.Background(), tenantID, "system-user")

	g, ok := pgcommon.GUCSetFromContext(ctx)
	assert.True(t, ok)
	assert.Equal(t, tenantID.String(), g.TenantID)
	assert.Equal(t, "system-user", g.UserID)
}

func TestWithTenantGUC_EmptyUserIDLeavesExistingUserUnset(t *testing.T) {
	tenantID := uuid.New()
	ctx := WithTenantGUC(context.Background(), tenantID, "")

	g, ok := pgcommon.GUCSetFromContext(ctx)
	assert.True(t, ok)
	assert.Equal(t, tenantID.String(), g.TenantID)
	assert.Empty(t, g.UserID)
}
