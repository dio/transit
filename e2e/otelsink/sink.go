// Package otelsink provides an in-memory OTLP receiver (logs + metrics) for
// e2e tests. A single gRPC server registers both LogsService and MetricsService
// on the same port. WaitForRecord / WaitForMetric block until a matching item
// arrives or the context is cancelled.
//
// Both services share a Sink that holds the received data. Because both
// LogsServiceServer and MetricsServiceServer define an Export method, they are
// wired via thin adapter types (logsSvc / metricsSvc) that delegate to the
// shared Sink rather than being embedded in it.
package otelsink

import (
	"context"
	"net"
	"sync"

	otlpcollectorlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	otlpcollectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	otlplogs "go.opentelemetry.io/proto/otlp/logs/v1"
	otlpmetrics "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/grpc"
)

// Sink is the shared in-memory store for OTLP logs and metrics.
type Sink struct {
	mu           sync.Mutex
	logRecords   []*otlplogs.LogRecord
	metrics      []*otlpmetrics.Metric
	logNotify    chan struct{}
	metricNotify chan struct{}
}

// New creates a new Sink. Call Start to begin listening.
func New() *Sink {
	return &Sink{
		logNotify:    make(chan struct{}, 256),
		metricNotify: make(chan struct{}, 256),
	}
}

// Start starts the gRPC server and returns the port it is listening on.
// Both LogsService and MetricsService are registered on the same port.
func (s *Sink) Start() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("otelsink: listen: " + err.Error())
	}
	srv := grpc.NewServer()
	otlpcollectorlogs.RegisterLogsServiceServer(srv, &logsSvc{sink: s})
	otlpcollectormetrics.RegisterMetricsServiceServer(srv, &metricsSvc{sink: s})
	go srv.Serve(l) //nolint:errcheck
	return l.Addr().(*net.TCPAddr).Port
}

// WaitForRecord blocks until a LogRecord matching predicate arrives or ctx is
// cancelled. Returns (record, true) on match, (nil, false) on timeout.
func (s *Sink) WaitForRecord(ctx context.Context, predicate func(*otlplogs.LogRecord) bool) (*otlplogs.LogRecord, bool) {
	for {
		s.mu.Lock()
		for _, r := range s.logRecords {
			if predicate(r) {
				s.mu.Unlock()
				return r, true
			}
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, false
		case <-s.logNotify:
		}
	}
}

// WaitForMetric blocks until a Metric matching predicate arrives or ctx is
// cancelled. Returns (metric, true) on match, (nil, false) on timeout.
func (s *Sink) WaitForMetric(ctx context.Context, predicate func(*otlpmetrics.Metric) bool) (*otlpmetrics.Metric, bool) {
	for {
		s.mu.Lock()
		for _, m := range s.metrics {
			if predicate(m) {
				s.mu.Unlock()
				return m, true
			}
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, false
		case <-s.metricNotify:
		}
	}
}

// logsSvc adapts Sink to otlpcollectorlogs.LogsServiceServer.
type logsSvc struct {
	otlpcollectorlogs.UnimplementedLogsServiceServer
	sink *Sink
}

func (l *logsSvc) Export(_ context.Context, req *otlpcollectorlogs.ExportLogsServiceRequest) (*otlpcollectorlogs.ExportLogsServiceResponse, error) {
	l.sink.mu.Lock()
	for _, rl := range req.ResourceLogs {
		for _, sl := range rl.ScopeLogs {
			l.sink.logRecords = append(l.sink.logRecords, sl.LogRecords...)
		}
	}
	l.sink.mu.Unlock()
	select {
	case l.sink.logNotify <- struct{}{}:
	default:
	}
	return &otlpcollectorlogs.ExportLogsServiceResponse{}, nil
}

// metricsSvc adapts Sink to otlpcollectormetrics.MetricsServiceServer.
type metricsSvc struct {
	otlpcollectormetrics.UnimplementedMetricsServiceServer
	sink *Sink
}

func (m *metricsSvc) Export(_ context.Context, req *otlpcollectormetrics.ExportMetricsServiceRequest) (*otlpcollectormetrics.ExportMetricsServiceResponse, error) {
	m.sink.mu.Lock()
	for _, rm := range req.ResourceMetrics {
		for _, sm := range rm.ScopeMetrics {
			m.sink.metrics = append(m.sink.metrics, sm.Metrics...)
		}
	}
	m.sink.mu.Unlock()
	select {
	case m.sink.metricNotify <- struct{}{}:
	default:
	}
	return &otlpcollectormetrics.ExportMetricsServiceResponse{}, nil
}
