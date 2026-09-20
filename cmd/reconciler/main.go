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
	"sync"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	gclogger "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/logger"
	pgcommon "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgmetrics"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/cmd/reconciler/jobs"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/eventbus"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/metrics"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/userprofile"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
	events "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

func main() {
	ensureGincommonEnv()
	log, err := gclogger.NewLogger(resolveAppEnv())
	if err != nil {
		panic("init logger: " + err.Error())
	}
	if err := run(log); err != nil {
		log.Error("iam-delegation-reconciler exited with error", map[string]interface{}{"error": err.Error()})
		os.Exit(1)
	}
}

func run(logger port.Logger) error {
	jobName := flag.String("job", "", "one of: delegation-expiry, delegation-activation, delegation-review, delegation-cleanup")
	flag.Parse()
	if *jobName == "" {
		return errors.New("--job is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Same TracerProvider as cmd/server so spans from this job export
	// through gincommon's OTLP pipeline when the collector is set —
	// matching iam-realm-provisioner's reconciler: InitTracingFromEnv,
	// then ObservabilityMiddlewares so Register() picks up {service,
	// version} const labels, then a job-root span.
	// Same LIFO order as cmd/server / iam-realm-provisioner: drain pools
	// (registered later) → shutdownTracing → gincommon.Shutdown (Zap Sync).
	shutdownTracing := gincommon.InitTracingFromEnv()
	//nolint:errcheck // best-effort flush on exit; the job's own exit code is what matters
	defer func() { _ = gincommon.Shutdown(logger) }()
	defer shutdownTracing()
	ginCfg := gincommon.Config{
		Logger:       logger,
		ServiceName:  getEnv("APP_NAME", "iam-delegation-reconciler"),
		BuildVersion: getEnv("BUILD_VERSION", "dev"),
	}
	_ = gincommon.ObservabilityMiddlewares(ginCfg)
	reconcilerMetrics := metrics.Register()
	events.InitWithRegisterer(ginCfg.ServiceName, ginCfg.BuildVersion, gincommon.MetricsRegisterer())
	pgmetrics.InitWithRegisterer(ginCfg.ServiceName, ginCfg.BuildVersion, gincommon.MetricsRegisterer())
	serviceName := ginCfg.ServiceName

	ctx, jobSpan := otel.Tracer(serviceName).Start(ctx, "reconciler."+*jobName)
	defer jobSpan.End()

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
	queryTracer := pgadapter.NewOTelTracer(serviceName)
	pgCfg.Tracer = queryTracer
	appPool, err := pgcommon.NewPool(ctx, pgCfg)
	if err != nil {
		return fmt.Errorf("connect app pool: %w", err)
	}

	// DrainAndClose is pgcommon's graceful path (wait for in-flight
	// WithConn/RunInTx, then close). sync.Once keeps DrainAndClose and a
	// later defer from running concurrently (pgcommon forbids that).
	// Matching iam-realm-provisioner's reconciler.
	var sysPool *pgcommon.Pool
	var drainOnce sync.Once
	drainPools := func() {
		drainOnce.Do(func() {
			drainCtx, cancelDrain := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancelDrain()
			if err := appPool.DrainAndClose(drainCtx); err != nil {
				logger.Error("app pool drain error", map[string]interface{}{"error": err.Error()})
			}
			if sysPool != nil {
				if err := sysPool.DrainAndClose(drainCtx); err != nil {
					logger.Error("sysPool drain error", map[string]interface{}{"error": err.Error()})
				}
			}
		})
	}
	defer drainPools()

	// Fail fast outside local/dev rather than let the dev-safe fallback
	// below degrade silently: every cross-tenant sweep this binary runs
	// would then execute under RLS with no tenant GUC bound and return
	// zero rows instead of erroring — no alert fires. Matching cmd/server
	// loadConfig's isDevLikeEnvironment guard (DLG-D35), not a literal
	// ENVIRONMENT=="production" compare.
	if !isDevLikeEnvironment(resolveAppEnv()) && os.Getenv("SYSTEM_DATABASE_URL") == "" {
		return fmt.Errorf("SYSTEM_DATABASE_URL is required outside local/dev environments (must be the BYPASSRLS delegation_migrator role, see .claude/database.md)")
	}
	sysDSN := pgadapter.SystemDSNFromEnv()
	sysCfg := pgadapter.SystemPoolConfig(sysDSN, logger)
	sysCfg.Tracer = queryTracer
	sysPool, err = pgcommon.NewPool(ctx, sysCfg)
	if err != nil {
		return fmt.Errorf("connect system pool: %w", err)
	}
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

	// ValidatingCodec + Publisher so cron-enqueued DelegationStarted /
	// DelegationEnded / DelegationReviewRequested are schema-checked at
	// enqueue, matching cmd/server and iam-realm-provisioner. Nil publisher
	// would leave EventPublisherFromContext empty and silently skip emission.
	enqueueCodec, err := eventbus.NewValidatingCodec(events.NoopCodec{})
	if err != nil {
		return fmt.Errorf("build enqueue codec: %w", err)
	}
	outboxPublisher := eventbus.New(domain.Source, enqueueCodec).WithLogger(logger)
	txRunner := pgadapter.NewTxRunner(appPool, outboxPublisher)

	batchLimit, err := getEnvIntVar("CRON_BATCH_LIMIT", 50)
	if err != nil {
		return err
	}
	retentionDays, err := getEnvIntVar("DELEGATION_RETENTION_DAYS", 90)
	if err != nil {
		return err
	}
	processedEventsTTLDays, err := getEnvIntVar("PROCESSED_EVENTS_TTL_DAYS", 30)
	if err != nil {
		return err
	}

	// jobs.Cleanup (delegation-cleanup, LLD §18.4/GAP-09) only ever runs
	// through this binary — cmd/server has no HTTP entry point for it
	// (unlike DLG-I1/I2's shared expiry/review path, DLG-D17) — so
	// ProcessedEvents.Prune is wired here or nowhere.
	processedEvents := pgadapter.NewProcessedEventsRepository(sysPool)

	// GAP-27 (DLG-D19 partial closure): registered above via
	// metrics.Register() after ObservabilityMiddlewares so collectors
	// share gincommon's {service, version} labels (iam-realm-provisioner).
	// This binary is a one-shot batch process with no /metrics scrape
	// endpoint of its own, so these increments are never exported
	// anywhere — the alert-visible deferred counters reach Prometheus
	// only via cmd/server's DLG-I1/I2 on-demand entry points. See
	// .claude/operations.md.
	jctx := &jobs.Context{
		Delegations:            pgadapter.NewDelegationRepository(sysPool),
		UserProfile:            userProfileClient,
		TxRunner:               txRunner,
		BindTenantGUC:          pgadapter.WithTenantGUC,
		Logger:                 logger,
		BatchLimit:             batchLimit,
		RetentionDays:          retentionDays,
		ProcessedEvents:        processedEvents,
		ProcessedEventsTTLDays: processedEventsTTLDays,
		Metrics:                reconcilerMetrics,
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
