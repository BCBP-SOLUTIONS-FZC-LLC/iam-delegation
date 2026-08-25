// Command iam-delegation-reconciler runs one of the three CronJob entry
// points (delegation-expiry, delegation-review, delegation-cleanup, LLD
// §16.1) selected by --job, then exits. The Helm chart's three CronJob
// templates each invoke this binary with a fixed --job value on their own
// schedule (*/5 * * * *, hourly, monthly — LLD §15/§21.2).
//
// This binary shares cmd/reconciler/jobs' implementation with cmd/server's
// DLG-I1/I2 HTTP entry points (DLG-D17) rather than calling them over HTTP.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	pgcommon "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/cmd/reconciler/jobs"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/userprofile"
)

func main() {
	if err := run(); err != nil {
		slog.Error("iam-delegation-reconciler exited with error", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	jobName := flag.String("job", "", "one of: delegation-expiry, delegation-review, delegation-cleanup")
	flag.Parse()
	if *jobName == "" {
		return errors.New("--job is required")
	}

	logger := newReconcilerLogger(getEnv("ENVIRONMENT", "development"))
	slog.SetDefault(logger)
	ml := mapLogger{l: logger}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The reconciler never runs migrations itself (cmd/server's startup
	// already applies them; running the same migration set from two
	// binaries racing at deploy time would be redundant, not incorrect,
	// but pointless) — it only needs the two pools.
	appPool, _, err := pgadapter.NewPool(ctx, slogDomainLogger{l: logger})
	if err != nil {
		return fmt.Errorf("connect app pool: %w", err)
	}
	//nolint:errcheck // best-effort shutdown cleanup — an error here has no recovery action at process exit.
	defer func() { _ = appPool.DrainAndClose(context.Background()) }()

	sysDSN, usedFallback := pgadapter.SystemDSNFromEnv()
	if usedFallback {
		logger.Warn("SYSTEM_DATABASE_URL not set — sysPool reuses app DSN; cross-tenant sweeps will be RLS-filtered")
	}
	sysPool, err := pgcommon.NewPool(ctx, pgcommon.Config{
		DSN: sysDSN, Logger: slogDomainLogger{l: logger}, PGBouncerMode: true,
	})
	if err != nil {
		return fmt.Errorf("connect system pool: %w", err)
	}
	//nolint:errcheck // best-effort shutdown cleanup — an error here has no recovery action at process exit.
	defer func() { _ = sysPool.DrainAndClose(context.Background()) }()

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

	jctx := &jobs.Context{
		Delegations:   pgadapter.NewDelegationRepository(sysPool),
		UserProfile:   userProfileClient,
		TxRunner:      txRunner,
		BindTenantGUC: pgadapter.WithTenantGUC,
		Logger:        ml,
		BatchLimit:    batchLimit,
		RetentionDays: retentionDays,
	}

	var result jobs.Result
	switch *jobName {
	case "delegation-expiry":
		result, err = jobs.Expiry(ctx, jctx)
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
	logger.Info("job complete", slog.String("job", *jobName),
		slog.Int("attempted", result.Attempted), slog.Int("succeeded", result.Succeeded),
		slog.Int("failed", result.Failed), slog.Int("warned_3d", result.Warned3d),
		slog.Int("warned_2d", result.Warned2d), slog.Int("warned_1d", result.Warned1d),
		slog.Int("expired", result.Expired), slog.Int("deferred", result.Deferred),
		slog.Int("purged", result.Purged))
	return nil
}
