package metrics

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

func TestMain(m *testing.M) {
	// ObservabilityMiddlewares is gincommon's public metrics-init API.
	// Call it before Register() so collectors pick up {service, version}
	// const labels and land on gincommon's registerer — the same order
	// cmd/server/main.go uses (iam-realm-provisioner).
	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{
		ServiceName:  "iam-delegation",
		BuildVersion: "test",
	})
	os.Exit(m.Run())
}

func ensureRegistered(t testing.TB) *Metrics {
	t.Helper()
	return Register(RegisterConfig{Environment: "test"})
}

func counterValue(t *testing.T, c prometheus.Collector) float64 {
	t.Helper()
	ch := make(chan prometheus.Metric, 1)
	c.Collect(ch)
	m := &dto.Metric{}
	require.NoError(t, (<-ch).Write(m))
	if m.Counter != nil {
		return m.Counter.GetValue()
	}
	return m.Gauge.GetValue()
}

func TestRegister_RecordsIncrementCounters(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := RegisterOn(reg, RegisterConfig{Environment: "test"})
	require.NoError(t, err)

	m.RecordCreated("all")
	m.RecordCreated("all")
	require.Equal(t, float64(2), counterValue(t, m.createdTotal.WithLabelValues("all")))

	m.RecordEnded("expired")
	m.RecordReviewWarned("7")
	m.RecordExpiryDeferred()
	m.RecordActivationDeferred()
	m.RecordReviewDeferred()
	m.RecordReviewExpired()
	m.RecordMembershipCheckFailure()
	m.RecordUPAvailabilityFailure("create")
	m.RecordIdempotencyHit()
	m.RecordMessageReceived("delegation-cascade-q")
	m.RecordCascadeProcessed()
	m.RecordCascadeDLQ()
	m.RecordProcessedEventsDuplicate("cascade")
	m.RecordUnknownEventAcknowledged("cascade", "SomeOtherEvent")
	m.ObserveMembershipCheckDuration(0.01)
	m.ReplaceActiveGauges(map[string]int64{"tenant-1": 3})

	metricFamilies, err := reg.Gather()
	require.NoError(t, err)
	require.NotEmpty(t, metricFamilies)

	names := map[string]bool{}
	for _, mf := range metricFamilies {
		names[mf.GetName()] = true
	}

	// Tier 1: platform_* metrics
	for _, want := range []string{
		"platform_messages_received_total",
		"platform_messages_processed_total",
		"platform_messages_failed_total",
		"platform_duplicate_messages_total",
	} {
		require.True(t, names[want], "missing platform metric family %s", want)
	}

	// Tier 3: iam_delegation_* metrics (including legacy compatibility metrics)
	for _, want := range []string{
		"iam_delegation_created_total",
		"iam_delegation_ended_total",
		"iam_delegation_active_gauge",
		"iam_delegation_expiry_deferred_total",
		"iam_delegation_activation_deferred_total",
		"iam_delegation_review_deferred_total",
		"iam_delegation_review_warned_total",
		"iam_delegation_review_expired_total",
		"iam_delegation_membership_check_duration_seconds",
		"iam_delegation_membership_check_failures_total",
		"iam_delegation_up_availability_failures_total",
		"iam_delegation_idempotency_hits_total",
		"iam_delegation_cascade_processed_total",
		"iam_delegation_cascade_dlq_total",
		"iam_delegation_processed_events_duplicates_total",
		"iam_delegation_unknown_event_acknowledged_total",
	} {
		require.True(t, names[want], "missing iam_delegation metric family %s", want)
	}
}

// TestRecordCascadeProcessed_DualEmit verifies that RecordCascadeProcessed
// increments both the platform Tier 1 metric and the legacy Tier 3 metric.
func TestRecordCascadeProcessed_DualEmit(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := RegisterOn(reg, RegisterConfig{Environment: "test"})
	require.NoError(t, err)

	m.RecordCascadeProcessed()

	require.Equal(t, float64(1), counterValue(t, m.platformMessagesProcessedTotal.WithLabelValues("delegation-cascade-q")))
	require.Equal(t, float64(1), counterValue(t, m.cascadeProcessedTotal))
}

// TestRecordCascadeDLQ_DualEmit verifies that RecordCascadeDLQ increments
// both the platform Tier 1 metric and the legacy Tier 3 metric.
func TestRecordCascadeDLQ_DualEmit(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := RegisterOn(reg, RegisterConfig{Environment: "test"})
	require.NoError(t, err)

	m.RecordCascadeDLQ()

	require.Equal(t, float64(1), counterValue(t, m.platformMessagesFailedTotal.WithLabelValues("delegation-cascade-q")))
	require.Equal(t, float64(1), counterValue(t, m.cascadeDLQTotal))
}

// TestRecordProcessedEventsDuplicate_DualEmit verifies that
// RecordProcessedEventsDuplicate increments both the platform Tier 1 metric
// (queue label) and the legacy Tier 3 metric (consumer label).
func TestRecordProcessedEventsDuplicate_DualEmit(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := RegisterOn(reg, RegisterConfig{Environment: "test"})
	require.NoError(t, err)

	m.RecordProcessedEventsDuplicate("cascade")

	require.Equal(t, float64(1), counterValue(t, m.platformDuplicateMessagesTotal.WithLabelValues("delegation-cascade-q")))
	require.Equal(t, float64(1), counterValue(t, m.processedEventsDuplicatesTotal.WithLabelValues("cascade")))
}

// TestRecordMessageReceived verifies that RecordMessageReceived increments
// platform_messages_received_total for the given queue.
func TestRecordMessageReceived(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := RegisterOn(reg, RegisterConfig{Environment: "test"})
	require.NoError(t, err)

	m.RecordMessageReceived("delegation-cascade-q")
	m.RecordMessageReceived("delegation-cascade-q")

	require.Equal(t, float64(2), counterValue(t, m.platformMessagesReceivedTotal.WithLabelValues("delegation-cascade-q")))
}

func TestRegisterOn_TwiceOnSameRegistryErrors(t *testing.T) {
	reg := prometheus.NewRegistry()
	_, err := RegisterOn(reg, RegisterConfig{})
	require.NoError(t, err)

	_, err = RegisterOn(reg, RegisterConfig{})
	require.Error(t, err, "a second RegisterOn on the same registry must surface the duplicate-collector error, not panic")
}

func TestRegister_IsIdempotent(t *testing.T) {
	ensureRegistered(t)
	assert.NotPanics(t, func() { Register(RegisterConfig{Environment: "test"}) })
}

func TestRegister_LandsOnGincommonRegisterer(t *testing.T) {
	m := ensureRegistered(t)
	m.RecordCreated("all")

	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	names := make(map[string]bool, len(families))
	for _, f := range families {
		names[f.GetName()] = true
	}
	assert.True(t, names["iam_delegation_created_total"], "collectors must land on gincommon's registerer (DefaultRegisterer after ObservabilityMiddlewares)")
}

func TestRegister_AppliesGincommonConstLabels(t *testing.T) {
	m := ensureRegistered(t)

	got := gincommonLabels()
	want := gincommon.MetricsConstLabels()
	assert.Equal(t, want, got)
	assert.Equal(t, "iam-delegation", got["service"])
	assert.Equal(t, "test", got["version"])

	m.RecordCreated("tender")
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	var found bool
	for _, f := range families {
		if f.GetName() != "iam_delegation_created_total" {
			continue
		}
		found = true
		require.NotEmpty(t, f.GetMetric())
		labels := map[string]string{}
		for _, lp := range f.GetMetric()[0].GetLabel() {
			labels[lp.GetName()] = lp.GetValue()
		}
		assert.Equal(t, "iam-delegation", labels["service"])
		assert.Equal(t, "test", labels["version"])
		assert.Equal(t, "test", labels["environment"], "environment label must be present on Tier 3 metrics")
	}
	assert.True(t, found, "iam_delegation_created_total must be gathered")
}

func TestReplaceActiveGauges_ResetDropsAbsentTenants(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := RegisterOn(reg, RegisterConfig{Environment: "test"})
	require.NoError(t, err)

	m.ReplaceActiveGauges(map[string]int64{"tenant-a": 3, "tenant-b": 1})
	require.Equal(t, float64(3), counterValue(t, m.activeGauge.WithLabelValues("tenant-a")))
	require.Equal(t, float64(1), counterValue(t, m.activeGauge.WithLabelValues("tenant-b")))

	m.ReplaceActiveGauges(map[string]int64{"tenant-b": 2})
	require.Equal(t, float64(2), counterValue(t, m.activeGauge.WithLabelValues("tenant-b")))

	ch := make(chan prometheus.Metric, 8)
	m.activeGauge.Collect(ch)
	close(ch)
	var tenants []string
	for metric := range ch {
		dm := &dto.Metric{}
		require.NoError(t, metric.Write(dm))
		for _, lp := range dm.GetLabel() {
			if lp.GetName() == "tenant" {
				tenants = append(tenants, lp.GetValue())
			}
		}
	}
	assert.ElementsMatch(t, []string{"tenant-b"}, tenants, "a tenant that dropped to zero active rows must disappear, not linger")
}

// TestRegister_PanicsOnError covers lines 79–80: Register panics when
// registerErr is non-nil after registerOnce.Do has already fired.
// We bypass Do by setting registerErr directly (same-package access).
func TestRegister_PanicsOnError(t *testing.T) {
	// Ensure Do has already fired so our manual set takes effect.
	Register(RegisterConfig{Environment: "test"})

	orig := registerErr
	origLive := Live
	t.Cleanup(func() { registerErr = orig; Live = origLive })

	registerErr = errors.New("forced registration error")
	assert.Panics(t, func() { Register(RegisterConfig{Environment: "test"}) }, "Register must panic when registerErr is set")

	registerErr = orig
	Live = origLive
}

// TestMetricsConformanceStandard enforces the Enterprise Platform
// Observability Standard naming and label invariants at the registry level:
//
//   - Counters must end in _total.
//   - Histograms must end in _seconds.
//   - platform_* metrics must carry domain, service, environment labels.
//   - platform_* metrics must not carry high-cardinality labels.
//   - iam_delegation_* metrics must carry service and environment labels.
func TestMetricsConformanceStandard(t *testing.T) {
	reg := prometheus.NewRegistry()
	_, err := RegisterOn(reg, RegisterConfig{Environment: "test"})
	require.NoError(t, err)

	families, err := reg.Gather()
	require.NoError(t, err)
	require.NotEmpty(t, families)

	highCardinality := []string{"user_id", "email", "tenant_id", "request_id", "event_id", "session_id"}

	for _, mf := range families {
		name := mf.GetName()

		// Rule: counters must end in _total.
		if mf.GetType() == dto.MetricType_COUNTER {
			assert.True(t, strings.HasSuffix(name, "_total"),
				"counter metric %q must end in _total (Enterprise Platform Observability Standard §Naming-4)", name)
		}

		// Rule: histograms must end in _seconds.
		if mf.GetType() == dto.MetricType_HISTOGRAM {
			assert.True(t, strings.HasSuffix(name, "_seconds"),
				"histogram metric %q must end in _seconds (Enterprise Platform Observability Standard §Naming-5)", name)
		}

		for _, m := range mf.GetMetric() {
			labelMap := make(map[string]string, len(m.GetLabel()))
			for _, lp := range m.GetLabel() {
				labelMap[lp.GetName()] = lp.GetValue()
			}

			// Tier 1: platform_* required labels.
			if strings.HasPrefix(name, "platform_") {
				assert.Equal(t, "iam", labelMap["domain"],
					"platform_* metric %q must have domain=\"iam\" label", name)
				assert.Equal(t, "iam-delegation", labelMap["service"],
					"platform_* metric %q must have service label", name)
				assert.NotEmpty(t, labelMap["environment"],
					"platform_* metric %q must have non-empty environment label", name)

				// No high-cardinality labels on platform_* metrics.
				for _, forbidden := range highCardinality {
					_, exists := labelMap[forbidden]
					assert.False(t, exists,
						"platform_* metric %q must not carry high-cardinality label %q", name, forbidden)
				}
			}

			// Tier 3: iam_delegation_* required labels.
			if strings.HasPrefix(name, "iam_delegation_") {
				assert.NotEmpty(t, labelMap["service"],
					"iam_delegation_* metric %q must have service label", name)
				assert.NotEmpty(t, labelMap["environment"],
					"iam_delegation_* metric %q must have environment label", name)
			}
		}
	}
}
