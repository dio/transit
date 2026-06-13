package up

import (
	"testing"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
	"github.com/stretchr/testify/require"

	"github.com/dio/transit/down"
	"github.com/dio/transit/up/testutil"
)

// fakeClusterFactory implements ClusterFactory (no metrics).
type fakeClusterFactory struct {
	created int
}

func (f *fakeClusterFactory) Create(_ []byte) (ClusterConfigFactory, error) {
	f.created++
	return &fakeClusterConfigFactory{}, nil
}

// fakeClusterFactoryWithMetrics implements both ClusterFactory and ClusterFactoryWithMetrics.
type fakeClusterFactoryWithMetrics struct {
	handle  ClusterMetrics
	created int
}

func (f *fakeClusterFactoryWithMetrics) Create(_ []byte) (ClusterConfigFactory, error) {
	f.created++
	return &fakeClusterConfigFactory{}, nil
}

func (f *fakeClusterFactoryWithMetrics) CreateWithMetrics(m ClusterMetrics, _ []byte) (ClusterConfigFactory, error) {
	f.handle = m
	f.created++
	return &fakeClusterConfigFactory{}, nil
}

type fakeClusterConfigFactory struct{}

func (f *fakeClusterConfigFactory) NewCluster(_ ClusterHandle) Cluster { return nil }
func (f *fakeClusterConfigFactory) Close()                             {}

// TestAdapter_PlainFactoryDoesNotImplementDownMetrics verifies that a plain
// ClusterFactory is not wrapped: the down layer sees no ClusterFactoryWithMetrics.
func TestAdapter_PlainFactoryDoesNotImplementDownMetrics(t *testing.T) {
	f := &fakeClusterFactory{}
	_, ok := any(f).(down.ClusterFactoryWithMetrics)
	require.False(t, ok)
}

// TestAdapter_CreateWithMetrics_CallsUserCreateWithMetrics verifies that
// upClusterFactoryWithMetricsAdapter.CreateWithMetrics invokes the user's
// CreateWithMetrics (not Create) and delivers a working ClusterMetrics handle.
func TestAdapter_CreateWithMetrics_CallsUserCreateWithMetrics(t *testing.T) {
	raw := &testutil.FakeClusterMetricsHandle{}
	userFactory := &fakeClusterFactoryWithMetrics{}

	adapter := &upClusterFactoryWithMetricsAdapter{
		factory:     userFactory,
		withMetrics: userFactory,
	}

	_, err := adapter.CreateWithMetrics(raw, []byte(`{}`))
	require.NoError(t, err)

	// CreateWithMetrics must have been called, not Create.
	require.Equal(t, 1, userFactory.created)
	require.NotNil(t, userFactory.handle, "user factory must receive a ClusterMetrics handle")

	// The delivered handle must forward operations to the raw fake.
	id, err := userFactory.handle.DefineCounter("hits")
	require.NoError(t, err)
	require.NotZero(t, id)
	require.NoError(t, userFactory.handle.IncrementCounter(id, 3))
	require.Len(t, raw.Counters, 1)
	require.Equal(t, uint64(3), raw.Counters[0].Value)
}

// TestAdapter_Create_FallsBackToUserCreate verifies that the adapter's Create
// method delegates to the user factory's own Create.
func TestAdapter_Create_FallsBackToUserCreate(t *testing.T) {
	userFactory := &fakeClusterFactoryWithMetrics{}

	adapter := &upClusterFactoryWithMetricsAdapter{
		factory:     userFactory,
		withMetrics: userFactory,
	}

	_, err := adapter.Create([]byte(`{}`))
	require.NoError(t, err)
	require.Equal(t, 1, userFactory.created)
}

// TestClusterMetricsAdapter_Define verifies that the up-layer adapter converts
// shared.MetricsSuccess into nil error and shared.MetricID into MetricID.
func TestClusterMetricsAdapter_Define(t *testing.T) {
	raw := &testutil.FakeClusterMetricsHandle{}
	m := &clusterMetricsAdapter{handle: raw}

	id, err := m.DefineCounter("req_total", "method")
	require.NoError(t, err)
	require.Equal(t, MetricID(1), id)

	id2, err := m.DefineGauge("active_hosts")
	require.NoError(t, err)
	require.Equal(t, MetricID(2), id2)

	id3, err := m.DefineHistogram("latency_ms", "route")
	require.NoError(t, err)
	require.Equal(t, MetricID(3), id3)
}

// TestClusterMetricsAdapter_Record verifies that record operations are forwarded
// through the adapter with correct IDs, values, and label values.
func TestClusterMetricsAdapter_Record(t *testing.T) {
	raw := &testutil.FakeClusterMetricsHandle{}
	m := &clusterMetricsAdapter{handle: raw}

	counterID, _ := m.DefineCounter("req_total")
	gaugeID, _ := m.DefineGauge("active_hosts")
	histID, _ := m.DefineHistogram("latency_ms")

	require.NoError(t, m.IncrementCounter(counterID, 5, "GET"))
	require.NoError(t, m.SetGauge(gaugeID, 10))
	require.NoError(t, m.IncrementGauge(gaugeID, 2))
	require.NoError(t, m.DecrementGauge(gaugeID, 1))
	require.NoError(t, m.RecordHistogram(histID, 42, "fast"))

	require.Equal(t, []testutil.MetricRecord{
		{ID: shared.MetricID(counterID), Value: 5, Labels: []string{"GET"}},
	}, raw.Counters)
	require.Equal(t, []testutil.MetricRecord{
		{ID: shared.MetricID(gaugeID), Value: 10},
		{ID: shared.MetricID(gaugeID), Value: 2},
		{ID: shared.MetricID(gaugeID), Value: 1},
	}, raw.Gauges)
	require.Equal(t, []testutil.MetricRecord{
		{ID: shared.MetricID(histID), Value: 42, Labels: []string{"fast"}},
	}, raw.Histograms)
}

// TestClusterMetricsAdapter_DefineError verifies that a non-success MetricsResult
// is surfaced as a Go error with the metric name in the message.
func TestClusterMetricsAdapter_DefineError(t *testing.T) {
	raw := &fakeFailingMetricsHandle{}
	m := &clusterMetricsAdapter{handle: raw}

	_, err := m.DefineCounter("bad_counter")
	require.Error(t, err)
	require.Contains(t, err.Error(), "bad_counter")

	_, err = m.DefineGauge("bad_gauge")
	require.Error(t, err)
	require.Contains(t, err.Error(), "bad_gauge")

	_, err = m.DefineHistogram("bad_hist")
	require.Error(t, err)
	require.Contains(t, err.Error(), "bad_hist")
}

// TestClusterMetricsAdapter_RecordError verifies that record errors are surfaced.
func TestClusterMetricsAdapter_RecordError(t *testing.T) {
	raw := &fakeFailingMetricsHandle{}
	m := &clusterMetricsAdapter{handle: raw}

	require.Error(t, m.IncrementCounter(1, 1))
	require.Error(t, m.SetGauge(1, 1))
	require.Error(t, m.IncrementGauge(1, 1))
	require.Error(t, m.DecrementGauge(1, 1))
	require.Error(t, m.RecordHistogram(1, 1))
}

// fakeFailingMetricsHandle always returns MetricsNotFound to exercise error paths.
type fakeFailingMetricsHandle struct{}

func (h *fakeFailingMetricsHandle) DefineCounter(_ string, _ ...string) (shared.MetricID, shared.MetricsResult) {
	return 0, shared.MetricsNotFound
}
func (h *fakeFailingMetricsHandle) DefineGauge(_ string, _ ...string) (shared.MetricID, shared.MetricsResult) {
	return 0, shared.MetricsNotFound
}
func (h *fakeFailingMetricsHandle) DefineHistogram(_ string, _ ...string) (shared.MetricID, shared.MetricsResult) {
	return 0, shared.MetricsNotFound
}
func (h *fakeFailingMetricsHandle) IncrementCounterValue(_ shared.MetricID, _ uint64, _ ...string) shared.MetricsResult {
	return shared.MetricsNotFound
}
func (h *fakeFailingMetricsHandle) SetGaugeValue(_ shared.MetricID, _ uint64, _ ...string) shared.MetricsResult {
	return shared.MetricsNotFound
}
func (h *fakeFailingMetricsHandle) IncrementGaugeValue(_ shared.MetricID, _ uint64, _ ...string) shared.MetricsResult {
	return shared.MetricsNotFound
}
func (h *fakeFailingMetricsHandle) DecrementGaugeValue(_ shared.MetricID, _ uint64, _ ...string) shared.MetricsResult {
	return shared.MetricsNotFound
}
func (h *fakeFailingMetricsHandle) RecordHistogramValue(_ shared.MetricID, _ uint64, _ ...string) shared.MetricsResult {
	return shared.MetricsNotFound
}
