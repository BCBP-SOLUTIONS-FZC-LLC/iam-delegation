// Metric exporter goroutines started from main.go. Each polls the
// BYPASSRLS sysPool on an interval and refreshes one DB-state gauge.
// Follows iam-realm-provisioner's cmd/server/exporters.go: exporters run
// as goroutines in the server pod, not as CronJobs, so the pod that
// serves /metrics is also the one that populates them. The SQL itself
// lives in the postgres adapter (pgadapter.GaugeRepository) — no SQL is
// written outside internal/adapter/outbound/postgres.
package main

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/metrics"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/adapter/outbound/postgres"
)

const exporterInterval = 5 * time.Minute

// runActiveGaugeExporter keeps iam_delegation_active_gauge{tenant} in
// sync with current table state. Before this existed the collector was
// registered but permanently empty (DLG-D30/D31). Emits once at start so
// the gauge is populated before the first Prometheus scrape, then ticks
// every exporterInterval until ctx is canceled at shutdown. Query
// failures are logged at WARN and the previous value is retained — a
// stale gauge beats a crashed pod.
func runActiveGaugeExporter(ctx context.Context, gauges *pgadapter.GaugeRepository, log Logger, pub *metrics.Metrics) {
	tick(ctx, "iam_delegation_active_gauge", log, func() error {
		counts, err := gauges.CountActiveByTenant(ctx)
		if err != nil {
			return err
		}
		pub.ReplaceActiveGauges(counts)
		return nil
	})
}

// tick runs emit once, then on exporterInterval until ctx is canceled.
func tick(ctx context.Context, gauge string, log Logger, emit func() error) {
	run := func() {
		if err := emit(); err != nil {
			log.Warn("gauge exporter query failed", map[string]interface{}{
				"gauge": gauge, "error": err.Error(),
			})
		}
	}
	run() // populate before the first scrape
	ticker := time.NewTicker(exporterInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}
