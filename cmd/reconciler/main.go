// Command iam-delegation-reconciler runs one of the four CronJob entry
// points (delegation-expiry, delegation-activation, delegation-review,
// delegation-cleanup, LLD §16.1) selected by --job, then exits. The Helm
// chart's four CronJob templates each invoke this binary with a fixed --job
// value on their own schedule (*/5 * * * *, */5 * * * *, hourly, monthly —
// LLD §15/§21.2).
//
// This binary shares cmd/reconciler/jobs' implementation with cmd/server's
// DLG-I1/I2 HTTP entry points (DLG-D17) rather than calling them over HTTP.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	gclogger "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/logger"
	pgcommon "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/cmd/reconciler/jobs"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/inbound/consumer"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/metrics"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/userprofile"
)

func main() {
	logger, err := gclogger.NewLogger(getEnv("ENVIRONMENT", "development"))
	if err != nil {
		panic("init logger: " + err.Error())
	}
	if err := run(logger); err != nil {
		logger.Error("iam-delegation-reconciler exited with error", map[string]interface{}{"error": err.Error()})
		os.Exit(1)
	}
}

func run(logger Logger) error {
	jobName := flag.String("job", "", "one of: delegation-expiry, delegation-activation, delegation-review, delegation-cleanup")
	flag.Parse()
	if *jobName == "" {
		return errors.New("--job is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Database — pgcommon.ConfigFromEnv reads DATABASE_URL/PG_* directly —
	// same source cmd/server/main.go uses — so pool sizing and DSN assembly
	// have exactly one implementation instead of a second one hand-rolled
	// here. The reconciler never runs migrations itself (cmd/server's
	// startup already applies them; running the same migration set from two
	// binaries racing at deploy time would be redundant, not incorrect, but
	// pointless) — it only needs the two pools.
	pgCfg, pgWarnings := pgcommon.ConfigFromEnv()
	for _, w := range pgWarnings {
		logger.Warn("postgres config warning", map[string]interface{}{"key": w.Key, "reason": w.Reason})
	}
	// DSNFromEnv (not a bare ApplyStatementTimeout(pgCfg.DSN)) so the
	// DATABASE_URL bypass applies here too: PG_STATEMENT_TIMEOUT must be
	// ignored when DATABASE_URL is set verbatim, per ApplyStatementTimeout's
	// own contract.
	dsn := pgadapter.DSNFromEnv()
	pgCfg.DSN = dsn
	pgCfg.GUCProvider = pgcommon.GUCSetFromContext
	pgCfg.Logger = pgadapter.NewLoggerAdapter(logger)
	appPool, err := pgcommon.NewPool(ctx, pgCfg)
	if err != nil {
		return fmt.Errorf("connect app pool: %w", err)
	}
	//nolint:errcheck // best-effort shutdown cleanup — an error here has no recovery action at process exit.
	defer func() { _ = appPool.DrainAndClose(context.Background()) }()

	sysDSN := pgadapter.SystemDSNFromEnv()
	sysPool, err := pgcommon.NewPool(ctx, pgcommon.Config{
		DSN: sysDSN, Logger: pgadapter.NewLoggerAdapter(logger), PGBouncerMode: true,
	})
	if err != nil {
		return fmt.Errorf("connect system pool: %w", err)
	}
	//nolint:errcheck // best-effort shutdown cleanup — an error here has no recovery action at process exit.
	defer func() { _ = sysPool.DrainAndClose(context.Background()) }()
	if sysDSN == dsn {
		logger.Warn("SYSTEM_DATABASE_URL not set — sysPool reuses app DSN; cross-tenant sweeps will be RLS-filtered", nil)
	}

	userProfileTimeout, err := getEnvDurationMS("USER_PROFILE_TIMEOUT_MS", 3000*time.Millisecond)
	if err != nil {
		return err
	}
	userProfileClient, err := userprofile.NewHTTPClient(os.Getenv("USER_PROFILE_BASE_URL"), nil, userProfileTimeout)
	if err != nil {
		return fmt.Errorf("build User Profile client: %w", err)
	}

	// No schema validator here: this binary never publishes events itself
	// through a Glue-encoded path — it enqueues plain JSON to the same
	// outbox cmd/server's runner drains, and DLG-EVT-1 atomicity is what
	// matters for a cron write, not wire-format validation redundancy
	// (cmd/server already validates every payload shape it defines, and
	// this binary uses the identical domain.*Payload structs).
	txRunner := pgadapter.NewTxRunner(appPool, nil)

	batchLimit, err := getEnvIntVar("CRON_BATCH_LIMIT", 50)
	if err != nil {
		return err
	}
	retentionDays, err := getEnvIntVar("DELEGATION_RETENTION_DAYS", 90)
	if err != nil {
		return err
	}

	// jobs.Cleanup (delegation-cleanup, LLD §18.4/GAP-09) only ever runs
	// through this binary — cmd/server has no HTTP entry point for it
	// (unlike DLG-I1/I2's shared expiry/review path, DLG-D17) — so
	// ProcessedEvents.CleanupExpired is wired here or nowhere.
	processedEvents := consumer.NewProcessedEvents(sysPool)

	// GAP-27 (DLG-D19 partial closure): registered here for jobs.Context
	// symmetry with cmd/server's real *metrics.Metrics (both binaries call
	// jobs.Expiry/ReviewSweep, which call jctx.Metrics unconditionally when
	// non-nil). Registered through gincommon.MetricsRegisterer() — every
	// Prometheus registration in this repo goes through platform-gincommon,
	// never a hand-rolled prometheus.NewRegistry() — even though this binary
	// never runs gincommon.ObservabilityMiddlewares/DefaultMiddlewares, so
	// MetricsRegisterer() falls back to prometheus.DefaultRegisterer (see
	// that function's doc comment). This binary is a one-shot batch process
	// with no /metrics scrape endpoint of its own, so these increments are
	// never exported anywhere regardless of which registry they land in —
	// the alert-visible deferred counters reach Prometheus only via
	// cmd/server's DLG-I1/I2 on-demand entry points, which do have a live
	// scrape target. See .claude/operations.md's Metrics section.
	reconcilerMetrics, err := metrics.Register(gincommon.MetricsRegisterer())
	if err != nil {
		return fmt.Errorf("register metrics: %w", err)
	}

	jctx := &jobs.Context{
		Delegations:     pgadapter.NewDelegationRepository(sysPool),
		UserProfile:     userProfileClient,
		TxRunner:        txRunner,
		BindTenantGUC:   pgadapter.WithTenantGUC,
		Logger:          logger,
		BatchLimit:      batchLimit,
		RetentionDays:   retentionDays,
		ProcessedEvents: processedEvents,
		Metrics:         reconcilerMetrics,
	}

	var result jobs.Result
	switch *jobName {
	case "delegation-expiry":
		result, err = jobs.Expiry(ctx, jctx)
	case "delegation-activation":
		result, err = jobs.Activation(ctx, jctx)
	case "delegation-review":
		result, err = jobs.ReviewSweep(ctx, jctx)
	case "delegation-cleanup":
		result, err = jobs.Cleanup(ctx, jctx)
	default:
		return fmt.Errorf("unknown --job %q", *jobName)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", *jobName, err)
	}
	logger.Info("job complete", map[string]interface{}{
		"job": *jobName, "attempted": result.Attempted, "succeeded": result.Succeeded,
		"failed": result.Failed, "warned_3d": result.Warned3d,
		"warned_2d": result.Warned2d, "warned_1d": result.Warned1d,
		"expired": result.Expired, "deferred": result.Deferred,
		"purged": result.Purged,
	})
	return nil
}
