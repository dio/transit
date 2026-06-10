package up

import (
	"testing"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
	"github.com/stretchr/testify/require"

	"github.com/dio/transit/up/testutil"
)

func TestWriter_MetricLabelsDirectWrite(t *testing.T) {
	h := testutil.NewFilterHandle()
	w := NewWriter(h)

	w.IncrementCounterLabels(11, 2, "chat", "openai")
	w.RecordHistogramLabels(12, 42, "chat", "openai", "input")

	require.Equal(t, []testutil.MetricRecord{
		{ID: shared.MetricID(11), Value: 2, Labels: []string{"chat", "openai"}},
	}, h.Counters)
	require.Equal(t, []testutil.MetricRecord{
		{ID: shared.MetricID(12), Value: 42, Labels: []string{"chat", "openai", "input"}},
	}, h.Histograms)
}

func TestWriter_MetricLabelsQueuedFlush(t *testing.T) {
	h := testutil.NewFilterHandle()
	f := &filter{handle: h}
	w := &Writer{f: f}

	counterLabels := []string{"chat", "openai"}
	histogramLabels := []string{"chat", "openai", "input"}
	w.IncrementCounterLabels(21, 3, counterLabels...)
	w.RecordHistogramLabels(22, 84, histogramLabels...)

	counterLabels[0] = "mutated"
	histogramLabels[0] = "mutated"
	f.flush(false)

	require.Equal(t, []testutil.MetricRecord{
		{ID: shared.MetricID(21), Value: 3, Labels: []string{"chat", "openai"}},
	}, h.Counters)
	require.Equal(t, []testutil.MetricRecord{
		{ID: shared.MetricID(22), Value: 84, Labels: []string{"chat", "openai", "input"}},
	}, h.Histograms)
}

func TestWriter_MetricNoLabelMethodsRemainUnlabeled(t *testing.T) {
	h := testutil.NewFilterHandle()
	w := NewWriter(h)

	w.IncrementCounter(31, 4)
	w.RecordHistogram(32, 168)

	require.Equal(t, []testutil.MetricRecord{
		{ID: shared.MetricID(31), Value: 4},
	}, h.Counters)
	require.Equal(t, []testutil.MetricRecord{
		{ID: shared.MetricID(32), Value: 168},
	}, h.Histograms)
}
