// Package otelsink provides an in-memory OTLP logs gRPC receiver for e2e tests.
// Start returns the port the server is listening on; WaitForRecord blocks until
// a log record matching a predicate arrives or the context is cancelled.
package otelsink

import (
	"context"
	"net"
	"sync"

	otlpcollectorlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	otlplogs "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/grpc"
)

// Sink is an in-memory OTLP logs gRPC receiver.
type Sink struct {
	otlpcollectorlogs.UnimplementedLogsServiceServer

	mu      sync.Mutex
	records []*otlplogs.LogRecord
	notify  chan struct{}
}

// New creates a new Sink. Call Start to begin listening.
func New() *Sink {
	return &Sink{notify: make(chan struct{}, 256)}
}

// Start starts the gRPC server and returns the port it is listening on.
func (s *Sink) Start() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("otelsink: listen: " + err.Error())
	}
	srv := grpc.NewServer()
	otlpcollectorlogs.RegisterLogsServiceServer(srv, s)
	go srv.Serve(l) //nolint:errcheck
	return l.Addr().(*net.TCPAddr).Port
}

// Export implements LogsServiceServer.
func (s *Sink) Export(_ context.Context, req *otlpcollectorlogs.ExportLogsServiceRequest) (*otlpcollectorlogs.ExportLogsServiceResponse, error) {
	s.mu.Lock()
	for _, rl := range req.ResourceLogs {
		for _, sl := range rl.ScopeLogs {
			s.records = append(s.records, sl.LogRecords...)
		}
	}
	s.mu.Unlock()
	// wake any WaitForRecord calls
	select {
	case s.notify <- struct{}{}:
	default:
	}
	return &otlpcollectorlogs.ExportLogsServiceResponse{}, nil
}

// WaitForRecord blocks until a LogRecord matching predicate arrives or ctx is
// cancelled. Returns (record, true) on match, (nil, false) on timeout.
func (s *Sink) WaitForRecord(ctx context.Context, predicate func(*otlplogs.LogRecord) bool) (*otlplogs.LogRecord, bool) {
	for {
		s.mu.Lock()
		for _, r := range s.records {
			if predicate(r) {
				s.mu.Unlock()
				return r, true
			}
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, false
		case <-s.notify:
		}
	}
}
