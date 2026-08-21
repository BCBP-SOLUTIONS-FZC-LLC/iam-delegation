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

	CascadeQueueURL string

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
		UserProfileBaseURL:   os.Getenv("USER_PROFILE_BASE_URL"),
		OrgMembershipBaseURL: os.Getenv("ORG_MEMBERSHIP_BASE_URL"),
		ValkeyAddr:           getEnv("VALKEY_ADDR", "localhost:6379"),
		SNSTopicARN:          os.Getenv("SNS_TOPIC_ARN"),
		AWSRegion:            getEnv("AWS_REGION", "us-east-1"),
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
	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
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
