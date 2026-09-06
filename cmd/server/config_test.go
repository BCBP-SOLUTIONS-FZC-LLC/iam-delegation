package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// setBaseRequiredEnv sets the two fail-fast-required vars (SNS_TOPIC_ARN,
// CASCADE_QUEUE_URL) that loadConfig demands regardless of ENVIRONMENT, so
// these tests exercise only the SYSTEM_DATABASE_URL branch.
func setBaseRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SNS_TOPIC_ARN", "arn:aws:sns:ap-south-1:000000000000:iam.delegation.events")
	t.Setenv("CASCADE_QUEUE_URL", "https://sqs.ap-south-1.amazonaws.com/000000000000/delegation-cascade-q")
}

// TestLoadConfig_SystemDatabaseURLFailFast covers the DLG-D34 guard widened
// from a literal ENVIRONMENT=="production" comparison to isDevLikeEnvironment
// — any non-dev-like environment name (not just "production") must fail
// fast when SYSTEM_DATABASE_URL is unset, since pgadapter.SystemDSNFromEnv's
// dev-safe fallback would otherwise silently run cross-tenant reconciler
// sweeps under RLS with no tenant GUC bound.
func TestLoadConfig_SystemDatabaseURLFailFast(t *testing.T) {
	tests := []struct {
		name        string
		environment string
		systemDBURL string
		wantErr     bool
	}{
		{name: "production, unset -> error", environment: "production", systemDBURL: "", wantErr: true},
		{name: "staging, unset -> error (the widened case)", environment: "staging", systemDBURL: "", wantErr: true},
		{name: "uat, unset -> error (the widened case)", environment: "uat", systemDBURL: "", wantErr: true},
		{name: "production, set -> no error", environment: "production", systemDBURL: "postgres://migrator@host/db", wantErr: false},
		{name: "development, unset -> no error", environment: "development", systemDBURL: "", wantErr: false},
		{name: "dev, unset -> no error", environment: "dev", systemDBURL: "", wantErr: false},
		{name: "local, unset -> no error", environment: "local", systemDBURL: "", wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setBaseRequiredEnv(t)
			t.Setenv("ENVIRONMENT", tt.environment)
			t.Setenv("SYSTEM_DATABASE_URL", tt.systemDBURL)

			_, err := loadConfig()
			if tt.wantErr {
				require.Error(t, err)
				require.ErrorContains(t, err, "SYSTEM_DATABASE_URL is required")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestIsDevLikeEnvironment(t *testing.T) {
	for _, env := range []string{"development", "dev", "local", ""} {
		require.True(t, isDevLikeEnvironment(env), "expected %q to be dev-like", env)
	}
	for _, env := range []string{"production", "staging", "uat", "prod"} {
		require.False(t, isDevLikeEnvironment(env), "expected %q to NOT be dev-like", env)
	}
}
