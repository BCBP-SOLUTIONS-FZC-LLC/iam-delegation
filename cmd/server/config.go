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

	ValkeyAddr     string
	IdempotencyTTL time.Duration
	ListCacheTTL   time.Duration

	SNSTopicARN      string
	AWSRegion        string
	AWSEndpointURL   string
	GlueRegistryName string

	CascadeQueueURL       string
	CascadeSQSConcurrency int

	// Outbox runner tunables (outbox.Config) — env-configurable to match
	// iam-org-membership's/iam-user-profile's identical OUTBOX_* surface;
	// defaults match platform-events' own library defaults where iam-org-
	// membership's are the same, and iam-org-membership's otherwise (the
	// fuller of the two sibling configs, and this service's original
	// source).
	OutboxPollInterval       time.Duration
	OutboxBatchSize          int
	OutboxMaxAttempts        int
	OutboxDrainTimeout       time.Duration
	OutboxPublishConcurrency int
	OutboxPublishTimeout     time.Duration
	OutboxStartupJitter      time.Duration
	OutboxClaimLeaseDuration time.Duration

	// Outbox prune sweep — platform-events' own outbox.Runner.PrunePublished
	// is never called without this: published outbox_events rows are never
	// deleted automatically (per pkg/outbox's own doc comment) and the table
	// grows unbounded otherwise. Mirrors iam-user-profile's runMaintenanceSweep
	// (daily ticker, 7-day retention, 1000-row batches) — iam-org-membership
	// instead hand-rolls the equivalent DELETE in a reconciler job rather than
	// calling PrunePublished; this service calls the library method directly,
	// matching user-profile and every other "pass through platform-events"
	// fix made this session.
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
	if cfg.OutboxPollInterval, err = getEnvDurationStr("OUTBOX_POLL_INTERVAL", 500*time.Millisecond); err != nil {
		return cfg, err
	}
	if cfg.OutboxBatchSize, err = getEnvInt("OUTBOX_BATCH_SIZE", 50); err != nil {
		return cfg, err
	}
	if cfg.OutboxMaxAttempts, err = getEnvInt("OUTBOX_MAX_ATTEMPTS", 5); err != nil {
		return cfg, err
	}
	if cfg.OutboxDrainTimeout, err = getEnvDurationStr("OUTBOX_DRAIN_TIMEOUT", 30*time.Second); err != nil {
		return cfg, err
	}
	if cfg.OutboxPublishConcurrency, err = getEnvInt("OUTBOX_PUBLISH_CONCURRENCY", 4); err != nil {
		return cfg, err
	}
	if cfg.OutboxPublishTimeout, err = getEnvDurationStr("OUTBOX_PUBLISH_TIMEOUT", 10*time.Second); err != nil {
		return cfg, err
	}
	if cfg.OutboxStartupJitter, err = getEnvDurationStr("OUTBOX_STARTUP_JITTER", 2*time.Second); err != nil {
		return cfg, err
	}
	if cfg.OutboxClaimLeaseDuration, err = getEnvDurationStr("OUTBOX_CLAIM_LEASE_DURATION", 10*time.Minute); err != nil {
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
	if !isDevLikeEnvironment(cfg.Environment) && os.Getenv("SYSTEM_DATABASE_URL") == "" {
		return cfg, fmt.Errorf("SYSTEM_DATABASE_URL is required outside local/dev environments (ENVIRONMENT=%q; must be the BYPASSRLS delegation_migrator role, see .claude/database.md)", cfg.Environment)
	}
	return cfg, nil
}

// isDevLikeEnvironment reports whether env is one of this service's
// recognized local/dev aliases (matching resolveAppEnv's switch below) — the
// only environments exempt from the SYSTEM_DATABASE_URL fail-fast above.
func isDevLikeEnvironment(env string) bool {
	switch env {
	case "development", "dev", "local", "":
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
// millisecond integer. Used for the OUTBOX_* tunables, matching
// iam-org-membership's/iam-user-profile's identical env-var format for
// those same names.
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
