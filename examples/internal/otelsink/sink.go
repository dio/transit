// Package otelsink provides an in-memory OTLP gRPC receiver (logs, metrics, traces)
// for e2e tests. Start returns the port the gRPC server is listening on.
// WaitForRecord/WaitForMetric/WaitForSpan block until matching items arrive or context is cancelled.
package otelsink

import (
	"context"
	"net"
	"sync"

	otlpcollectorlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	otlpcollectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	otlpcollectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	otlplogs "go.opentelemetry.io/proto/otlp/logs/v1"
	otlpmetrics "go.opentelemetry.io/proto/otlp/metrics/v1"
	otlptrace "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
)

// Sink is an in-memory OTLP receiver for logs, metrics, and traces.
type Sink struct {
	mu           sync.Mutex
	logRecords   []*otlplogs.LogRecord
	metrics      []*otlpmetrics.Metric
	spans        []*otlptrace.Span
	logNotify    chan struct{}
	metricNotify chan struct{}
	traceNotify  chan struct{}
}

// New creates a new Sink. Call Start to begin listening.
func New() *Sink {
	return &Sink{
		logNotify:    make(chan struct{}, 256),
		metricNotify: make(chan struct{}, 256),
		traceNotify:  make(chan struct{}, 256),
	}
}

// Start starts the gRPC server and returns the port it is listening on.
func (s *Sink) Start() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("otelsink: listen: " + err.Error())
	}
	srv := grpc.NewServer()
	otlpcollectorlogs.RegisterLogsServiceServer(srv, &logsSvc{sink: s})
	otlpcollectormetrics.RegisterMetricsServiceServer(srv, &metricsSvc{sink: s})
	otlpcollectortrace.RegisterTraceServiceServer(srv, &tracesSvc{sink: s})
	go srv.Serve(l) //nolint:errcheck
	addr := l.Addr()
	if addr == nil {
		panic("otelsink: addr is nil")
	}
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok {
		panic("otelsink: addr is not a TCP address")
	}
	return tcpAddr.Port
}

// WaitForRecord blocks until a log record matching predicate arrives or ctx is
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

// WaitForMetric blocks until a metric matching predicate arrives or ctx is
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

// WaitForSpan blocks until a span matching predicate arrives or ctx is
// cancelled. Returns (span, true) on match, (nil, false) on timeout.
func (s *Sink) WaitForSpan(ctx context.Context, predicate func(*otlptrace.Span) bool) (*otlptrace.Span, bool) {
	for {
		s.mu.Lock()
		for _, sp := range s.spans {
			if predicate(sp) {
				s.mu.Unlock()
				return sp, true
			}
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, false
		case <-s.traceNotify:
		}
	}
}

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

type tracesSvc struct {
	otlpcollectortrace.UnimplementedTraceServiceServer
	sink *Sink
}

func (t *tracesSvc) Export(_ context.Context, req *otlpcollectortrace.ExportTraceServiceRequest) (*otlpcollectortrace.ExportTraceServiceResponse, error) {
	t.sink.mu.Lock()
	for _, rs := range req.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			t.sink.spans = append(t.sink.spans, ss.Spans...)
		}
	}
	t.sink.mu.Unlock()
	select {
	case t.sink.traceNotify <- struct{}{}:
	default:
	}
	return &otlpcollectortrace.ExportTraceServiceResponse{}, nil
}
