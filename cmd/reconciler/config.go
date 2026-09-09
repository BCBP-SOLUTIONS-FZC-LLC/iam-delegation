package main

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

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

// isDevLikeEnvironment reports whether env is one of this binary's
// recognized local/dev aliases — the only environments exempt from the
// SYSTEM_DATABASE_URL fail-fast. Mirrors cmd/server's helper so staging/uat
// cannot silently reuse the RLS-scoped app DSN.
func isDevLikeEnvironment(env string) bool {
	switch env {
	case "development", "dev", "local", "test", "":
		return true
	default:
		return false
	}
}

// ensureGincommonEnv fills the env vars InitTracingFromEnv / NewLogger
// read so a CronJob that only sets ENVIRONMENT still exports traces
// under iam-delegation-reconciler, matching iam-realm-provisioner's
// reconciler (distinct service name from cmd/server).
func ensureGincommonEnv() {
	if os.Getenv("APP_ENV") == "" {
		//nolint:errcheck // os.Setenv on the current process's own env cannot fail
		_ = os.Setenv("APP_ENV", resolveAppEnv())
	}
	if os.Getenv("APP_NAME") == "" {
		//nolint:errcheck // os.Setenv on the current process's own env cannot fail
		_ = os.Setenv("APP_NAME", "iam-delegation-reconciler")
	}
	if os.Getenv("OTEL_SERVICE_NAME") == "" {
		//nolint:errcheck // os.Setenv on the current process's own env cannot fail
		_ = os.Setenv("OTEL_SERVICE_NAME", "iam-delegation-reconciler")
	}
}

func getEnvDurationMS(key string, fallback time.Duration) (time.Duration, error) {
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

func getEnvIntVar(key string, fallback int) (int, error) {
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
