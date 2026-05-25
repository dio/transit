// Package otelsink provides an in-memory OTLP gRPC receiver (traces) for e2e
// tests. Start returns the port the gRPC server is listening on.
// WaitForSpan blocks until a matching span arrives or the context is cancelled.
package otelsink

import (
	"context"
	"net"
	"sync"

	otlpcollectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	otlptrace "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
)

// Sink is an in-memory OTLP trace receiver.
type Sink struct {
	mu          sync.Mutex
	spans       []*otlptrace.Span
	traceNotify chan struct{}
}

// New creates a new Sink. Call Start to begin listening.
func New() *Sink {
	return &Sink{traceNotify: make(chan struct{}, 256)}
}

// Start starts the gRPC server and returns the port it is listening on.
func (s *Sink) Start() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("otelsink: listen: " + err.Error())
	}
	srv := grpc.NewServer()
	otlpcollectortrace.RegisterTraceServiceServer(srv, &tracesSvc{sink: s})
	go srv.Serve(l) //nolint:errcheck
	return l.Addr().(*net.TCPAddr).Port
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
