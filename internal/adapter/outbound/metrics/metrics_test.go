package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

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
	m, err := Register(reg)
	require.NoError(t, err)

	m.RecordCreated("all")
	m.RecordCreated("all")
	require.Equal(t, float64(2), counterValue(t, m.createdTotal.WithLabelValues("all")))

	m.RecordEnded("expired")
	m.RecordReviewWarned("7")
	m.RecordExpiryDeferred()
	m.RecordReviewDeferred()
	m.RecordReviewExpired()
	m.RecordMembershipCheckFailure()
	m.RecordUPAvailabilityFailure("create")
	m.RecordIdempotencyHit()
	m.RecordCascadeProcessed()
	m.RecordCascadeDLQ()
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
		"iam_delegation_review_deferred_total",
		"iam_delegation_review_warned_total",
		"iam_delegation_review_expired_total",
		"iam_delegation_membership_check_duration_seconds",
		"iam_delegation_membership_check_failures_total",
		"iam_delegation_up_availability_failures_total",
		"iam_delegation_idempotency_hits_total",
		"iam_delegation_cascade_processed_total",
		"iam_delegation_cascade_dlq_total",
	} {
		require.True(t, names[want], "missing metric family %s", want)
	}
}

func TestRegister_TwiceOnSameRegistryErrors(t *testing.T) {
	reg := prometheus.NewRegistry()
	_, err := Register(reg)
	require.NoError(t, err)

	_, err = Register(reg)
	require.Error(t, err, "a second Register on the same registry must surface the duplicate-collector error, not panic")
}
