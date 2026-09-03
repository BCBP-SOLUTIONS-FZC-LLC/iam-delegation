package requestctx

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWithContext_FromContext_RoundTrip(t *testing.T) {
	rc := &Context{UserID: "u1", TenantID: "t1", Roles: []string{"tenant_admin"}}
	ctx := WithContext(context.Background(), rc)

	got, ok := FromContext(ctx)
	assert.True(t, ok)
	assert.Same(t, rc, got)
}

func TestFromContext_Absent(t *testing.T) {
	got, ok := FromContext(context.Background())
	assert.False(t, ok)
	assert.Nil(t, got)
}

func TestHasRole(t *testing.T) {
	rc := &Context{Roles: []string{"tenant_admin", "member"}}
	assert.True(t, rc.HasRole("tenant_admin"))
	assert.True(t, rc.HasRole("member"))
	assert.False(t, rc.HasRole("tenant_owner"))
}

func TestIsAdmin(t *testing.T) {
	tests := []struct {
		name  string
		roles []string
		want  bool
	}{
		{"tenant_admin", []string{"tenant_admin"}, true},
		{"tenant_owner", []string{"tenant_owner"}, true},
		{"member only", []string{"member"}, false},
		{"no roles", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rc := &Context{Roles: tc.roles}
			assert.Equal(t, tc.want, rc.IsAdmin())
		})
	}
}
