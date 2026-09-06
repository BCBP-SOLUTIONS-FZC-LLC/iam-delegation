package metrics

import (
	"errors"
	"os"
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
	return Register()
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
	m, err := RegisterOn(reg)
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
	m.RecordCascadeProcessed()
	m.RecordCascadeDLQ()
	m.RecordProcessedEventsDuplicate("cascade")
	m.RecordUnknownEventAcknowledged("cascade", "SomeOtherEvent")
	m.ObserveMembershipCheckDuration(0.01)
	m.SetActiveGauge("tenant-1", 3)

	metricFamilies, err := reg.Gather()
	require.NoError(t, err)
	require.NotEmpty(t, metricFamilies)

	names := map[string]bool{}
	for _, mf := range metricFamilies {
		names[mf.GetName()] = true
	}
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
		require.True(t, names[want], "missing metric family %s", want)
	}
}

func TestRegisterOn_TwiceOnSameRegistryErrors(t *testing.T) {
	reg := prometheus.NewRegistry()
	_, err := RegisterOn(reg)
	require.NoError(t, err)

	_, err = RegisterOn(reg)
	require.Error(t, err, "a second RegisterOn on the same registry must surface the duplicate-collector error, not panic")
}

func TestRegister_IsIdempotent(t *testing.T) {
	ensureRegistered(t)
	assert.NotPanics(t, func() { Register() })
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
	}
	assert.True(t, found, "iam_delegation_created_total must be gathered")
}

func TestReplaceActiveGauges_ResetDropsAbsentTenants(t *testing.T) {
	reg := prometheus.NewRegistry()
	m, err := RegisterOn(reg)
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
	Register()

	orig := registerErr
	origLive := Live
	t.Cleanup(func() { registerErr = orig; Live = origLive })

	registerErr = errors.New("forced registration error")
	assert.Panics(t, func() { Register() }, "Register must panic when registerErr is set")

	registerErr = orig
	Live = origLive
}
