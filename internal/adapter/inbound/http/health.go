package http

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	pgcommon "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

// Pinger is satisfied by any dependency this adapter must check for
// readiness with a plain liveness probe — the Valkey cache and the outbox
// dispatcher alike. cmd/server injects concrete implementations; a nil
// Pinger is treated as "not wired yet" and skipped rather than failing
// readiness (so this package doesn't hard-depend on the outbox package
// existing yet — DB/outbox health objects live outside this package per
// the task split).
type Pinger interface {
	Ping(ctx context.Context) error
}

// PostgresHealth is satisfied by *pgcommon.Pool. Used for the Postgres
// check specifically instead of the plain Pinger: pgcommon.Pool.Health's
// own doc comment recommends exposing it at /healthz or /readyz precisely
// because it carries pool connection stats (total/idle/acquired/max/
// utilization) alongside the liveness ping.
type PostgresHealth interface {
	Health(ctx context.Context) pgcommon.HealthStatus
}

type healthHandlers struct {
	postgres    PostgresHealth
	sysPostgres PostgresHealth
	cache       Pinger
	outbox      Pinger
}

// readyz checks Postgres (critical path for every route — 503 if
// unreachable), Valkey, and the outbox dispatcher (both degraded-only: a
// cache or outbox check failure is reported without flipping overall
// readiness, since neither blocks every route the way Postgres does).
//
// @Summary   Readiness check
// @Tags      infra
// @Produce   json
// @Success   200  {object}  map[string]interface{}
// @Failure   503  {object}  map[string]interface{}
// @Router    /readyz [get]
func (h *healthHandlers) readyz(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()

	healthy := true
	checks := gin.H{}

	if h.postgres != nil {
		hs := h.postgres.Health(ctx)
		if hs.Healthy {
			checks["postgres"] = "ok"
		} else {
			checks["postgres"] = "error"
			healthy = false
		}
		checks["postgres_pool"] = gin.H{
			"total_conns":    hs.TotalConns,
			"idle_conns":     hs.IdleConns,
			"acquired_conns": hs.AcquiredConns,
			"max_conns":      hs.MaxConns,
			"utilization":    hs.Utilization,
		}
	}

	if h.sysPostgres != nil {
		hs := h.sysPostgres.Health(ctx)
		if hs.Healthy {
			checks["sys_postgres"] = "ok"
		} else {
			checks["sys_postgres"] = "error"
			healthy = false
		}
		checks["sys_postgres_pool"] = gin.H{
			"total_conns":    hs.TotalConns,
			"idle_conns":     hs.IdleConns,
			"acquired_conns": hs.AcquiredConns,
			"max_conns":      hs.MaxConns,
			"utilization":    hs.Utilization,
		}
	}

	if h.cache != nil {
		if err := h.cache.Ping(ctx); err != nil {
			checks["valkey"] = "degraded"
		} else {
			checks["valkey"] = "ok"
		}
	}

	if h.outbox != nil {
		if err := h.outbox.Ping(ctx); err != nil {
			checks["outbox"] = "degraded"
		} else {
			checks["outbox"] = "ok"
		}
	}

	status := http.StatusOK
	overall := "ok"
	if !healthy {
		status = http.StatusServiceUnavailable
		overall = "error"
	}
	c.JSON(status, gin.H{"status": overall, "checks": checks})
}
