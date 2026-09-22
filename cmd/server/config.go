package main

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// config holds every environment-driven setting this binary needs beyond
// what pgcommon.ConfigFromEnv/platform-events already load themselves (LLD
// §15). DATABASE_URL/MIGRATION_DATABASE_URL/SYSTEM_DATABASE_URL are read
// directly by the postgres package; everything else lives here.
type config struct {
	Environment string
	Port        string
	MetricsPort string

	UserProfileBaseURL   string
	UserProfileTimeout   time.Duration
	OrgMembershipBaseURL string
	OrgMembershipTimeout time.Duration
	CatalogAdminBaseURL  string // optional — empty disables department scope_id validation (GAP-020)
	CatalogAdminTimeout  time.Duration

	ValkeyAddr     string
	IdempotencyTTL time.Duration
	ListCacheTTL   time.Duration

	SNSTopicARN      string
	AWSRegion        string
	AWSEndpointURL   string
	GlueRegistryName string

	CascadeQueueURL       string
	CascadeSQSConcurrency int

	// Outbox runner tunables come from platform-events config.LoadOutbox
	// (OUTBOX_*), mapped via RunnerConfigFromEnv — matching iam-user-profile
	// / iam-org-membership. Helm / .env.example keep the historical 500ms /
	// concurrency-4 / 2s jitter / 10m claim-lease values; library defaults
	// apply only when those env vars are unset.

	// Outbox prune sweep — platform-events' own outbox.Runner.PrunePublished
	// is never called without this: published outbox_events rows are never
	// deleted automatically (per pkg/outbox's own doc comment) and the table
	// grows unbounded otherwise. Mirrors iam-user-profile's runMaintenanceSweep
	// (daily ticker, 7-day retention, 1000-row batches). LoadOutbox does not
	// cover prune, so these stay local.
	OutboxPruneInterval  time.Duration
	OutboxPruneRetention time.Duration
	OutboxPruneLimit     int

	OTELExporterOTLPEndpoint string

	DocsEnabled   bool
	DocsAuthToken string

	PolicyDefaultMaxDurationDays  int
	PolicyDefaultReviewWindowDays int
}

func loadConfig() (config, error) {
	cfg := config{
		Environment:          getEnv("ENVIRONMENT", "development"),
		Port:                 getEnv("PORT", "8080"),
		MetricsPort:          getEnv("METRICS_PORT", "9090"),
		UserProfileBaseURL:   os.Getenv("USER_PROFILE_BASE_URL"),
		OrgMembershipBaseURL: os.Getenv("ORG_MEMBERSHIP_BASE_URL"),
		CatalogAdminBaseURL:  os.Getenv("CATALOG_ADMIN_BASE_URL"),
		ValkeyAddr:           getEnv("VALKEY_ADDR", "localhost:6379"),
		SNSTopicARN:          os.Getenv("SNS_TOPIC_ARN"),
		AWSRegion:            getEnv("AWS_REGION", "ap-south-1"),
		AWSEndpointURL:       os.Getenv("AWS_ENDPOINT_URL"),
		GlueRegistryName:     os.Getenv("GLUE_REGISTRY_NAME"),
		CascadeQueueURL:      os.Getenv("CASCADE_QUEUE_URL"),

		OTELExporterOTLPEndpoint: os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),

		DocsAuthToken: os.Getenv("DOCS_AUTH_TOKEN"),
	}

	var err error
	if cfg.UserProfileTimeout, err = getEnvDuration("USER_PROFILE_TIMEOUT_MS", 3000*time.Millisecond); err != nil {
		return cfg, err
	}
	if cfg.OrgMembershipTimeout, err = getEnvDuration("ORG_MEMBERSHIP_MEMBERSHIP_CHECK_TIMEOUT_MS", 3000*time.Millisecond); err != nil {
		return cfg, err
	}
	if cfg.CatalogAdminTimeout, err = getEnvDuration("CATALOG_ADMIN_TIMEOUT_MS", 3000*time.Millisecond); err != nil {
		return cfg, err
	}
	if cfg.IdempotencyTTL, err = getEnvDurationSeconds("IDEMPOTENCY_TTL_SECONDS", 86400*time.Second); err != nil {
		return cfg, err
	}
	if cfg.ListCacheTTL, err = getEnvDurationSeconds("LIST_CACHE_TTL_SECONDS", 60*time.Second); err != nil {
		return cfg, err
	}
	if cfg.DocsEnabled, err = getEnvBool("DOCS_ENABLED", cfg.Environment != "production"); err != nil {
		return cfg, err
	}
	if cfg.PolicyDefaultMaxDurationDays, err = getEnvInt("POLICY_DEFAULT_MAX_DURATION_DAYS", 90); err != nil {
		return cfg, err
	}
	if cfg.PolicyDefaultReviewWindowDays, err = getEnvInt("POLICY_DEFAULT_REVIEW_WINDOW_DAYS", 90); err != nil {
		return cfg, err
	}
	if cfg.OutboxPruneInterval, err = getEnvDurationStr("OUTBOX_PRUNE_INTERVAL", 24*time.Hour); err != nil {
		return cfg, err
	}
	if cfg.OutboxPruneRetention, err = getEnvDurationStr("OUTBOX_PRUNE_RETENTION", 7*24*time.Hour); err != nil {
		return cfg, err
	}
	if cfg.OutboxPruneLimit, err = getEnvInt("OUTBOX_PRUNE_LIMIT", 1000); err != nil {
		return cfg, err
	}
	if cfg.CascadeSQSConcurrency, err = getEnvInt("CASCADE_SQS_CONCURRENCY", 4); err != nil {
		return cfg, err
	}

	// DATABASE_URL (or the split PG_HOST/PG_USER/PG_PASSWORD form
	// pgcommon.ConfigFromEnv also accepts) is required — matching
	// iam-realm-provisioner's / iam-org-membership's validateRequiredEnv.
	if os.Getenv("DATABASE_URL") == "" &&
		(os.Getenv("PG_HOST") == "" || os.Getenv("PG_USER") == "" || os.Getenv("PG_PASSWORD") == "") {
		return cfg, fmt.Errorf("DATABASE_URL (or PG_HOST + PG_USER + PG_PASSWORD) is required")
	}
	// Migrations acquire a session-scoped pg_advisory_lock; under
	// transaction pooling they must bypass PgBouncer via
	// MIGRATION_DATABASE_URL. Sibling validateRequiredEnv only fires when
	// DATABASE_URL is also empty, which never happens in Helm — require
	// the override whenever PG_BOUNCER_MODE=true.
	if os.Getenv("PG_BOUNCER_MODE") == "true" && os.Getenv("MIGRATION_DATABASE_URL") == "" {
		return cfg, fmt.Errorf("MIGRATION_DATABASE_URL is required when PG_BOUNCER_MODE=true — migrations must bypass PgBouncer")
	}

	// Fail-fast on empty base URLs for the two data-bearing outbound clients
	// (LLD §15 "base URLs required, fail-fast on empty") — deferred to the
	// client constructors themselves (userprofile.NewHTTPClient /
	// orgmembership.NewHTTPChecker already return an error on "", so run()
	// surfaces it there rather than duplicating the check here.
	if cfg.SNSTopicARN == "" {
		return cfg, fmt.Errorf("SNS_TOPIC_ARN is required")
	}
	if cfg.CascadeQueueURL == "" {
		return cfg, fmt.Errorf("CASCADE_QUEUE_URL is required")
	}

	// Fail fast outside local/dev rather than let pgadapter.SystemDSNFromEnv's
	// dev-safe fallback (SYSTEM_DATABASE_URL unset -> reuse the RLS-scoped
	// app DSN) degrade silently: the reconciler's cross-tenant sweeps
	// (ListExpiringBefore/FindDueForDailyWarn/FindDueForAutoEnd/
	// HardPurgeSoftDeletedBefore) and the active-gauge exporter would then
	// run under RLS with no tenant GUC bound, so every query returns zero
	// rows instead of erroring — no alert fires, since that's a different
	// failure mode than the existing "deferred" counters/alerts cover.
	//
	// Originally gated on a literal ENVIRONMENT=="production" (DLG-D34);
	// widened to isDevLikeEnvironment after a production-readiness review
	// found any other environment name (staging, uat, ...) would silently
	// skip this check and hit the same degrade-with-no-alert failure mode.
	// Keyed off resolveAppEnv() (APP_ENV, then ENVIRONMENT) so a deploy
	// that only sets APP_ENV=production cannot skip this the way a bare
	// ENVIRONMENT default of "development" would — matching
	// iam-org-membership's validateRequiredEnv / reconciler isDevLikeEnv.
	appEnv := resolveAppEnv()
	if !isDevLikeEnvironment(appEnv) && os.Getenv("SYSTEM_DATABASE_URL") == "" {
		return cfg, fmt.Errorf("SYSTEM_DATABASE_URL is required outside local/dev environments (APP_ENV=%q; must be the BYPASSRLS delegation_migrator role, see .claude/database.md)", appEnv)
	}

	// registerDocsRoutes (internal/adapter/inbound/http/router.go) only puts
	// docsAuthMiddleware in front of /swagger and /asyncapi when
	// Environment=="production" AND AuthToken!="" — so DOCS_ENABLED=true
	// with DOCS_AUTH_TOKEN unset in production serves both with zero auth.
	// Fail fast here instead of letting that combination reach the router.
	if cfg.Environment == "production" && cfg.DocsEnabled && cfg.DocsAuthToken == "" {
		return cfg, fmt.Errorf("DOCS_AUTH_TOKEN is required when DOCS_ENABLED=true in production")
	}
	return cfg, nil
}

// isDevLikeEnvironment reports whether env is one of this service's
// recognized local/dev aliases — the only environments exempt from the
// SYSTEM_DATABASE_URL fail-fast above. Includes "test" so CI (APP_ENV=test)
// matches iam-org-membership's isDevLikeEnv.
func isDevLikeEnvironment(env string) bool {
	switch env {
	case "development", "dev", "local", "test", "":
		return true
	default:
		return false
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// resolveAppEnv matches iam-realm-provisioner's APP_ENV contract (the
// value platform-gincommon's InitTracingFromEnv / logger.NewLogger read)
// while still honoring this service's existing ENVIRONMENT var.
func resolveAppEnv() string {
	if v := os.Getenv("APP_ENV"); v != "" {
		return v
	}
	if isDevLikeEnvironment(getEnv("ENVIRONMENT", "development")) {
		return "dev"
	}
	return getEnv("ENVIRONMENT", "production")
}

// ensureGincommonEnv fills the env vars InitTracingFromEnv / NewLogger
// read so a deployment that only sets ENVIRONMENT still gets the same
// OTLP service name, sample ratio, and version labels as iam-realm-provisioner.
func ensureGincommonEnv(version string) {
	if os.Getenv("APP_ENV") == "" {
		//nolint:errcheck // os.Setenv on the current process's own env cannot fail
		_ = os.Setenv("APP_ENV", resolveAppEnv())
	}
	if os.Getenv("APP_NAME") == "" {
		//nolint:errcheck // os.Setenv on the current process's own env cannot fail
		_ = os.Setenv("APP_NAME", "iam-delegation")
	}
	if os.Getenv("OTEL_SERVICE_NAME") == "" {
		//nolint:errcheck // os.Setenv on the current process's own env cannot fail
		_ = os.Setenv("OTEL_SERVICE_NAME", "iam-delegation")
	}
	if os.Getenv("BUILD_VERSION") == "" && version != "" {
		//nolint:errcheck // os.Setenv on the current process's own env cannot fail
		_ = os.Setenv("BUILD_VERSION", version)
	}
}

// ensureOutboxEnv backfills this service's historical outbox tunables
// (500ms poll / concurrency-4 / 2s jitter / 10m claim-lease) whenever the
// corresponding OUTBOX_* var is unset, before eventcfg.LoadOutbox() reads
// them. platform-events' own library defaults for these four (5s poll /
// concurrency-1 / 0 jitter / 0 claim-lease) differ from this service's
// original hardcoded Go defaults; Helm's values.yaml and .env.example both
// set the historical values explicitly, but any other invocation path
// (a bare `go run`, a test binary, a manual container run) would otherwise
// silently regress to the library's slower/less-concurrent defaults with
// no error — the same class of silent-degrade risk SYSTEM_DATABASE_URL's
// fail-fast above exists to avoid. Mirrors ensureGincommonEnv's pattern.
func ensureOutboxEnv() {
	defaults := map[string]string{
		"OUTBOX_POLL_INTERVAL":        "500ms",
		"OUTBOX_STARTUP_JITTER":       "2s",
		"OUTBOX_PUBLISH_CONCURRENCY":  "4",
		"OUTBOX_CLAIM_LEASE_DURATION": "10m",
	}
	for key, val := range defaults {
		if os.Getenv(key) == "" {
			//nolint:errcheck // os.Setenv on the current process's own env cannot fail
			_ = os.Setenv(key, val)
		}
	}
}

func getEnvDuration(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	ms, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// getEnvDurationStr parses key as a Go duration string (e.g. "500ms", "5s",
// "10m") — distinct from getEnvDuration, which treats its env var as a bare
// millisecond integer. Used for OUTBOX_PRUNE_* (LoadOutbox does not cover
// prune); runner tunables go through platform-events config.LoadOutbox.
func getEnvDurationStr(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

func getEnvDurationSeconds(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	s, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return time.Duration(s) * time.Second, nil
}

func getEnvBool(key string, fallback bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
}

func getEnvInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}
