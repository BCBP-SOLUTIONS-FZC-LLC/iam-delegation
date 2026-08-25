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

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"go.opentelemetry.io/otel"

	events "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/outbox"
	gincommon "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	gclogger "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/logger"
	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/migrate"
	pgcommon "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"

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

func main() {
	logger, err := gclogger.NewLogger(getEnv("ENVIRONMENT", "development"))
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

	// Tracing is entirely gincommon's — ObservabilityMiddlewares lazily
	// installs the real OTel TracerProvider the first time it runs, using
	// the Tracing options below. There is no separate hand-rolled
	// TracerProvider/exporter setup in this process (LLD §14.3).
	insecure := cfg.Environment != "production"
	sampleRatio := 1.0
	if cfg.Environment == "production" {
		sampleRatio = 0.1
	}
	tracing := &gincommon.TracingOptions{
		Endpoint:    cfg.OTELExporterOTLPEndpoint,
		Insecure:    &insecure,
		SampleRatio: &sampleRatio,
	}
	tracer := otel.Tracer("iam-delegation")
	defer func() {
		if shutdownErr := gincommon.Shutdown(nil); shutdownErr != nil {
			logger.Error("telemetry shutdown failed", map[string]interface{}{"error": shutdownErr.Error()})
		}
	}()

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
	// Migrations run at startup against the delegation_migrator (BYPASSRLS)
	// role — MIGRATION_DATABASE_URL, falling back to DSNFromEnv() (LLD §7.4).
	migratorDSN := pgadapter.MigrationDSNFromEnv()
	if err := pgadapter.RunMigrations(baseCtx, migratorDSN, pgadapter.NewLoggerAdapter(logger)); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	if err := outbox.ApplySchema(baseCtx, &pgmigrate.Runner{DSN: migratorDSN}); err != nil {
		return fmt.Errorf("apply outbox schema: %w", err)
	}

	// The RLS-scoped delegation_app pool — every public-route write and
	// read goes through this pool, with app.tenant_id bound per-request by
	// tenantGUCMiddleware (router.go) or per-call by gucBoundReader.
	pgCfg.DSN = dsn
	pgCfg.GUCProvider = pgcommon.GUCSetFromContext
	pgCfg.Logger = pgadapter.NewLoggerAdapter(logger)
	pool, err := pgcommon.NewPool(baseCtx, pgCfg)
	if err != nil {
		return fmt.Errorf("connect app pool: %w", err)
	}
	//nolint:errcheck // best-effort shutdown cleanup — an error here has no recovery action at process exit.
	defer func() { _ = pool.DrainAndClose(context.Background()) }()

	// The BYPASSRLS delegation_migrator-privileged pool the cross-tenant
	// cron sweep queries and the RLS-exempt processed_events ledger need
	// (LLD §7.2.3/§11.3/§11.4/§18.4). SYSTEM_DATABASE_URL should point at a
	// role with BYPASSRLS in production; falling back to DSNFromEnv()
	// degrades the reconciler's sweeps to RLS-filtered (incomplete) rather
	// than failing startup.
	sysDSN := pgadapter.SystemDSNFromEnv()
	sysPool, err := pgcommon.NewPool(baseCtx, pgcommon.Config{
		DSN: sysDSN, Logger: pgadapter.NewLoggerAdapter(logger), Tracer: otelSpanTracer{tracer: tracer}, PGBouncerMode: true,
	})
	if err != nil {
		return fmt.Errorf("connect system pool: %w", err)
	}
	if sysDSN == dsn {
		logger.Warn("SYSTEM_DATABASE_URL not set — sysPool reuses app DSN; cross-tenant cron/internal sweeps will be RLS-filtered", nil)
	}
	//nolint:errcheck // best-effort shutdown cleanup — an error here has no recovery action at process exit.
	defer func() { _ = sysPool.DrainAndClose(context.Background()) }()

	// Event schema validation (enqueue-time) + SNS publisher w/ optional
	// Glue codec (publish-time) — the enqueue-vs-publish split, LLD §10.3.1.
	validator, err := eventbus.NewSchemaValidator()
	if err != nil {
		return fmt.Errorf("build schema validator: %w", err)
	}
	txRunner := pgadapter.NewTxRunner(pool, validator)

	var codec events.Codec = eventbus.NoopCodec{}
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

	// Metrics — iam_delegation_* counters/gauges land in gincommon's own
	// Prometheus registry (LLD §14.2), alongside its HTTP metrics.
	appMetrics, err := metrics.Register(gincommon.MetricsRegisterer())
	if err != nil {
		return fmt.Errorf("register metrics: %w", err)
	}
	events.InitWithRegisterer("iam-delegation", buildVersion, gincommon.MetricsRegisterer())

	// Outbound clients.
	userProfileClient, err := userprofile.NewHTTPClient(cfg.UserProfileBaseURL, &http.Client{Timeout: cfg.UserProfileTimeout}, cfg.UserProfileTimeout)
	if err != nil {
		return fmt.Errorf("build User Profile client: %w", err)
	}
	membershipCheckClient, err := orgmembership.NewHTTPChecker(cfg.OrgMembershipBaseURL, &http.Client{Timeout: cfg.OrgMembershipTimeout}, cfg.OrgMembershipTimeout)
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
	delegationService := service.NewDelegationService(delegationRepo, settingsRepo, membershipCheckClient, userProfileClient, idempotencyStore, cache, txRunner)
	settingsService := service.NewSettingsService(settingsRepo)
	cascadeService := service.NewCascadeService(delegationRepo, settingsRepo, userProfileClient, txRunner)

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
		pool, redisPinger{client: redisClient}, outboxPinger{runner: outboxRunner},
		logger, tracing, docs, bindTenantGUC,
	)

	httpServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           router.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Cascade consumer — Core's MembershipRevoked/TenantMembershipsPurged on
	// delegation-cascade-q (LLD §10.1/§11.5/§11.6).
	processedEvents := consumer.NewProcessedEvents(sysPool)
	cascadeConsumer := consumer.NewCascadeConsumer(cascadeService, processedEvents, pgadapter.WithTenantGUC, logger)
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
	sqsConsumer, err := consumer.NewCascadeSQSConsumer(sqsClient, cfg.CascadeQueueURL, logger, cascadeConsumer,
		events.WithConsumerCodec(eventbus.GlueDecodeCodec{}))
	if err != nil {
		return fmt.Errorf("build cascade SQS consumer: %w", err)
	}

	g, gCtx := errgroup.WithContext(baseCtx)
	g.Go(func() error { return outboxRunner.Start(gCtx) })
	g.Go(func() error { return sqsConsumer.Start(gCtx) })
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
