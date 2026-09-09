package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// setBaseRequiredEnv sets the fail-fast-required vars that loadConfig
// demands regardless of ENVIRONMENT, so these tests exercise only the
// SYSTEM_DATABASE_URL / bouncer-mode branches.
func setBaseRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SNS_TOPIC_ARN", "arn:aws:sns:ap-south-1:000000000000:iam.delegation.events")
	t.Setenv("CASCADE_QUEUE_URL", "https://sqs.ap-south-1.amazonaws.com/000000000000/delegation-cascade-q")
	t.Setenv("DATABASE_URL", "postgres://delegation:delegation@localhost:5432/delegation?sslmode=disable")
	t.Setenv("PG_BOUNCER_MODE", "false")
	t.Setenv("APP_ENV", "")
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
		{name: "test, unset -> no error (CI)", environment: "test", systemDBURL: "", wantErr: false},
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
	for _, env := range []string{"development", "dev", "local", "test", ""} {
		require.True(t, isDevLikeEnvironment(env), "expected %q to be dev-like", env)
	}
	for _, env := range []string{"production", "staging", "uat", "prod"} {
		require.False(t, isDevLikeEnvironment(env), "expected %q to NOT be dev-like", env)
	}
}

func TestLoadConfig_SystemDatabaseURLFailFast_AppEnvWinsOverEnvironment(t *testing.T) {
	setBaseRequiredEnv(t)
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("APP_ENV", "production")
	t.Setenv("SYSTEM_DATABASE_URL", "")

	_, err := loadConfig()
	require.Error(t, err)
	require.ErrorContains(t, err, "SYSTEM_DATABASE_URL is required")
}

func TestLoadConfig_DatabaseURLRequired(t *testing.T) {
	t.Setenv("SNS_TOPIC_ARN", "arn:aws:sns:ap-south-1:000000000000:iam.delegation.events")
	t.Setenv("CASCADE_QUEUE_URL", "https://sqs.ap-south-1.amazonaws.com/000000000000/delegation-cascade-q")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("PG_HOST", "")
	t.Setenv("PG_USER", "")
	t.Setenv("PG_PASSWORD", "")
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("APP_ENV", "")

	_, err := loadConfig()
	require.Error(t, err)
	require.ErrorContains(t, err, "DATABASE_URL")
}

func TestLoadConfig_DatabaseURLSatisfiedByPGSplitVars(t *testing.T) {
	t.Setenv("SNS_TOPIC_ARN", "arn:aws:sns:ap-south-1:000000000000:iam.delegation.events")
	t.Setenv("CASCADE_QUEUE_URL", "https://sqs.ap-south-1.amazonaws.com/000000000000/delegation-cascade-q")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("PG_HOST", "localhost")
	t.Setenv("PG_USER", "delegation")
	t.Setenv("PG_PASSWORD", "delegation")
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("APP_ENV", "")
	t.Setenv("PG_BOUNCER_MODE", "")

	_, err := loadConfig()
	require.NoError(t, err)
}

func TestLoadConfig_MigrationDatabaseURLRequiredWhenBouncerMode(t *testing.T) {
	setBaseRequiredEnv(t)
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("PG_BOUNCER_MODE", "true")
	t.Setenv("MIGRATION_DATABASE_URL", "")

	_, err := loadConfig()
	require.Error(t, err)
	require.ErrorContains(t, err, "MIGRATION_DATABASE_URL")
}

func TestLoadConfig_MigrationDatabaseURLSetWhenBouncerMode(t *testing.T) {
	setBaseRequiredEnv(t)
	t.Setenv("ENVIRONMENT", "production")
	t.Setenv("SYSTEM_DATABASE_URL", "postgres://migrator@host/db")
	t.Setenv("PG_BOUNCER_MODE", "true")
	t.Setenv("MIGRATION_DATABASE_URL", "postgres://migrator@host/db")

	_, err := loadConfig()
	require.NoError(t, err)
}
