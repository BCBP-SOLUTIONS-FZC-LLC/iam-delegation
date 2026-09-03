package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	pgcommon "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

type fakePostgresHealth struct {
	status pgcommon.HealthStatus
}

func (f fakePostgresHealth) Health(ctx context.Context) pgcommon.HealthStatus { return f.status }

type fakePinger struct {
	err error
}

func (f fakePinger) Ping(ctx context.Context) error { return f.err }

func TestReadyz(t *testing.T) {
	t.Run("all nil dependencies -> 200 ok, empty checks", func(t *testing.T) {
		hc := &healthHandlers{}
		c, w := newRequestWithIdentity(http.MethodGet, "/readyz", nil, nil)
		hc.readyz(c)
		require.Equal(t, http.StatusOK, w.Code)
		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		require.Equal(t, "ok", body["status"])
		require.Empty(t, body["checks"])
	})

	t.Run("postgres healthy -> 200 with pool stats", func(t *testing.T) {
		hc := &healthHandlers{postgres: fakePostgresHealth{status: pgcommon.HealthStatus{
			Healthy: true, TotalConns: 10, IdleConns: 8, AcquiredConns: 2, MaxConns: 20, Utilization: 0.1,
		}}}
		c, w := newRequestWithIdentity(http.MethodGet, "/readyz", nil, nil)
		hc.readyz(c)
		require.Equal(t, http.StatusOK, w.Code)
		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		require.Equal(t, "ok", body["status"])
		checks := body["checks"].(map[string]any)
		require.Equal(t, "ok", checks["postgres"])
		pool := checks["postgres_pool"].(map[string]any)
		require.Equal(t, float64(20), pool["max_conns"])
	})

	t.Run("postgres unhealthy -> 503 overall error", func(t *testing.T) {
		hc := &healthHandlers{postgres: fakePostgresHealth{status: pgcommon.HealthStatus{Healthy: false}}}
		c, w := newRequestWithIdentity(http.MethodGet, "/readyz", nil, nil)
		hc.readyz(c)
		require.Equal(t, http.StatusServiceUnavailable, w.Code)
		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		require.Equal(t, "error", body["status"])
		checks := body["checks"].(map[string]any)
		require.Equal(t, "error", checks["postgres"])
	})

	t.Run("cache degraded does not flip overall readiness", func(t *testing.T) {
		hc := &healthHandlers{cache: fakePinger{err: errors.New("down")}}
		c, w := newRequestWithIdentity(http.MethodGet, "/readyz", nil, nil)
		hc.readyz(c)
		require.Equal(t, http.StatusOK, w.Code)
		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		require.Equal(t, "ok", body["status"])
		checks := body["checks"].(map[string]any)
		require.Equal(t, "degraded", checks["valkey"])
	})

	t.Run("cache healthy", func(t *testing.T) {
		hc := &healthHandlers{cache: fakePinger{}}
		c, w := newRequestWithIdentity(http.MethodGet, "/readyz", nil, nil)
		hc.readyz(c)
		require.Equal(t, http.StatusOK, w.Code)
		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		checks := body["checks"].(map[string]any)
		require.Equal(t, "ok", checks["valkey"])
	})

	t.Run("outbox degraded does not flip overall readiness", func(t *testing.T) {
		hc := &healthHandlers{outbox: fakePinger{err: errors.New("down")}}
		c, w := newRequestWithIdentity(http.MethodGet, "/readyz", nil, nil)
		hc.readyz(c)
		require.Equal(t, http.StatusOK, w.Code)
		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		checks := body["checks"].(map[string]any)
		require.Equal(t, "degraded", checks["outbox"])
	})

	t.Run("outbox healthy", func(t *testing.T) {
		hc := &healthHandlers{outbox: fakePinger{}}
		c, w := newRequestWithIdentity(http.MethodGet, "/readyz", nil, nil)
		hc.readyz(c)
		require.Equal(t, http.StatusOK, w.Code)
		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		checks := body["checks"].(map[string]any)
		require.Equal(t, "ok", checks["outbox"])
	})

	t.Run("postgres unhealthy + cache degraded still 503", func(t *testing.T) {
		hc := &healthHandlers{
			postgres: fakePostgresHealth{status: pgcommon.HealthStatus{Healthy: false}},
			cache:    fakePinger{err: errors.New("down")},
		}
		c, w := newRequestWithIdentity(http.MethodGet, "/readyz", nil, nil)
		hc.readyz(c)
		require.Equal(t, http.StatusServiceUnavailable, w.Code)
		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		require.Equal(t, "error", body["status"])
		checks := body["checks"].(map[string]any)
		require.Equal(t, "error", checks["postgres"])
		require.Equal(t, "degraded", checks["valkey"])
	})

	t.Run("sys postgres unhealthy -> 503 overall error", func(t *testing.T) {
		hc := &healthHandlers{
			postgres:    fakePostgresHealth{status: pgcommon.HealthStatus{Healthy: true}},
			sysPostgres: fakePostgresHealth{status: pgcommon.HealthStatus{Healthy: false}},
		}
		c, w := newRequestWithIdentity(http.MethodGet, "/readyz", nil, nil)
		hc.readyz(c)
		require.Equal(t, http.StatusServiceUnavailable, w.Code)
		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		checks := body["checks"].(map[string]any)
		require.Equal(t, "error", checks["sys_postgres"])
	})
}
