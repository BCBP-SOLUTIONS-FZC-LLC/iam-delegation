// Package metrics registers every Prometheus instrument this service emits,
// organized by the Enterprise Platform Observability Standard three-tier taxonomy:
//
//   - Tier 1 (platform_*): cross-domain metrics emitted identically by IAM,
//     Billing, Workflow, Tender Management, etc.  Required labels: domain,
//     service, environment.  Injected centrally via a WrapRegistererWith wrapper
//     so instrumentation code cannot omit or misspell them.
//
//   - Tier 3 (iam_delegation_*): service-specific business metrics that have no
//     cross-service semantic equivalent.  Required labels: service, environment
//     (added to gincommon's existing {service, version} set).
//
// Tier 2 (iam_*) domain-shared metrics are not currently emitted by this service
// — all business signals either qualify as platform-shared or are delegation-
// specific.
//
// Generic per-request HTTP metrics (http_requests_total / http_request_duration_
// seconds) are emitted by platform-gincommon.  platform-events' consumer/outbox
// emit their own events_*/sqs_*/outbox_* series.  Everything below is a metric
// neither of those has an equivalent for: business-level lifecycle (create/end/
// review), cascade message pipeline, and dependency health.
//
// Backward compatibility: every legacy iam_delegation_cascade_* and
// iam_delegation_processed_events_duplicates_total metric continues to be emitted
// alongside the new platform_* equivalents during the compatibility period.
// Remove the legacy metrics once dashboards and on-call runbooks have migrated.
package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

// RegisterConfig carries runtime-supplied context for central label injection.
// Pass it to Register / RegisterOn before the /metrics endpoint is served.
type RegisterConfig struct {
	// Environment is the deployment environment value (e.g. "production",
	// "staging", "development") — injected as an environment const label on
	// every collector so Prometheus queries can filter by environment without
	// relying on external relabelling.  Sourced from ENVIRONMENT / APP_ENV.
	Environment string
}

// gincommonLabels returns a copy of gincommon's {service, version} const
// labels so business collectors scrape on the same registry and labels as
// HTTP metrics. Returns nil when ObservabilityMiddlewares has not run yet,
// matching prometheus's "no const labels" zero value so unit tests that
// never bootstrap gincommon still register cleanly.
func gincommonLabels() prometheus.Labels {
	labels := gincommon.MetricsConstLabels()
	if len(labels) == 0 {
		return nil
	}
	return labels
}

// extendedServiceLabels returns gincommon's {service, version} labels plus
// an environment entry — the full Tier 3 const-label set.  Returns a new
// map; never mutates gincommon's own label map.
func extendedServiceLabels(env string) prometheus.Labels {
	base := gincommonLabels()
	out := make(prometheus.Labels, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	if env != "" {
		out["environment"] = env
	}
	return out
}

// Live is the process-wide recorder after Register. Nil until Register
// runs (and in unit tests that never bootstrap metrics). Adapters that
// cannot take a constructor parameter (HTTP handlers, outbound clients)
// read this — matching iam-realm-provisioner's package-level collectors.
var Live *Metrics

var (
	registerOnce sync.Once
	registerErr  error
)

// Metrics holds every instrument this service emits, grouped by tier.
//
// Tier 1 fields (platform_*) are CounterVecs registered on a
// prometheus.WrapRegistererWith wrapper that injects {domain, service,
// environment} automatically.
//
// Tier 3 fields (iam_delegation_*) carry {service, version, environment}
// as ConstLabels.  The legacy iam_delegation_cascade_* and
// iam_delegation_processed_events_duplicates_total fields remain for the
// backward-compatibility period; see each field's comment.
type Metrics struct {
	// ── Tier 1: platform_* ────────────────────────────────────────────────

	// platform_messages_received_total — every message that enters the
	// cascade consumer's Handle(), including duplicates, unknowns, and
	// malformed envelopes.  Labels (beyond const): queue.
	platformMessagesReceivedTotal *prometheus.CounterVec

	// platform_messages_processed_total — messages fully handled without
	// error (success path at the end of Handle()).  Labels: queue.
	// Dual-emitted alongside legacy iam_delegation_cascade_processed_total.
	platformMessagesProcessedTotal *prometheus.CounterVec

	// platform_messages_failed_total — messages where the handler returned
	// an error, causing SQS to redeliver (up to maxReceiveCount times before
	// the queue's redrive policy routes the message to the DLQ).  Labels:
	// queue.  Dual-emitted alongside legacy iam_delegation_cascade_dlq_total.
	//
	// Note: this counts individual processing-failure occurrences, not
	// distinct DLQ arrivals.  True DLQ-arrival counts require infrastructure-
	// level measurement (e.g. CloudWatch SQS DLQ depth) and are outside the
	// scope of application-side instrumentation.
	platformMessagesFailedTotal *prometheus.CounterVec

	// platform_duplicate_messages_total — SQS redeliveries skipped because
	// processed_events already recorded the envelope (IDEMP-4).  Labels:
	// queue.  Dual-emitted alongside legacy
	// iam_delegation_processed_events_duplicates_total.
	//
	// REGISTRY-PROPOSED: this metric name requires Platform Observability
	// Registry ratification before broad adoption across other services.
	// Emitted here under the standard's pre-ratification guidance for
	// services already in development (§Registry Ratification Requirement).
	platformDuplicateMessagesTotal *prometheus.CounterVec

	// ── Tier 3: iam_delegation_* ─────────────────────────────────────────

	createdTotal                 *prometheus.CounterVec
	endedTotal                   *prometheus.CounterVec
	activeGauge                  *prometheus.GaugeVec
	expiryDeferredTotal          prometheus.Counter
	activationDeferredTotal      prometheus.Counter
	reviewDeferredTotal          prometheus.Counter
	reviewWarnedTotal            *prometheus.CounterVec
	reviewExpiredTotal           prometheus.Counter
	membershipCheckDuration      prometheus.Histogram
	membershipCheckFailuresTotal prometheus.Counter
	upAvailabilityFailuresTotal  *prometheus.CounterVec
	idempotencyHitsTotal         prometheus.Counter

	// Legacy Tier 3 — kept for backward compatibility during the migration
	// period; will be removed once dashboards and on-call runbooks migrate to
	// the platform_* equivalents above.
	cascadeProcessedTotal          prometheus.Counter
	cascadeDLQTotal                prometheus.Counter
	processedEventsDuplicatesTotal *prometheus.CounterVec

	unknownEventAcknowledgedTotal *prometheus.CounterVec
}

// Register wires all instruments onto gincommon's Prometheus registerer
// (the same registry and {service, version} const labels as HTTP metrics).
// Call once at startup AFTER ObservabilityMiddlewares has run and BEFORE
// the /metrics endpoint is served. Idempotent — only the first call's cfg
// is used; subsequent calls return the same *Metrics.
func Register(cfg RegisterConfig) *Metrics {
	registerOnce.Do(func() {
		Live, registerErr = registerOn(gincommon.MetricsRegisterer(), cfg)
	})
	if registerErr != nil {
		panic(registerErr)
	}
	return Live
}

// RegisterOn builds and registers all instruments onto an isolated
// registerer. Tests that must not pollute the process-wide gincommon
// registry use this; composition roots call Register().
func RegisterOn(reg prometheus.Registerer, cfg RegisterConfig) (*Metrics, error) {
	return registerOn(reg, cfg)
}

// platformCascadeQueue is the logical name of the single inbound SQS queue
// consumed by this service.  Used as the pre-initialization label value for
// platform_messages_* metrics so dashboards show 0 rather than "no data"
// before the first message is received.
const platformCascadeQueue = "delegation-cascade-q"

func registerOn(reg prometheus.Registerer, cfg RegisterConfig) (*Metrics, error) {
	// Tier 3: {service, version} from gincommon + environment.
	svcLabels := extendedServiceLabels(cfg.Environment)

	// Tier 1: {domain, service, environment} injected via wrapper so every
	// platform_* collector picks them up without repeating them in ConstLabels.
	// version is intentionally absent — not an approved label for platform_*
	// metrics per the Enterprise Platform Observability Standard.
	platformReg := prometheus.WrapRegistererWith(prometheus.Labels{
		"domain":      "iam",
		"service":     "iam-delegation",
		"environment": cfg.Environment,
	}, reg)

	m := &Metrics{
		// ── Tier 1: platform_* ────────────────────────────────────────────

		platformMessagesReceivedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "platform_messages_received_total",
				Help: "Total messages received by the cascade consumer Handle() entry point, regardless of outcome (success, duplicate, unknown type, error). Labels: queue.",
			},
			[]string{"queue"},
		),
		platformMessagesProcessedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "platform_messages_processed_total",
				Help: "Total messages processed successfully by the cascade consumer (MembershipRevoked/TenantMembershipsPurged/UserUpdated{disabled}). Labels: queue.",
			},
			[]string{"queue"},
		),
		platformMessagesFailedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "platform_messages_failed_total",
				Help: "Total cascade consumer processing failures (handler returned error; SQS will redeliver). Does not count actual DLQ arrivals — use SQS CloudWatch for DLQ-depth monitoring. Labels: queue.",
			},
			[]string{"queue"},
		),
		platformDuplicateMessagesTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				// REGISTRY-PROPOSED — requires Platform Observability Registry
				// ratification before adoption by other services.
				Name: "platform_duplicate_messages_total",
				Help: "Total SQS redeliveries skipped because processed_events already recorded the envelope (IDEMP-4). Labels: queue.",
			},
			[]string{"queue"},
		),

		// ── Tier 3: iam_delegation_* ──────────────────────────────────────

		createdTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name:        "iam_delegation_created_total",
				Help:        "Total delegations created (DLG-2), labeled by scope (all/department/tender).",
				ConstLabels: svcLabels,
			},
			[]string{"scope"},
		),
		endedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name:        "iam_delegation_ended_total",
				Help:        "Total delegations ended, labeled by ended_reason (expired/cancelled/reassigned/delegate_removed/review_expired/delegate_disabled).",
				ConstLabels: svcLabels,
			},
			[]string{"ended_reason"},
		),
		activeGauge: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name:        "iam_delegation_active_gauge",
				Help:        "Current count of active delegations, labeled by tenant.",
				ConstLabels: svcLabels,
			},
			[]string{"tenant"},
		),
		expiryDeferredTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name:        "iam_delegation_expiry_deferred_total",
				Help:        "Total DEL-6 expiry auto-end cron passes deferred because iam-user-profile was unavailable.",
				ConstLabels: svcLabels,
			},
		),
		activationDeferredTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name:        "iam_delegation_activation_deferred_total",
				Help:        "Total DLG-D25 activation cron passes deferred because iam-user-profile was unavailable.",
				ConstLabels: svcLabels,
			},
		),
		reviewDeferredTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name:        "iam_delegation_review_deferred_total",
				Help:        "Total DLG-Q5/DLG-D6 review auto-end cron passes deferred because iam-user-profile was unavailable.",
				ConstLabels: svcLabels,
			},
		),
		reviewWarnedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name:        "iam_delegation_review_warned_total",
				Help:        "Total DLG-Q6 3-day daily-cascade review notices fired, labeled by days_remaining (3, 2, or 1).",
				ConstLabels: svcLabels,
			},
			[]string{"days_remaining"},
		),
		reviewExpiredTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name:        "iam_delegation_review_expired_total",
				Help:        "Total delegations auto-ended by the review-window cron (DLG-D6/DLG-Q5, ended_reason=review_expired).",
				ConstLabels: svcLabels,
			},
		),
		membershipCheckDuration: prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name:        "iam_delegation_membership_check_duration_seconds",
				Help:        "Latency of Core's grant-time membership-existence check (LLD §7.6.2).",
				Buckets:     prometheus.DefBuckets,
				ConstLabels: svcLabels,
			},
		),
		membershipCheckFailuresTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name:        "iam_delegation_membership_check_failures_total",
				Help:        "Total grant-time membership-existence checks that failed (network/timeout/5xx).",
				ConstLabels: svcLabels,
			},
		),
		upAvailabilityFailuresTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name:        "iam_delegation_up_availability_failures_total",
				Help:        "Total iam-user-profile SetAvailability call failures, labeled by path (create/cancel/extend/reassign/cascade/expiry-cron/review-cron/activation-cron).",
				ConstLabels: svcLabels,
			},
			[]string{"path"},
		),
		idempotencyHitsTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name:        "iam_delegation_idempotency_hits_total",
				Help:        "Total DLG-2 create calls short-circuited by a create-idempotency-key replay (DLG-Q3).",
				ConstLabels: svcLabels,
			},
		),

		// Legacy Tier 3 — dual-emitted alongside platform_messages_*
		// equivalents during the compatibility period.
		cascadeProcessedTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name:        "iam_delegation_cascade_processed_total",
				Help:        "DEPRECATED — use platform_messages_processed_total{queue=\"delegation-cascade-q\"} instead. Total inbound MembershipRevoked/TenantMembershipsPurged cascade messages processed successfully.",
				ConstLabels: svcLabels,
			},
		),
		cascadeDLQTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name:        "iam_delegation_cascade_dlq_total",
				Help:        "DEPRECATED — use platform_messages_failed_total{queue=\"delegation-cascade-q\"} instead. Total inbound cascade messages where the handler returned an error (routed to DLQ by SQS redrive after maxReceiveCount retries).",
				ConstLabels: svcLabels,
			},
		),
		processedEventsDuplicatesTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name:        "iam_delegation_processed_events_duplicates_total",
				Help:        "DEPRECATED — use platform_duplicate_messages_total{queue=\"delegation-cascade-q\"} instead. Total inbound SQS redeliveries skipped because processed_events already recorded the envelope (IDEMP-4).",
				ConstLabels: svcLabels,
			},
			[]string{"consumer"},
		),

		unknownEventAcknowledgedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name:        "iam_delegation_unknown_event_acknowledged_total",
				Help:        "Total inbound SQS events with no wired handler, silently acked and marked processed (forward-compat).",
				ConstLabels: svcLabels,
			},
			[]string{"consumer", "event_type"},
		),
	}

	// Register Tier 1 collectors on the platform-wrapped registerer so they
	// pick up {domain, service, environment} without having those in ConstLabels.
	platformCollectors := []prometheus.Collector{
		m.platformMessagesReceivedTotal,
		m.platformMessagesProcessedTotal,
		m.platformMessagesFailedTotal,
		m.platformDuplicateMessagesTotal,
	}
	for _, c := range platformCollectors {
		if err := platformReg.Register(c); err != nil {
			return nil, err
		}
	}

	// Register Tier 3 collectors on the base registerer (ConstLabels already
	// carry the required label set).
	svcCollectors := []prometheus.Collector{
		m.createdTotal, m.endedTotal, m.activeGauge,
		m.expiryDeferredTotal, m.activationDeferredTotal, m.reviewDeferredTotal,
		m.reviewWarnedTotal, m.reviewExpiredTotal,
		m.membershipCheckDuration, m.membershipCheckFailuresTotal,
		m.upAvailabilityFailuresTotal, m.idempotencyHitsTotal,
		m.cascadeProcessedTotal, m.cascadeDLQTotal,
		m.processedEventsDuplicatesTotal, m.unknownEventAcknowledgedTotal,
	}
	for _, c := range svcCollectors {
		if err := reg.Register(c); err != nil {
			return nil, err
		}
	}

	Live = m

	// Pre-initialize known label values so dashboards show 0 rather than
	// "no data" before the first event (mirrors iam-catalog-admin/
	// iam-tender-acl's identical convention).

	// Tier 1 — queue label
	for _, q := range []string{platformCascadeQueue} {
		m.platformMessagesReceivedTotal.WithLabelValues(q)
		m.platformMessagesProcessedTotal.WithLabelValues(q)
		m.platformMessagesFailedTotal.WithLabelValues(q)
		m.platformDuplicateMessagesTotal.WithLabelValues(q)
	}

	// Tier 3 — variable labels
	for _, scope := range []string{"all", "department", "tender"} {
		m.createdTotal.WithLabelValues(scope)
	}
	for _, reason := range []string{"expired", "cancelled", "reassigned", "delegate_removed", "review_expired", "delegate_disabled"} {
		m.endedTotal.WithLabelValues(reason)
	}
	for _, days := range []string{"3", "2", "1"} {
		m.reviewWarnedTotal.WithLabelValues(days)
	}
	for _, consumer := range []string{"cascade", "offboarding", "delegate_disable"} {
		m.processedEventsDuplicatesTotal.WithLabelValues(consumer)
	}

	return m, nil
}

// ── Tier 1 recorders ─────────────────────────────────────────────────────────

// RecordMessageReceived increments platform_messages_received_total for the
// given queue.  Call at the very start of Handle() — before any dedup, decode,
// or error path — so every SQS delivery is counted regardless of outcome.
func (m *Metrics) RecordMessageReceived(queue string) {
	m.platformMessagesReceivedTotal.WithLabelValues(queue).Inc()
}

// ── Tier 3 recorders ─────────────────────────────────────────────────────────

// RecordCreated increments iam_delegation_created_total for a DLG-2 create,
// tagged by scope.
func (m *Metrics) RecordCreated(scope string) {
	m.createdTotal.WithLabelValues(scope).Inc()
}

// RecordEnded increments iam_delegation_ended_total, tagged by ended_reason.
func (m *Metrics) RecordEnded(reason string) {
	m.endedTotal.WithLabelValues(reason).Inc()
}

// ReplaceActiveGauges republishes iam_delegation_active_gauge from a
// full-table snapshot. Reset first so a tenant that dropped to zero
// active delegations disappears instead of lingering at its last count
// — CountActiveByTenant's GROUP BY omits zero-count tenants.
func (m *Metrics) ReplaceActiveGauges(counts map[string]int64) {
	m.activeGauge.Reset()
	for tenant, n := range counts {
		m.activeGauge.WithLabelValues(tenant).Set(float64(n))
	}
}

// RecordExpiryDeferred increments iam_delegation_expiry_deferred_total.
func (m *Metrics) RecordExpiryDeferred() {
	m.expiryDeferredTotal.Inc()
}

// RecordActivationDeferred increments iam_delegation_activation_deferred_total.
func (m *Metrics) RecordActivationDeferred() {
	m.activationDeferredTotal.Inc()
}

// RecordReviewDeferred increments iam_delegation_review_deferred_total.
func (m *Metrics) RecordReviewDeferred() {
	m.reviewDeferredTotal.Inc()
}

// RecordReviewWarned increments iam_delegation_review_warned_total, tagged
// by daysRemaining ("3", "2", or "1").
func (m *Metrics) RecordReviewWarned(daysRemaining string) {
	m.reviewWarnedTotal.WithLabelValues(daysRemaining).Inc()
}

// RecordReviewExpired increments iam_delegation_review_expired_total.
func (m *Metrics) RecordReviewExpired() {
	m.reviewExpiredTotal.Inc()
}

// ObserveMembershipCheckDuration records
// iam_delegation_membership_check_duration_seconds for one Core
// membership-existence check.
func (m *Metrics) ObserveMembershipCheckDuration(seconds float64) {
	m.membershipCheckDuration.Observe(seconds)
}

// RecordMembershipCheckFailure increments
// iam_delegation_membership_check_failures_total.
func (m *Metrics) RecordMembershipCheckFailure() {
	m.membershipCheckFailuresTotal.Inc()
}

// RecordUPAvailabilityFailure increments
// iam_delegation_up_availability_failures_total, tagged by path.
func (m *Metrics) RecordUPAvailabilityFailure(path string) {
	m.upAvailabilityFailuresTotal.WithLabelValues(path).Inc()
}

// RecordIdempotencyHit increments iam_delegation_idempotency_hits_total.
func (m *Metrics) RecordIdempotencyHit() {
	m.idempotencyHitsTotal.Inc()
}

// RecordCascadeProcessed increments platform_messages_processed_total (Tier 1)
// and the legacy iam_delegation_cascade_processed_total (compatibility period).
func (m *Metrics) RecordCascadeProcessed() {
	m.platformMessagesProcessedTotal.WithLabelValues(platformCascadeQueue).Inc()
	m.cascadeProcessedTotal.Inc()
}

// RecordCascadeDLQ increments platform_messages_failed_total (Tier 1) and the
// legacy iam_delegation_cascade_dlq_total (compatibility period).
func (m *Metrics) RecordCascadeDLQ() {
	m.platformMessagesFailedTotal.WithLabelValues(platformCascadeQueue).Inc()
	m.cascadeDLQTotal.Inc()
}

// RecordProcessedEventsDuplicate increments platform_duplicate_messages_total
// (Tier 1, registry-proposed) and the legacy
// iam_delegation_processed_events_duplicates_total (compatibility period).
// consumer is the processed_events bucket name (cascade/offboarding/
// delegate_disable) and is preserved on the legacy metric only; the platform
// metric uses the queue label instead.
func (m *Metrics) RecordProcessedEventsDuplicate(consumer string) {
	m.platformDuplicateMessagesTotal.WithLabelValues(platformCascadeQueue).Inc()
	m.processedEventsDuplicatesTotal.WithLabelValues(consumer).Inc()
}

// RecordUnknownEventAcknowledged increments
// iam_delegation_unknown_event_acknowledged_total.
func (m *Metrics) RecordUnknownEventAcknowledged(consumer, eventType string) {
	m.unknownEventAcknowledgedTotal.WithLabelValues(consumer, eventType).Inc()
}
