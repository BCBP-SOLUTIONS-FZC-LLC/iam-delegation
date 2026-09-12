// Command iam-delegation-server runs the Delegation Service's HTTP API
// (DLG-1…7, DLG-I1…I4) and its cascade SQS consumer (delegation-cascade-q)
// in one process, per LLD §16.1. The reconciler CronJobs
// (delegation-expiry/-review/-cleanup) run as a separate binary,
// cmd/reconciler, sharing cmd/reconciler/jobs' implementation with this
// binary's DLG-I1/I2 HTTP entry points (DLG-D17).
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	events "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/outbox"
	gincommon "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	gclogger "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/logger"
	pgcommon "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgmetrics"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/cmd/reconciler/jobs"
	// Blank-imported for its init()-time swaggo spec registration, read by
	// the /swagger UI wired in internal/adapter/inbound/http/router.go.
	// Regenerate via `make swag` whenever a handler's // @… annotations
	// change (docs/swagger/ is checked in; CI's swagger-staleness check
	// fails a PR whose annotations diverge from it).
	_ "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/docs/swagger"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/inbound/consumer"
	httpadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/inbound/http"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/eventbus"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/orgmembership"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/userprofile"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/valkey"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/service"
)

// buildVersion is stamped at build time via -ldflags="-X main.buildVersion=...".
var buildVersion = "dev"

const serviceName = "iam-delegation"

func main() {
	ensureGincommonEnv(buildVersion)
	appEnv := resolveAppEnv()
	if appEnv != "dev" {
		gin.SetMode(gin.ReleaseMode)
	}
	logger, err := gclogger.NewLogger(appEnv)
	if err != nil {
		panic("init logger: " + err.Error())
	}
	if err := run(logger); err != nil {
		logger.Error("iam-delegation-server exited with error", map[string]interface{}{"error": err.Error()})
		os.Exit(1)
	}
}

func run(logger Logger) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	baseCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Tracing + metrics init through platform-gincommon, matching
	// iam-realm-provisioner line-for-line:
	//   1. InitTracingFromEnv installs the TracerProvider first so any
	//      later otel.Tracer (NewOTelTracer / otelhttp) gets a real
	//      provider, even when OTEL_EXPORTER_OTLP_ENDPOINT is unset (dev).
	//   2. ObservabilityMiddlewares is gincommon's public metrics-init
	//      API — call it before any collector registration so business /
	//      events / pgcommon metrics land on MetricsRegisterer with
	//      matching {service, version} const labels. NewRouter applies
	//      the same middleware slice; gincommon's metrics.Init is sync.Once.
	//   3. metrics.Register() + events/pgmetrics Init immediately after,
	//      before DB or outbound clients.
	// Telemetry shutdown must run AFTER pool DrainAndClose (those defers
	// register later) and in this order, matching iam-realm-provisioner /
	// iam-org-membership: flush the TracerProvider first, then
	// gincommon.Shutdown (which also calls otel.Shutdown and Syncs Zap).
	// Defers are LIFO, so Shutdown is registered first.
	shutdownTracing := gincommon.InitTracingFromEnv()
	defer func() {
		if shutdownErr := gincommon.Shutdown(logger); shutdownErr != nil {
			logger.Error("telemetry shutdown failed", map[string]interface{}{"error": shutdownErr.Error()})
		}
	}()
	defer shutdownTracing()
	ginCfg := gincommon.Config{
		Logger:       logger,
		ServiceName:  getEnv("APP_NAME", serviceName),
		BuildVersion: getEnv("BUILD_VERSION", buildVersion),
	}
	_ = gincommon.ObservabilityMiddlewares(ginCfg)
	appMetrics := metrics.Register(metrics.RegisterConfig{Environment: cfg.Environment})
	events.InitWithRegisterer(ginCfg.ServiceName, ginCfg.BuildVersion, gincommon.MetricsRegisterer())
	pgmetrics.InitWithRegisterer(ginCfg.ServiceName, ginCfg.BuildVersion, gincommon.MetricsRegisterer())

	// Database — pgcommon.ConfigFromEnv reads DATABASE_URL/PG_* directly so
	// pool sizing, PgBouncer mode, and DSN assembly have exactly one
	// implementation instead of a second one hand-rolled here.
	pgCfg, pgWarnings := pgcommon.ConfigFromEnv()
	for _, w := range pgWarnings {
		logger.Warn("postgres config warning", map[string]interface{}{"key": w.Key, "reason": w.Reason})
	}
	// DSNFromEnv (not a bare ApplyStatementTimeout(pgCfg.DSN)) so the
	// DATABASE_URL bypass applies here too: PG_STATEMENT_TIMEOUT must be
	// ignored when DATABASE_URL is set verbatim, per ApplyStatementTimeout's
	// own contract.
	dsn := pgadapter.DSNFromEnv()
	// Migrations must bypass PgBouncer because the runner acquires a
	// pg_advisory_lock, which is session-scoped. MIGRATION_DATABASE_URL
	// points directly at Postgres; falls back to dsn when unset (LLD §7.4).
	migratorDSN := pgadapter.MigrationDSNFromEnv()

	// Migrations run before any pool is opened. Migration order is critical
	// (postgres.Migrate / iam-realm-provisioner): outbox.ApplySchema creates
	// outbox_events BEFORE the domain migration, which GRANTs delegation_app
	// on that table. Domain-then-outbox (the iam-user-profile order) fails
	// every fresh-database bring-up. Crucially, migrations must also precede
	// pool construction — the domain migration creates the delegation_app role,
	// so opening the app pool before it runs fails on a fresh database.
	if err := pgadapter.Migrate(baseCtx, migratorDSN, logger); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	// The RLS-scoped delegation_app pool — every public-route write and
	// read goes through this pool, with app.tenant_id bound per-request by
	// tenantGUCMiddleware (router.go) or per-call by gucBoundReader.
	pgCfg.DSN = dsn
	pgCfg.GUCProvider = pgcommon.GUCSetFromContext
	pgCfg.Logger = pgadapter.NewLoggerAdapter(logger)
	// One shared query tracer on both pools — iam-realm-provisioner.
	queryTracer := pgadapter.NewOTelTracer(ginCfg.ServiceName)
	pgCfg.Tracer = queryTracer
	pool, err := pgcommon.NewPool(baseCtx, pgCfg)
	if err != nil {
		return fmt.Errorf("connect app pool: %w", err)
	}
	// Close is the panic/early-return safety net; DrainAndClose is the
	// graceful path. pgcommon forbids calling them concurrently — these
	// defers are LIFO so DrainAndClose runs first, then Close (idempotent
	// after DrainAndClose). Matching iam-realm-provisioner.
	defer pool.Close()
	defer func() {
		drainCtx, cancelDrain := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelDrain()
		if err := pool.DrainAndClose(drainCtx); err != nil {
			logger.Error("app pool drain error", map[string]interface{}{"error": err.Error()})
		}
	}()

	// The BYPASSRLS delegation_migrator-privileged pool the cross-tenant
	// cron sweep queries and the RLS-exempt processed_events ledger need
	// (LLD §7.2.3/§11.3/§11.4/§18.4). SYSTEM_DATABASE_URL should point at a
	// role with BYPASSRLS in production; falling back to DSNFromEnv()
	// degrades the reconciler's sweeps to RLS-filtered (incomplete) rather
	// than failing startup.
	sysDSN := pgadapter.SystemDSNFromEnv()
	sysCfg := pgadapter.SystemPoolConfig(sysDSN, logger)
	sysCfg.Tracer = queryTracer
	sysPool, err := pgcommon.NewPool(baseCtx, sysCfg)
	if err != nil {
		return fmt.Errorf("connect system pool: %w", err)
	}
	if sysDSN == dsn {
		logger.Warn("SYSTEM_DATABASE_URL not set — sysPool reuses app DSN; cross-tenant cron/internal sweeps will be RLS-filtered", nil)
	}
	defer sysPool.Close()
	defer func() {
		drainCtx, cancelDrain := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelDrain()
		if err := sysPool.DrainAndClose(drainCtx); err != nil {
			logger.Error("sysPool drain error", map[string]interface{}{"error": err.Error()})
		}
	}()

	// Event enqueue (ValidatingCodec + Publisher) + SNS publisher w/ optional
	// Glue codec (publish-time) — the enqueue-vs-publish split, LLD §10.3.1,
	// matching iam-realm-provisioner. postgres.TxRunner injects the publisher;
	// it does not import platform-events.
	enqueueCodec, err := eventbus.NewValidatingCodec(eventbus.NoopCodec{})
	if err != nil {
		return fmt.Errorf("build enqueue codec: %w", err)
	}
	outboxPublisher := eventbus.New(domain.Source, enqueueCodec).WithLogger(logger)
	txRunner := pgadapter.NewTxRunner(pool, outboxPublisher)

	var codec events.Codec = events.NoopCodec{}
	if cfg.GlueRegistryName != "" {
		awsCfg, err := awsconfig.LoadDefaultConfig(baseCtx, awsconfig.WithRegion(cfg.AWSRegion))
		if err != nil {
			return fmt.Errorf("load AWS config: %w", err)
		}
		glueClient := glue.NewFromConfig(awsCfg, func(o *glue.Options) {
			if cfg.AWSEndpointURL != "" {
				o.BaseEndpoint = &cfg.AWSEndpointURL
			}
		})
		gc, err := eventbus.NewGlueCodec(baseCtx, glueClient, cfg.GlueRegistryName, []string{
			domain.EventDelegationStarted, domain.EventDelegationEnded, domain.EventDelegationReviewRequested,
			domain.EventDelegationEscalationRequested,
		})
		if err != nil {
			return fmt.Errorf("build Glue codec: %w", err)
		}
		gc.WithLogger(logger).StartRefresher(baseCtx, 5*time.Minute)
		codec = gc
	}
	snsPublisher, err := events.NewSNSPublisher(events.SNSConfig{
		TopicARN: cfg.SNSTopicARN, Region: cfg.AWSRegion, EndpointURL: cfg.AWSEndpointURL, Logger: logger,
	}, events.WithCodec(codec))
	if err != nil {
		return fmt.Errorf("build SNS publisher: %w", err)
	}

	outboxRunner, err := outbox.NewRunner(outbox.Config{
		Pool: pool, Publisher: snsPublisher, Logger: logger,
		PollInterval:       cfg.OutboxPollInterval,
		BatchSize:          cfg.OutboxBatchSize,
		MaxAttempts:        cfg.OutboxMaxAttempts,
		DrainTimeout:       cfg.OutboxDrainTimeout,
		PublishConcurrency: cfg.OutboxPublishConcurrency,
		PublishTimeout:     cfg.OutboxPublishTimeout,
		StartupJitter:      cfg.OutboxStartupJitter,
		ClaimLeaseDuration: cfg.OutboxClaimLeaseDuration,
	})
	if err != nil {
		return fmt.Errorf("build outbox runner: %w", err)
	}

	// Outbound clients — nil *http.Client so NewHTTPClient wraps
	// httpx.NewClient (otelhttp transport; iam-realm-provisioner).
	userProfileClient, err := userprofile.NewHTTPClient(cfg.UserProfileBaseURL, nil, cfg.UserProfileTimeout)
	if err != nil {
		return fmt.Errorf("build User Profile client: %w", err)
	}
	membershipCheckClient, err := orgmembership.NewHTTPChecker(cfg.OrgMembershipBaseURL, nil, cfg.OrgMembershipTimeout)
	if err != nil {
		return fmt.Errorf("build Org Membership client: %w", err)
	}

	redisClient := valkey.NewClient(cfg.ValkeyAddr)
	//nolint:errcheck // best-effort shutdown cleanup — an error here has no recovery action at process exit.
	defer func() { _ = redisClient.Close() }()
	cache := valkey.NewCache(redisClient, logger)
	idempotencyStore := valkey.NewIdempotencyStore(redisClient, logger)

	// Repositories. delegationRepo/settingsRepo (app pool) back every
	// public-route and cascade-consumer write/read; delegationRepoSys
	// (BYPASSRLS pool) backs only the reconciler's cross-tenant sweep
	// finders — writes issued from inside a jctx.TxRunner.RunInTx closure
	// still go through whichever pgx.Tx that opened (the app pool's),
	// regardless of which pool the repository *value* wraps, since
	// withPool joins an already-bound tx before ever touching r.pool.
	delegationRepo := pgadapter.NewDelegationRepository(pool)
	delegationRepoSys := pgadapter.NewDelegationRepository(sysPool)
	settingsRepo := pgadapter.NewSettingsRepository(pool)

	// Core services.
	delegationService := service.NewDelegationService(delegationRepo, settingsRepo, membershipCheckClient, userProfileClient, idempotencyStore, cache, txRunner).WithMetrics(appMetrics)
	settingsService := service.NewSettingsService(settingsRepo)
	cascadeService := service.NewCascadeService(delegationRepo, settingsRepo, userProfileClient, txRunner).WithMetrics(appMetrics)

	// Reconciler jobs — shared with cmd/reconciler (DLG-D17).
	jctx := &jobs.Context{
		Delegations: delegationRepoSys, UserProfile: userProfileClient, TxRunner: txRunner,
		BindTenantGUC: pgadapter.WithTenantGUC, Logger: logger, BatchLimit: 50, RetentionDays: 90,
		Metrics: appMetrics,
	}
	runner := reconcilerRunner{jctx: jctx}

	// HTTP adapter.
	internalReader := gucBoundReader{repo: delegationRepo}
	delegationHandler := httpadapter.NewDelegationHandler(delegationService, delegationRepo)
	settingsHandler := httpadapter.NewSettingsHandler(settingsService)
	internalHandler := httpadapter.NewInternalHandler(runner, runner, internalReader)

	docs := httpadapter.DocsConfig{Environment: cfg.Environment, Enabled: cfg.DocsEnabled, AuthToken: cfg.DocsAuthToken}
	// tenantGUCMiddleware's identity (pkg/requestctx.Context) carries
	// TenantID/UserID as strings; pgadapter.WithTenantGUC wants a parsed
	// uuid.UUID for TenantID. A malformed header should fail closed (RLS's
	// NULLIF makes an empty/invalid GUC yield zero rows, never a
	// cross-tenant leak) rather than panic, so an unparsable tenantID
	// leaves ctx unchanged.
	bindTenantGUC := func(ctx context.Context, tenantID, userID string) context.Context {
		tid, err := uuid.Parse(tenantID)
		if err != nil {
			return ctx
		}
		return pgadapter.WithTenantGUC(ctx, tid, userID)
	}
	router := httpadapter.NewRouter(
		delegationHandler, settingsHandler, internalHandler,
		pool, sysPool, redisPinger{client: redisClient}, outboxPinger{runner: outboxRunner},
		ginCfg, docs, bindTenantGUC,
	)

	httpServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           router.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Metrics on a dedicated port/listener, separate from the API server
	// — so a NetworkPolicy can grant the monitoring namespace scrape
	// access without also granting it the tenant-facing/gateway API.
	// Helm ServiceMonitor scrapes :9090/metrics (service.metricsPort).
	// Mirrors iam-realm-provisioner's identical split.
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	metricsServer := &http.Server{
		Addr:              ":" + cfg.MetricsPort,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Cascade consumer — Core's MembershipRevoked/TenantMembershipsPurged on
	// delegation-cascade-q (LLD §10.1/§11.5/§11.6).
	processedEvents := pgadapter.NewProcessedEventsRepository(sysPool)
	cascadeConsumer := consumer.NewCascadeConsumer(cascadeService, processedEvents, pgadapter.WithTenantGUC, txRunner, logger)
	awsCfg, err := awsconfig.LoadDefaultConfig(baseCtx, awsconfig.WithRegion(cfg.AWSRegion))
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}
	sqsClient := sqs.NewFromConfig(awsCfg, func(o *sqs.Options) {
		if cfg.AWSEndpointURL != "" {
			o.BaseEndpoint = &cfg.AWSEndpointURL
		}
	})
	// GlueDecodeCodec (unconditional, not gated on cfg.GlueRegistryName):
	// Core (iam-org-membership) Glue-encodes MembershipRevoked/
	// TenantMembershipsPurged whenever ITS OWN GLUE_REGISTRY_MEMBERSHIP_NAME
	// is set — independently of this service's outbound publish-side Glue
	// config — so the inbound consumer needs decode support regardless of
	// whether this service's own events are Glue-encoded (LLD §10.1, DLG-D21).
	sqsConsumer, err := consumer.NewCascadeSQSConsumer(sqsClient, cfg.CascadeQueueURL, cfg.AWSRegion, cfg.AWSEndpointURL, logger, cascadeConsumer,
		events.WithConsumerCodec(eventbus.GlueDecodeCodec{}),
		events.WithConcurrency(cfg.CascadeSQSConcurrency),
	)
	if err != nil {
		return fmt.Errorf("build cascade SQS consumer: %w", err)
	}

	g, gCtx := errgroup.WithContext(baseCtx)
	g.Go(func() error { return outboxRunner.Start(gCtx) })
	g.Go(func() error { return sqsConsumer.Start(gCtx) })
	// iam_delegation_active_gauge is a count of current table state, so
	// no request or reconciler path can maintain it. Refreshed here from
	// the BYPASSRLS sysPool, matching iam-realm-provisioner's exporter
	// goroutines. Returns when gCtx is canceled at shutdown.
	g.Go(func() error {
		runActiveGaugeExporter(gCtx, pgadapter.NewGaugeRepository(sysPool), logger, appMetrics)
		return nil
	})
	// Outbox prune sweep — outbox.Runner.PrunePublished never runs on its
	// own; without this, published outbox_events rows accumulate forever
	// (LLD gap, matching iam-user-profile's identical runMaintenanceSweep
	// ticker: daily by default, 7-day retention, 1000-row batches).
	g.Go(func() error {
		ticker := time.NewTicker(cfg.OutboxPruneInterval)
		defer ticker.Stop()
		for {
			select {
			case <-gCtx.Done():
				return nil
			case <-ticker.C:
				pruned, err := outboxRunner.PrunePublished(gCtx, cfg.OutboxPruneRetention, cfg.OutboxPruneLimit)
				if err != nil {
					logger.Error("outbox prune failed", map[string]interface{}{"error": err.Error()})
					continue
				}
				logger.Info("outbox prune complete", map[string]interface{}{"rows_deleted": pruned})
			}
		}
	})
	g.Go(func() error {
		logger.Info("iam-delegation-server listening", map[string]interface{}{"addr": httpServer.Addr})
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		logger.Info("metrics server starting", map[string]interface{}{"addr": metricsServer.Addr})
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("metrics server: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		<-gCtx.Done()
		// shutdownCtx is deliberately not derived from gCtx: gCtx is already
		// Done() at this point, so a context.WithTimeout(gCtx, ...) would be
		// pre-cancelled and give the shutdown sequence below zero time to
		// run — the standard http.Server.Shutdown pattern.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		//nolint:contextcheck // see shutdownCtx's comment above
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("http server shutdown failed", map[string]interface{}{"error": err.Error()})
		}
		//nolint:contextcheck // see shutdownCtx's comment above
		if err := metricsServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("metrics server shutdown failed", map[string]interface{}{"error": err.Error()})
		}
		if err := outboxRunner.Stop(); err != nil {
			logger.Error("outbox runner shutdown failed", map[string]interface{}{"error": err.Error()})
		}
		if err := sqsConsumer.Stop(); err != nil {
			logger.Error("cascade consumer shutdown failed", map[string]interface{}{"error": err.Error()})
		}
		return nil
	})

	return g.Wait()
}
