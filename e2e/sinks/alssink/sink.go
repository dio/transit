// Package alssink provides an in-process gRPC access log service (ALS) receiver
// for e2e tests. It implements envoy.service.accesslog.v3.AccessLogService so
// Envoy can stream HTTP access log entries to it directly via
// envoy.access_loggers.http_grpc.
//
// Unlike otelsink (OTLP) or accessloggersink (custom HTTP), alssink uses the
// standard Envoy ALS proto — no custom filter needed; any listener wired with
// the built-in http_grpc access logger will feed it.
package alssink

import (
	"context"
	"net"
	"sync"

	accesslogdatav3 "github.com/envoyproxy/go-control-plane/envoy/data/accesslog/v3"
	accesslogv3 "github.com/envoyproxy/go-control-plane/envoy/service/accesslog/v3"
	"google.golang.org/grpc"
)

// Sink is the in-memory ALS receiver.
type Sink struct {
	accesslogv3.UnimplementedAccessLogServiceServer
	mu      sync.Mutex
	entries []*accesslogdatav3.HTTPAccessLogEntry
	notify  chan struct{}
}

// New creates a new Sink. Call Start to begin listening.
func New() *Sink {
	return &Sink{notify: make(chan struct{}, 256)}
}

// Start registers the ALS service on a random port and returns that port.
func (s *Sink) Start() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("alssink: listen: " + err.Error())
	}
	srv := grpc.NewServer()
	accesslogv3.RegisterAccessLogServiceServer(srv, s)
	go srv.Serve(l) //nolint:errcheck
	return l.Addr().(*net.TCPAddr).Port
}

// StreamAccessLogs implements AccessLogServiceServer.
func (s *Sink) StreamAccessLogs(stream accesslogv3.AccessLogService_StreamAccessLogsServer) error {
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		if http := msg.GetHttpLogs(); http != nil {
			s.mu.Lock()
			s.entries = append(s.entries, http.GetLogEntry()...)
			s.mu.Unlock()
			select {
			case s.notify <- struct{}{}:
			default:
			}
		}
	}
}

// WaitForHTTPEntry blocks until a log entry matching predicate arrives or ctx
// is cancelled. Returns (entry, true) on match, (nil, false) on timeout.
func (s *Sink) WaitForHTTPEntry(ctx context.Context, predicate func(*accesslogdatav3.HTTPAccessLogEntry) bool) (*accesslogdatav3.HTTPAccessLogEntry, bool) {
	for {
		s.mu.Lock()
		for _, e := range s.entries {
			if predicate(e) {
				s.mu.Unlock()
				return e, true
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

// Reset clears all collected entries.
func (s *Sink) Reset() {
	s.mu.Lock()
	s.entries = s.entries[:0]
	s.mu.Unlock()
}
