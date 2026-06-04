package mcp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

type sidecarOptions struct {
	listenAddr      string
	shutdownTimeout time.Duration
	egressURL       string
}

type sidecar struct {
	handler http.Handler
	opts    sidecarOptions

	ready    chan struct{}
	started  chan struct{}
	mu       sync.Mutex
	srv      *http.Server
	ln       net.Listener
	resolved string
	stopOnce sync.Once
}

func newSidecar(h http.Handler, opts sidecarOptions) *sidecar {
	if opts.shutdownTimeout == 0 {
		opts.shutdownTimeout = 5 * time.Second
	}
	return &sidecar{
		handler: h,
		opts:    opts,
		ready:   make(chan struct{}),
		started: make(chan struct{}),
	}
}

func (s *sidecar) Ready() <-chan struct{} { return s.ready }

func (s *sidecar) ListenAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolved
}

func (s *sidecar) execute(name string) error {
	ln, err := net.Listen("tcp", s.opts.listenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: sidecar listen %s: %v\n", name, s.opts.listenAddr, err)
		close(s.started)
		return err
	}

	s.mu.Lock()
	s.ln = ln
	s.resolved = ln.Addr().String()
	s.srv = &http.Server{Handler: s.handler}
	s.mu.Unlock()

	close(s.ready)
	close(s.started)

	if s.opts.egressURL == "" {
		fmt.Fprintf(os.Stderr, "%s: WARNING: MCP sidecar egress URL is empty; backend egress must go through Envoy\n", name)
	}
	return s.srv.Serve(ln)
}

func (s *sidecar) stop() {
	s.stopOnce.Do(func() {
		<-s.started
		s.mu.Lock()
		srv := s.srv
		ln := s.ln
		s.mu.Unlock()
		if srv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), s.opts.shutdownTimeout)
			defer cancel()
			_ = srv.Shutdown(ctx)
		}
		if ln != nil {
			_ = ln.Close()
		}
	})
}
