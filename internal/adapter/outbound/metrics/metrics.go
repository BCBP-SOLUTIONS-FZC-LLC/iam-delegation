// Package metrics registers every iam-delegation-specific Prometheus
// instrument this service emits, named per iam-lld-delegation-service.md
// §14.2. Uses prometheus/client_golang directly, registered onto
// gincommon.MetricsRegisterer() — the same registry platform-gincommon's
// own HTTP metrics and this process's /metrics endpoint already share —
// mirroring iam-realm-provisioner's convention (and the other IAM siblings).
//
// Generic per-request HTTP metrics (count/duration/status by method+route)
// are deliberately NOT reimplemented here: gincommon's own
// ObservabilityMiddlewares already records those (http_requests_total/
// http_request_duration_seconds), and platform-events' consumer likewise
// emits its own events_*/sqs_* metrics — §14.2's "plus passthrough
// http_*/events_*/sqs_*". Everything below is a metric neither of those
// has an equivalent for: business-level create/end/review/cascade
// outcomes.
package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

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

// Live is the process-wide recorder after Register. Nil until Register
// runs (and in unit tests that never bootstrap metrics). Adapters that
// cannot take a constructor parameter (HTTP handlers, outbound clients)
// read this — matching iam-realm-provisioner's package-level collectors.
var Live *Metrics

var (
	registerOnce sync.Once
	registerErr  error
)

// Metrics holds every iam_delegation_* instrument this service emits.
type Metrics struct {
	createdTotal                   *prometheus.CounterVec
	endedTotal                     *prometheus.CounterVec
	activeGauge                    *prometheus.GaugeVec
	expiryDeferredTotal            prometheus.Counter
	activationDeferredTotal        prometheus.Counter
	reviewDeferredTotal            prometheus.Counter
	reviewWarnedTotal              *prometheus.CounterVec
	reviewExpiredTotal             prometheus.Counter
	membershipCheckDuration        prometheus.Histogram
	membershipCheckFailuresTotal   prometheus.Counter
	upAvailabilityFailuresTotal    *prometheus.CounterVec
	idempotencyHitsTotal           prometheus.Counter
	cascadeProcessedTotal          prometheus.Counter
	cascadeDLQTotal                prometheus.Counter
	processedEventsDuplicatesTotal *prometheus.CounterVec
	unknownEventAcknowledgedTotal  *prometheus.CounterVec
}

// Register wires business metrics onto gincommon's Prometheus registerer
// (same registry and {service, version} const labels as HTTP metrics).
// Call once at startup AFTER ObservabilityMiddlewares has run and BEFORE
// the /metrics endpoint is served. Idempotent — matching
// iam-realm-provisioner's no-arg Register().
func Register() *Metrics {
	registerOnce.Do(func() {
		Live, registerErr = registerOn(gincommon.MetricsRegisterer())
	})
	if registerErr != nil {
		panic(registerErr)
	}
	return Live
}

// RegisterOn builds and registers every iam_delegation_* instrument onto
// an isolated registerer. Tests that must not pollute the process-wide
// gincommon registry use this; composition roots call Register().
func RegisterOn(reg prometheus.Registerer) (*Metrics, error) {
	return registerOn(reg)
}

func registerOn(reg prometheus.Registerer) (*Metrics, error) {
	labels := gincommonLabels()
	m := &Metrics{
		createdTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name:        "iam_delegation_created_total",
				Help:        "Total delegations created (DLG-2), labeled by scope (all/department/tender).",
				ConstLabels: labels,
			},
			[]string{"scope"},
		),
		endedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name:        "iam_delegation_ended_total",
				Help:        "Total delegations ended, labeled by ended_reason (expired/cancelled/reassigned/delegate_removed/review_expired/delegate_disabled).",
				ConstLabels: labels,
			},
			[]string{"ended_reason"},
		),
		activeGauge: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name:        "iam_delegation_active_gauge",
				Help:        "Current count of active delegations, labeled by tenant.",
				ConstLabels: labels,
			},
			[]string{"tenant"},
		),
		expiryDeferredTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name:        "iam_delegation_expiry_deferred_total",
				Help:        "Total DEL-6 expiry auto-end cron passes deferred because iam-user-profile was unavailable.",
				ConstLabels: labels,
			},
		),
		activationDeferredTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name:        "iam_delegation_activation_deferred_total",
				Help:        "Total DLG-D25 activation cron passes deferred because iam-user-profile was unavailable.",
				ConstLabels: labels,
			},
		),
		reviewDeferredTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name:        "iam_delegation_review_deferred_total",
				Help:        "Total DLG-Q5/DLG-D6 review auto-end cron passes deferred because iam-user-profile was unavailable.",
				ConstLabels: labels,
			},
		),
		reviewWarnedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name:        "iam_delegation_review_warned_total",
				Help:        "Total DLG-Q6 3-day daily-cascade review notices fired, labeled by days_remaining (3, 2, or 1).",
				ConstLabels: labels,
			},
			[]string{"days_remaining"},
		),
		reviewExpiredTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name:        "iam_delegation_review_expired_total",
				Help:        "Total delegations auto-ended by the review-window cron (DLG-D6/DLG-Q5, ended_reason=review_expired).",
				ConstLabels: labels,
			},
		),
		membershipCheckDuration: prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name:        "iam_delegation_membership_check_duration_seconds",
				Help:        "Latency of Core's grant-time membership-existence check (LLD §7.6.2).",
				Buckets:     prometheus.DefBuckets,
				ConstLabels: labels,
			},
		),
		membershipCheckFailuresTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name:        "iam_delegation_membership_check_failures_total",
				Help:        "Total grant-time membership-existence checks that failed (network/timeout/5xx).",
				ConstLabels: labels,
			},
		),
		upAvailabilityFailuresTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name:        "iam_delegation_up_availability_failures_total",
				Help:        "Total iam-user-profile SetAvailability call failures, labeled by path (create/cancel/extend/reassign/cascade/expiry-cron/review-cron/activation-cron).",
				ConstLabels: labels,
			},
			[]string{"path"},
		),
		idempotencyHitsTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name:        "iam_delegation_idempotency_hits_total",
				Help:        "Total DLG-2 create calls short-circuited by a create-idempotency-key replay (DLG-Q3).",
				ConstLabels: labels,
			},
		),
		cascadeProcessedTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name:        "iam_delegation_cascade_processed_total",
				Help:        "Total inbound MembershipRevoked/TenantMembershipsPurged cascade messages processed successfully.",
				ConstLabels: labels,
			},
		),
		cascadeDLQTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name:        "iam_delegation_cascade_dlq_total",
				Help:        "Total inbound cascade messages routed to the DLQ (schema-decode failure or exhausted retries).",
				ConstLabels: labels,
			},
		),
		processedEventsDuplicatesTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name:        "iam_delegation_processed_events_duplicates_total",
				Help:        "Total inbound SQS redeliveries skipped because processed_events already recorded the envelope (IDEMP-4).",
				ConstLabels: labels,
			},
			[]string{"consumer"},
		),
		unknownEventAcknowledgedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name:        "iam_delegation_unknown_event_acknowledged_total",
				Help:        "Total inbound SQS events with no wired handler, silently acked and marked processed (forward-compat).",
				ConstLabels: labels,
			},
			[]string{"consumer", "event_type"},
		),
	}

	collectors := []prometheus.Collector{
		m.createdTotal, m.endedTotal, m.activeGauge,
		m.expiryDeferredTotal, m.activationDeferredTotal, m.reviewDeferredTotal, m.reviewWarnedTotal, m.reviewExpiredTotal,
		m.membershipCheckDuration, m.membershipCheckFailuresTotal,
		m.upAvailabilityFailuresTotal, m.idempotencyHitsTotal,
		m.cascadeProcessedTotal, m.cascadeDLQTotal,
		m.processedEventsDuplicatesTotal, m.unknownEventAcknowledgedTotal,
	}
	for _, c := range collectors {
		if err := reg.Register(c); err != nil {
			return nil, err
		}
	}
	Live = m

	// Pre-initialize known label values so dashboards show 0 rather than
	// "no data" before the first event (mirrors iam-catalog-admin/
	// iam-tender-acl's identical convention).
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

// RecordCascadeProcessed increments iam_delegation_cascade_processed_total.
func (m *Metrics) RecordCascadeProcessed() {
	m.cascadeProcessedTotal.Inc()
}

// RecordCascadeDLQ increments iam_delegation_cascade_dlq_total.
func (m *Metrics) RecordCascadeDLQ() {
	m.cascadeDLQTotal.Inc()
}

// RecordProcessedEventsDuplicate increments
// iam_delegation_processed_events_duplicates_total for consumer.
func (m *Metrics) RecordProcessedEventsDuplicate(consumer string) {
	m.processedEventsDuplicatesTotal.WithLabelValues(consumer).Inc()
}

// RecordUnknownEventAcknowledged increments
// iam_delegation_unknown_event_acknowledged_total.
func (m *Metrics) RecordUnknownEventAcknowledged(consumer, eventType string) {
	m.unknownEventAcknowledgedTotal.WithLabelValues(consumer, eventType).Inc()
}
