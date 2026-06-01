package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
	otlpmetrics "go.opentelemetry.io/proto/otlp/metrics/v1"
)

// OtelMetricsSuite tests Envoy's envoy.stat_sinks.open_telemetry stats sink.
//
// Envoy is configured with stats_flush_interval: 1s and a stats_sinks entry
// pointing at the shared in-memory OTLP sink. Envoy's tag extractor strips the
// HCM stat_prefix from metric names and encodes it as the attribute
// "envoy.http_conn_manager_prefix".
//
// For example, Envoy stat http.metadata_e2e.downstream_rq_total becomes:
//
//	metric name:  http.downstream_rq_total
//	attribute:    envoy.http_conn_manager_prefix = "metadata_e2e"
type OtelMetricsSuite struct {
	suite.Suite
}

func TestOtelMetrics(t *testing.T) {
	suite.Run(t, new(OtelMetricsSuite))
}

// TestStats_metricsReceived verifies that Envoy flushes at least one metric to
// the OTLP sink within the flush interval.
func (s *OtelMetricsSuite) TestStats_metricsReceived() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	m, ok := otelSink.WaitForMetric(ctx, func(m *otlpmetrics.Metric) bool {
		return m.Name != ""
	})
	s.Require().True(ok, "timed out waiting for any OTLP metric from Envoy stats sink")
	s.Require().NotEmpty(m.Name)
}

// TestStats_serverMetricPresent verifies that Envoy's server-scope stats
// (server.uptime, server.total_connections, etc.) are exported.
func (s *OtelMetricsSuite) TestStats_serverMetricPresent() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	m, ok := otelSink.WaitForMetric(ctx, func(m *otlpmetrics.Metric) bool {
		return strings.HasPrefix(m.Name, "server.")
	})
	s.Require().True(ok, "timed out waiting for a server.* metric")
	s.Require().NotEmpty(m.Name)
}

// TestStats_httpCounterAfterRequest verifies that after sending a request to
// the metadata-e2e listener, an OTLP metric for that listener appears.
//
// Envoy's tag extractor encodes the HCM stat_prefix as the attribute
// "envoy.http_conn_manager_prefix", so the predicate checks both the metric
// name prefix and that attribute rather than the raw stat name.
func (s *OtelMetricsSuite) TestStats_httpCounterAfterRequest() {
	req, _ := http.NewRequest(http.MethodGet, metadataAddr+"/", nil)
	resp := mustDo(s.T(), req)
	resp.Body.Close()
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	m, ok := otelSink.WaitForMetric(ctx, func(m *otlpmetrics.Metric) bool {
		return strings.HasPrefix(m.Name, "http.") && metricHasAttr(m, "envoy.http_conn_manager_prefix", "metadata_e2e")
	})
	s.Require().True(ok, "timed out waiting for an http.* metric with envoy.http_conn_manager_prefix=metadata_e2e")
	s.Require().NotEmpty(m.Name)
}

// TestStats_customDynamicModuleCounter verifies that a counter defined by a
// dynamic module filter via DefineCounter is exported via the OTel stats sink.
// Envoy prepends "dynamicmodulescustom." to every metric defined by a dynamic
// module, so "e2e.requests_total" becomes "dynamicmodulescustom.e2e.requests_total".
func (s *OtelMetricsSuite) TestStats_customDynamicModuleCounter() {
	req, _ := http.NewRequest(http.MethodGet, metricsAddr+"/", nil)
	resp := mustDo(s.T(), req)
	resp.Body.Close()
	s.Require().Equal(http.StatusOK, resp.StatusCode)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	m, ok := otelSink.WaitForMetric(ctx, func(m *otlpmetrics.Metric) bool {
		return m.Name == "dynamicmodulescustom.e2e.requests_total"
	})
	s.Require().True(ok, "timed out waiting for dynamicmodulescustom.e2e.requests_total")
	s.Require().NotEmpty(m.Name)
}

// metricHasAttr reports whether any data point in m carries an attribute
// with the given key and string value.
func metricHasAttr(m *otlpmetrics.Metric, key, val string) bool {
	switch v := m.Data.(type) {
	case *otlpmetrics.Metric_Sum:
		for _, dp := range v.Sum.DataPoints {
			for _, a := range dp.Attributes {
				if a.Key == key && a.Value.GetStringValue() == val {
					return true
				}
			}
		}
	case *otlpmetrics.Metric_Gauge:
		for _, dp := range v.Gauge.DataPoints {
			for _, a := range dp.Attributes {
				if a.Key == key && a.Value.GetStringValue() == val {
					return true
				}
			}
		}
	case *otlpmetrics.Metric_Histogram:
		for _, dp := range v.Histogram.DataPoints {
			for _, a := range dp.Attributes {
				if a.Key == key && a.Value.GetStringValue() == val {
					return true
				}
			}
		}
	}
	return false
}
