// Package postgres is the outbound Postgres adapter: the RLS-scoped
// delegation_app connection pool, the TxRunner that wraps
// pgcommon.RunInTx and injects port.EventPublisher into context,
// and the DelegationRepository / SettingsRepository / GaugeRepository implementations.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/puddle/v2"
)

// DSNFromEnv builds a PostgreSQL connection URL for the application pool by
// delegating host/port/user/password/dbname/sslmode parsing and DSN
// assembly to pgcommon.ConfigFromEnv() — the same env vars
// (DATABASE_URL/PG_HOST/PG_PORT/PG_USER/PG_PASSWORD/PG_DBNAME/PG_SSLMODE)
// platform-pgcommon itself reads to build the pool Config used by main.go,
// so there is exactly one DSN-assembly implementation instead of two
// drifting in parallel. Warnings from ConfigFromEnv (invalid/insecure
// settings replaced by defaults) are surfaced at the call site that owns a
// logger (see cmd/server/main.go); this helper only returns the DSN string.
// Mirrors iam-user-profile's/iam-org-membership's identical DSNFromEnv.
//
// URL format is required because the migration runner (pgmigrate.Runner)
// calls url.Parse on the DSN after prepending "pgx5://"; a keyword/value DSN
// would produce invalid URL escapes (%20 for spaces) and fail at startup.
// pgcommon builds the DSN via net/url, which already produces this format.
func DSNFromEnv() string {
	cfg, _ := pgcommon.ConfigFromEnv()
	if os.Getenv("DATABASE_URL") != "" {
		// DATABASE_URL is returned verbatim by pgcommon.ConfigFromEnv — set
		// statement_timeout via its own query string, not appended here.
		return cfg.DSN
	}
	return ApplyStatementTimeout(cfg.DSN)
}

// ApplyStatementTimeout appends a server-side statement_timeout option to dsn
// so hung queries release pool connections instead of holding them for the
// full request deadline. PG_STATEMENT_TIMEOUT accepts a Go duration string
// (e.g. "5s", "500ms"). This has no pgcommon equivalent — pgcommon.Config has
// no statement-timeout field — so it remains a small extension layered on
// top of the pgcommon-built DSN rather than a full DSN builder. Ignored when
// dsn is empty or PG_STATEMENT_TIMEOUT is unset. Idempotent: a DSN that
// already carries statement_timeout is returned unchanged.
func ApplyStatementTimeout(dsn string) string {
	if dsn == "" {
		return dsn
	}
	if t := os.Getenv("PG_STATEMENT_TIMEOUT"); t != "" {
		if d, err := time.ParseDuration(t); err == nil && d > 0 {
			if strings.Contains(dsn, "statement_timeout") {
				return dsn
			}
			dsn += fmt.Sprintf("&options=-c%%20statement_timeout%%3D%d", d.Milliseconds())
		}
	}
	return dsn
}

// SystemPoolConfig returns pgcommon.Config for the BYPASSRLS system pool
// (LLD §7.2.3), matching iam-user-profile. The system pool deliberately has
// no GUCProvider — cross-tenant reconciler/sweep queries run under a
// BYPASSRLS role — but still connects through PgBouncer in production, so
// PGBouncerMode is forced true unconditionally (SimpleProtocol + MinConns:0).
// A bare pgcommon.Config{DSN, Logger} literal would leave PGBouncerMode at
// the Go zero-value false and drop ConfigFromEnv pool sizing, breaking
// transaction-pooling deployments even when the app pool correctly reads
// PG_BOUNCER_MODE.
//
// Pool sizing, lifetimes, and SlowQueryThreshold are copied from
// ConfigFromEnv so sysPool and the app pool share one env-driven source
// of truth. Tracer is left unset — call sites wire NewOTelTracer so
// db.query spans export through gincommon's TracerProvider.
func SystemPoolConfig(dsn string, log Logger) pgcommon.Config {
	cfg, _ := pgcommon.ConfigFromEnv()
	cfg.DSN = ApplyStatementTimeout(dsn)
	cfg.GUCProvider = nil
	cfg.PGBouncerMode = true
	cfg.Tracer = nil
	if log != nil {
		cfg.Logger = NewLoggerAdapter(log)
	} else {
		cfg.Logger = nil
	}
	return cfg
}

// SystemDSNFromEnv returns the DSN to use for the delegation_migrator
// (BYPASSRLS) role's privileged cross-tenant pool — required by the
// reconciler's cross-tenant sweep queries (ListExpiringBefore,
// FindDueForDailyWarn, FindDueForAutoEnd, HardPurgeSoftDeletedBefore, LLD
// §11.3/§11.4/§18.4) and the cascade consumer's processed_events ledger
// (RLS-exempt, LLD §7.2.3).
//
// Falls back to DSNFromEnv() when SYSTEM_DATABASE_URL is unset — safe for
// local dev where RLS is not enforced. In production the two DSNs MUST
// differ so the app pool remains scoped to the RLS-enforced role. Mirrors
// iam-user-profile's/iam-org-membership's identical SystemDSNFromEnv — the
// caller compares the returned DSN against DSNFromEnv()'s to detect the
// fallback and log a warning, rather than a second bool return value.
func SystemDSNFromEnv() string {
	if dsn := os.Getenv("SYSTEM_DATABASE_URL"); dsn != "" {
		return dsn
	}
	return DSNFromEnv()
}

// MigrationDSNFromEnv returns the DSN to use for schema migrations.
// Migrations must bypass PgBouncer (transaction pooling) because
// golang-migrate uses pg_advisory_lock which is session-scoped and breaks
// across pooled connections. MIGRATION_DATABASE_URL overrides to a direct
// Postgres connection; falls back to DSNFromEnv() when not set (safe for
// direct-Postgres setups). Mirrors iam-user-profile's/iam-org-membership's
// identical MigrationDSNFromEnv.
func MigrationDSNFromEnv() string {
	if dsn := os.Getenv("MIGRATION_DATABASE_URL"); dsn != "" {
		return ApplyStatementTimeout(dsn)
	}
	return DSNFromEnv()
}

// WithTenantGUC binds app.tenant_id (and, when userID is non-empty,
// app.user_id) into ctx via pgcommon's GUCSet so the next pool checkout
// issues `SET LOCAL app.tenant_id = ...` (RLS-6). The reconciler jobs
// (delegation-expiry, delegation-review) and the cascade consumer MUST call
// this before any UPDATE against a specific tenant's rows — without it the
// WITH CHECK / USING clause silently affects zero rows rather than erroring,
// the split-brain trap DEL-6 guards against (LLD §7.3).
func WithTenantGUC(ctx context.Context, tenantID uuid.UUID, userID string) context.Context {
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.TenantID = tenantID.String()
	if userID != "" {
		g.UserID = userID
	}
	return pgcommon.WithGUCSet(ctx, g)
}

// TxRunner implements port.TxRunner over pgcommon.RunInTx. Inside the
// callback it binds the active pgx.Tx into context (so repository methods
// called via port.DelegationRepository/SettingsRepository join the same
// transaction rather than opening a new one) and, when a publisher is
// provided, binds it via port.WithEventPublisher so service code can
// enqueue events atomically with the state change (DLG-EVT-1) without ever
// importing this package or touching pgx.Tx directly.
// Matching iam-realm-provisioner: postgres does not import platform-events;
// eventbus.Publisher reads the tx via TxFromContext and calls outbox.Enqueue.
type TxRunner struct {
	pool   *pgcommon.Pool
	events port.EventPublisher
}

var _ port.TxRunner = (*TxRunner)(nil)

// writeRetryOpts is pgcommon's documented high-throughput OLTP preset.
// Deadlock (40P01) and serialization failure (40001) retry with exponential
// backoff + jitter. Nested withPool joins (already inside a tx) do not
// retry — the outer TxRunner owns the attempt. Matching iam-realm-provisioner.
var writeRetryOpts = pgcommon.RetryOptions{
	MaxAttempts:    3,
	InitialWait:    10 * time.Millisecond,
	MaxWait:        500 * time.Millisecond,
	Multiplier:     2.0,
	JitterFraction: 0.25,
}

// NewTxRunner constructs a TxRunner. Pass nil for events during bootstrap
// paths where no outbox writes occur. Matching iam-realm-provisioner.
func NewTxRunner(pool *pgcommon.Pool, events port.EventPublisher) *TxRunner {
	return &TxRunner{pool: pool, events: events}
}

// RunInTx implements port.TxRunner (see the TxRunner doc comment above).
// Contended writes retry via pgcommon.RunInTxWithRetryOpts on deadlock /
// serialization failure (iam-realm-provisioner).
func (r *TxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return wrapConnErr(pgcommon.RunInTxWithRetryOpts(ctx, r.pool, pgx.TxOptions{}, writeRetryOpts, func(ctx context.Context, tx pgx.Tx) error {
		txCtx := WithTx(ctx, tx)
		if r.events != nil {
			txCtx = port.WithEventPublisher(txCtx, r.events)
		}
		return fn(txCtx)
	}))
}

type txKey struct{}

// WithTx stores the active pgx.Tx in ctx so repository withPool joins and
// EventPublisher.Enqueue writes the outbox on the same transaction.
func WithTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

// TxFromContext retrieves the active pgx.Tx set by RunInTx, if any.
func TxFromContext(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	return tx, ok
}

// withPool runs fn against the tx already bound on ctx by TxRunner, or, when
// no tx is bound (a plain read-only call outside a TxRunner.RunInTx
// closure), opens a short-lived transaction of its own via pgcommon.RunInTx.
func withPool(ctx context.Context, pool *pgcommon.Pool, fn func(pgx.Tx) error) error {
	if tx, ok := TxFromContext(ctx); ok {
		return wrapConnErr(fn(tx))
	}
	return wrapConnErr(pgcommon.RunInTx(ctx, pool, pgx.TxOptions{}, func(_ context.Context, tx pgx.Tx) error {
		return fn(tx)
	}))
}

// wrapConnErr converts non-protocol database errors into
// domain.ErrDBUnavailable (503). SQL-protocol errors that are not
// connectivity/resource classes pass through so the service layer can
// distinguish an integrity violation from a network outage.
//
// SQLSTATE class 08 (connection exception), 53 (insufficient resources),
// 57 (operator intervention) and 58 (system error) are remapped here so
// HTTP HandleError never needs to inspect a raw *pgconn.PgError.
// puddle.ErrClosedPool is the other positively-identifiable connectivity
// failure. Everything else — including a plain Go error a caller's own
// RunInTx/withPool callback returns — passes through unchanged.
// Matching iam-realm-provisioner (pgcommon v1.3.0 helpers).
func wrapConnErr(err error) error {
	if err == nil {
		return nil
	}
	var de *domain.Error
	if errors.As(err, &de) {
		return err
	}
	if pgcommon.IsConnectionException(err) || pgcommon.IsInsufficientResources(err) || isOperatorOrSystemErrorSQLState(err) || errors.Is(err, puddle.ErrClosedPool) {
		return domain.NewError(domain.ErrDBUnavailable, "database unavailable")
	}
	return err
}

// isOperatorOrSystemErrorSQLState reports whether err is a Postgres error
// in SQLSTATE class 57 or 58. pgcommon v1.3.0 has dedicated helpers for
// 08/53 but not these two; we classify via the pgconn Error() text
// ("… (SQLSTATE 57P01)") so this package never imports pgconn.
func isOperatorOrSystemErrorSQLState(err error) bool {
	if !pgcommon.IsPgError(err) {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLSTATE 57") || strings.Contains(msg, "SQLSTATE 58")
}
