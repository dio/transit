package mcp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type sidecarOptions struct {
	listenAddr      string
	shutdownTimeout time.Duration
	egressURL       string
}

// Sidecar is the MCP HTTP/SSE sidecar. Create with newSidecar or mcp.NewSidecar,
// then call Listen to bind the socket, Serve to start accepting connections, and
// Stop to shut down gracefully.
type Sidecar struct {
	handler http.Handler
	opts    sidecarOptions

	ready          chan struct{}
	started        chan struct{}
	mu             sync.Mutex
	srv            *http.Server
	ln             net.Listener
	resolved       string
	unixSocketPath string // non-empty when ln is a Unix socket; removed in Stop
	stopOnce       sync.Once
}

func newSidecar(h http.Handler, opts sidecarOptions) *Sidecar {
	if opts.shutdownTimeout == 0 {
		opts.shutdownTimeout = 5 * time.Second
	}
	return &Sidecar{
		handler: h,
		opts:    opts,
		ready:   make(chan struct{}),
		started: make(chan struct{}),
	}
}

func (s *Sidecar) Ready() <-chan struct{} { return s.ready }

func (s *Sidecar) ListenAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolved
}

// Listen binds the listener synchronously. Supports TCP (default) and Unix
// domain sockets (addr prefixed with "unix://"). On success Ready() is closed
// and ListenAddr() returns the actual bound address. On failure the error is
// returned and no channels are closed.
func (s *Sidecar) Listen() error {
	ln, err := listenForSidecar(s.opts.listenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "orange-mcp: sidecar listen %s: %v\n", s.opts.listenAddr, err)
		close(s.started)
		return err
	}

	var unixPath string
	if ln.Addr().Network() == "unix" {
		unixPath = ln.Addr().String()
	}

	s.mu.Lock()
	s.ln = ln
	s.resolved = ln.Addr().String()
	s.srv = &http.Server{Handler: s.handler}
	s.unixSocketPath = unixPath
	s.mu.Unlock()

	close(s.ready)
	close(s.started)
	return nil
}

// Serve accepts connections on the already-bound listener. Must be called after
// a successful Listen. Returns http.ErrServerClosed when Stop is called.
func (s *Sidecar) Serve() error {
	if s.opts.egressURL == "" {
		fmt.Fprintf(os.Stderr, "orange-mcp: WARNING: MCP sidecar egress URL is empty; backend egress must go through Envoy\n")
	}
	s.mu.Lock()
	ln := s.ln
	srv := s.srv
	s.mu.Unlock()
	return srv.Serve(ln)
}

// Stop shuts down the sidecar gracefully and removes the Unix socket file if
// applicable.
func (s *Sidecar) Stop() {
	s.stopOnce.Do(func() {
		<-s.started
		s.mu.Lock()
		srv := s.srv
		ln := s.ln
		unixPath := s.unixSocketPath
		s.mu.Unlock()
		if srv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), s.opts.shutdownTimeout)
			defer cancel()
			_ = srv.Shutdown(ctx)
		}
		if ln != nil {
			_ = ln.Close()
		}
		if unixPath != "" {
			_ = os.Remove(unixPath)
		}
	})
}

// listenForSidecar parses addr and returns the appropriate listener:
//   - "unix://path" or "unix:///path" → net.Listen("unix", path)
//   - anything else → net.Listen("tcp", addr)
func listenForSidecar(addr string) (net.Listener, error) {
	if strings.HasPrefix(addr, "unix://") {
		path := strings.TrimPrefix(addr, "unix://")
		return net.Listen("unix", path)
	}
	return net.Listen("tcp", addr)
}
