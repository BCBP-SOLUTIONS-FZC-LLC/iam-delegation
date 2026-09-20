package main

import (
	"os"
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

// TestLoadConfig_DocsAuthTokenFailFast covers the guard added after a
// production-readiness review found registerDocsRoutes only gates
// /swagger and /asyncapi behind docsAuthMiddleware when
// Environment=="production" AND AuthToken!="" — so DOCS_ENABLED=true with
// DOCS_AUTH_TOKEN unset in production would otherwise serve both with no
// auth at all.
func TestLoadConfig_DocsAuthTokenFailFast(t *testing.T) {
	tests := []struct {
		name        string
		environment string
		docsEnabled string
		authToken   string
		wantErr     bool
	}{
		{name: "production, enabled, no token -> error", environment: "production", docsEnabled: "true", authToken: "", wantErr: true},
		{name: "production, enabled, token set -> no error", environment: "production", docsEnabled: "true", authToken: "s3cr3t", wantErr: false},
		{name: "production, disabled, no token -> no error", environment: "production", docsEnabled: "false", authToken: "", wantErr: false},
		{name: "development, enabled, no token -> no error", environment: "development", docsEnabled: "true", authToken: "", wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setBaseRequiredEnv(t)
			t.Setenv("ENVIRONMENT", tt.environment)
			if tt.environment != "development" {
				t.Setenv("SYSTEM_DATABASE_URL", "postgres://migrator@host/db")
			}
			t.Setenv("DOCS_ENABLED", tt.docsEnabled)
			t.Setenv("DOCS_AUTH_TOKEN", tt.authToken)

			_, err := loadConfig()
			if tt.wantErr {
				require.Error(t, err)
				require.ErrorContains(t, err, "DOCS_AUTH_TOKEN is required")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestEnsureOutboxEnv covers the safety net added alongside the DOCS_AUTH_TOKEN
// fail-fast: OUTBOX_* must backfill to this service's historical values
// whenever unset (platform-events' own library defaults differ), and must
// never override a value the environment already set.
func TestEnsureOutboxEnv(t *testing.T) {
	for _, key := range []string{"OUTBOX_POLL_INTERVAL", "OUTBOX_STARTUP_JITTER", "OUTBOX_PUBLISH_CONCURRENCY", "OUTBOX_CLAIM_LEASE_DURATION"} {
		t.Setenv(key, "")
	}
	ensureOutboxEnv()
	require.Equal(t, "500ms", os.Getenv("OUTBOX_POLL_INTERVAL"))
	require.Equal(t, "2s", os.Getenv("OUTBOX_STARTUP_JITTER"))
	require.Equal(t, "4", os.Getenv("OUTBOX_PUBLISH_CONCURRENCY"))
	require.Equal(t, "10m", os.Getenv("OUTBOX_CLAIM_LEASE_DURATION"))

	t.Setenv("OUTBOX_POLL_INTERVAL", "1s")
	ensureOutboxEnv()
	require.Equal(t, "1s", os.Getenv("OUTBOX_POLL_INTERVAL"), "must not override an explicitly set value")
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
