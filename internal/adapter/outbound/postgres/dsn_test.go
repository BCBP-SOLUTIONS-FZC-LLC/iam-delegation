package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDSNFromEnv_StatementTimeout verifies that a valid PG_STATEMENT_TIMEOUT
// duration is appended to the DSN as a millisecond integer in the options
// query parameter.
func TestDSNFromEnv_StatementTimeout(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("PG_HOST", "localhost")
	t.Setenv("PG_PORT", "5432")
	t.Setenv("PG_USER", "u")
	t.Setenv("PG_PASSWORD", "p")
	t.Setenv("PG_DBNAME", "db")
	t.Setenv("PG_SSLMODE", "disable")
	t.Setenv("PG_STATEMENT_TIMEOUT", "5s")

	dsn := DSNFromEnv()
	require.NotEmpty(t, dsn)
	assert.Contains(t, dsn, "statement_timeout", "DSN must include statement_timeout option")
	assert.Contains(t, dsn, "5000", "5s must be encoded as 5000 ms")
}

// TestDSNFromEnv_InvalidTimeout_Ignored verifies that an unparseable
// PG_STATEMENT_TIMEOUT value is silently ignored and does not appear in the
// generated DSN.
func TestDSNFromEnv_InvalidTimeout_Ignored(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("PG_HOST", "localhost")
	t.Setenv("PG_PORT", "5432")
	t.Setenv("PG_USER", "u")
	t.Setenv("PG_PASSWORD", "p")
	t.Setenv("PG_DBNAME", "db")
	t.Setenv("PG_SSLMODE", "disable")
	t.Setenv("PG_STATEMENT_TIMEOUT", "notaduration")

	dsn := DSNFromEnv()
	require.NotEmpty(t, dsn)
	assert.NotContains(t, dsn, "statement_timeout", "invalid timeout must be ignored")
}

// TestDSNFromEnv_ZeroDuration_Ignored verifies that a zero-duration timeout
// (e.g. "0s") is not appended to the DSN.
func TestDSNFromEnv_ZeroDuration_Ignored(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("PG_HOST", "localhost")
	t.Setenv("PG_PORT", "5432")
	t.Setenv("PG_USER", "u")
	t.Setenv("PG_PASSWORD", "p")
	t.Setenv("PG_DBNAME", "db")
	t.Setenv("PG_SSLMODE", "disable")
	t.Setenv("PG_STATEMENT_TIMEOUT", "0s")

	dsn := DSNFromEnv()
	require.NotEmpty(t, dsn)
	assert.NotContains(t, dsn, "statement_timeout", "zero duration must be ignored")
}

// TestDSNFromEnv_DATABASE_URL_TakesPrecedence verifies that DATABASE_URL is
// returned verbatim and PG_STATEMENT_TIMEOUT is not applied to it.
func TestDSNFromEnv_DATABASE_URL_TakesPrecedence(t *testing.T) {
	const rawDSN = "postgres://user:pass@host:5432/db?sslmode=disable"
	t.Setenv("DATABASE_URL", rawDSN)
	t.Setenv("PG_STATEMENT_TIMEOUT", "10s")

	dsn := DSNFromEnv()
	assert.Equal(t, rawDSN, dsn, "DATABASE_URL must be returned verbatim, no timeout appended")
}

// ApplyStatementTimeout is a small extension layered on top of the
// pgcommon-built DSN (LLD has no pgcommon equivalent); an empty dsn must be
// returned unchanged regardless of PG_STATEMENT_TIMEOUT.
func TestApplyStatementTimeout_EmptyDSN_ReturnsEmpty(t *testing.T) {
	t.Setenv("PG_STATEMENT_TIMEOUT", "5s")
	assert.Equal(t, "", ApplyStatementTimeout(""))
}

func TestApplyStatementTimeout_Idempotent(t *testing.T) {
	t.Setenv("PG_STATEMENT_TIMEOUT", "5s")
	once := ApplyStatementTimeout("postgres://u@h/db?sslmode=disable")
	assert.Equal(t, once, ApplyStatementTimeout(once))
}

func TestSystemPoolConfig_ForcesPGBouncerMode(t *testing.T) {
	t.Setenv("PG_BOUNCER_MODE", "false")
	t.Setenv("PG_MAX_CONNS", "20")
	t.Setenv("PG_SLOW_QUERY_THRESHOLD", "200ms")
	t.Setenv("PG_STATEMENT_TIMEOUT", "")

	cfg := SystemPoolConfig("postgres://sys@host/db", nil)
	assert.Equal(t, "postgres://sys@host/db", cfg.DSN)
	assert.True(t, cfg.PGBouncerMode, "sysPool must force PGBouncerMode:true — zero-value false breaks PgBouncer txn pooling")
	assert.Nil(t, cfg.GUCProvider, "sysPool must not inject tenant GUCs")
	assert.Nil(t, cfg.Tracer, "Tracer is wired by the call site, not SystemPoolConfig")
	assert.Nil(t, cfg.Logger, "nil log must leave Logger unset")
	assert.Equal(t, int32(20), cfg.MaxConns, "sysPool inherits pool sizing from ConfigFromEnv")
}

func TestSystemPoolConfig_NonNilLogger_WrapsWithAdapter(t *testing.T) {
	cfg := SystemPoolConfig("postgres://sys@host/db", &fakeMapLogger{})
	assert.NotNil(t, cfg.Logger, "a non-nil log must be wrapped, not dropped")
}

func TestSystemPoolConfig_AppliesStatementTimeout(t *testing.T) {
	t.Setenv("PG_STATEMENT_TIMEOUT", "5s")
	cfg := SystemPoolConfig("postgres://sys@host/db?sslmode=disable", nil)
	assert.Contains(t, cfg.DSN, "statement_timeout%3D5000")
}

// TestSystemDSNFromEnv_UsesEnvVarWhenSet covers the SYSTEM_DATABASE_URL-set
// branch of SystemDSNFromEnv.
func TestSystemDSNFromEnv_UsesEnvVarWhenSet(t *testing.T) {
	const want = "postgres://sys:sys@sysdb:5432/delegation?sslmode=disable"
	t.Setenv("SYSTEM_DATABASE_URL", want)
	assert.Equal(t, want, SystemDSNFromEnv())
}

// TestSystemDSNFromEnv_FallsBackToAppDSN covers the fallback branch — an
// unset SYSTEM_DATABASE_URL must resolve through DSNFromEnv(), the same
// pgcommon-built DSN the app pool uses, not a bare os.Getenv("DATABASE_URL")
// (which would silently be empty under PG_HOST/PG_PORT-style config).
func TestSystemDSNFromEnv_FallsBackToAppDSN(t *testing.T) {
	t.Setenv("SYSTEM_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("PG_HOST", "apphost")
	t.Setenv("PG_PORT", "5432")
	t.Setenv("PG_USER", "u")
	t.Setenv("PG_PASSWORD", "p")
	t.Setenv("PG_DBNAME", "delegation")
	t.Setenv("PG_SSLMODE", "disable")

	want := DSNFromEnv()
	require.NotEmpty(t, want, "DSNFromEnv must build a DSN from PG_* vars even with no DATABASE_URL")
	assert.Equal(t, want, SystemDSNFromEnv())
}

func TestMigrationDSNFromEnv_PrefersMigrationDatabaseURL(t *testing.T) {
	t.Setenv("MIGRATION_DATABASE_URL", "postgres://migrator@host/db")
	t.Setenv("DATABASE_URL", "postgres://app@host/db")
	t.Setenv("PG_STATEMENT_TIMEOUT", "")
	assert.Equal(t, "postgres://migrator@host/db", MigrationDSNFromEnv())
}

func TestMigrationDSNFromEnv_AppliesStatementTimeout(t *testing.T) {
	t.Setenv("MIGRATION_DATABASE_URL", "postgres://m:x@migrations.example:5432/db?sslmode=disable")
	t.Setenv("PG_STATEMENT_TIMEOUT", "5s")
	assert.Contains(t, MigrationDSNFromEnv(), "statement_timeout%3D5000")
}

// TestMigrationDSNFromEnv_FallsBackToAppDSN covers the fallback branch —
// same rationale as TestSystemDSNFromEnv_FallsBackToAppDSN.
func TestMigrationDSNFromEnv_FallsBackToAppDSN(t *testing.T) {
	t.Setenv("MIGRATION_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("PG_HOST", "apphost")
	t.Setenv("PG_PORT", "5432")
	t.Setenv("PG_USER", "u")
	t.Setenv("PG_PASSWORD", "p")
	t.Setenv("PG_DBNAME", "delegation")
	t.Setenv("PG_SSLMODE", "disable")

	want := DSNFromEnv()
	require.NotEmpty(t, want)
	assert.Equal(t, want, MigrationDSNFromEnv())
}
