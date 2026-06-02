package up

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

// SidecarSessionEvent is delivered to SidecarOptions.OnSession when a session ends.
type SidecarSessionEvent struct {
	Path     string
	Start    time.Time
	Duration time.Duration
	Err      error
}

// SidecarOptions configures a Sidecar.
type SidecarOptions struct {
	ListenAddr      string
	ShutdownTimeout time.Duration // default 5s
	EgressURL       string        // empty = break-glass direct-dial
	Rationale       string        // required when EgressURL is empty
	OnSession       func(SidecarSessionEvent)
	// StartupLogFile, when non-empty, causes the startup rationale to also be
	// written to this file (one line). Used by e2e tests to assert the rationale
	// without capturing raw stderr.
	StartupLogFile string
}

// Sidecar wraps an http.Handler with net.Listen, readiness, and graceful shutdown.
type Sidecar struct {
	handler      http.Handler
	opts         SidecarOptions
	ready        chan struct{} // closed after net.Listen, before srv.Serve
	started      chan struct{} // closed when execute sets srv+ln, or returns with error
	resolvedAddr string
	srv          *http.Server
	ln           net.Listener
	shutdownOnce sync.Once
}

// NewSidecar creates a new Sidecar wrapping the given handler.
func NewSidecar(handler http.Handler, opts SidecarOptions) *Sidecar {
	if opts.ShutdownTimeout == 0 {
		opts.ShutdownTimeout = 5 * time.Second
	}
	return &Sidecar{
		handler: handler,
		opts:    opts,
		ready:   make(chan struct{}),
		started: make(chan struct{}),
	}
}

// Ready returns a channel that is closed after net.Listen succeeds and before
// srv.Serve is called. ListenAddr() returns a valid address after Ready() closes.
func (s *Sidecar) Ready() <-chan struct{} { return s.ready }

// ListenAddr returns the resolved listen address (e.g. "127.0.0.1:43210").
// Returns "" before Ready() closes.
func (s *Sidecar) ListenAddr() string { return s.resolvedAddr }

// execute is the Group actor. name is the filter name for logging.
func (s *Sidecar) execute(name string) error {
	ln, err := net.Listen("tcp", s.opts.ListenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sidecar %s: listen %s: %v\n", name, s.opts.ListenAddr, err)
		close(s.started)
		return err
	}

	s.ln = ln
	s.resolvedAddr = ln.Addr().String()

	handler := http.Handler(s.handler)
	if s.opts.OnSession != nil {
		onSession := s.opts.OnSession
		orig := s.handler
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			orig.ServeHTTP(w, r)
			onSession(SidecarSessionEvent{
				Path:     r.URL.Path,
				Start:    start,
				Duration: time.Since(start),
			})
		})
	}

	s.srv = &http.Server{Handler: handler}
	close(s.ready)   // happens-before: resolvedAddr and srv are visible to callers after Ready() closes
	close(s.started) // happens-before: stop can safely wait on started

	// Startup log.
	if s.opts.EgressURL == "" {
		msg := ""
		if s.opts.Rationale != "" {
			msg = fmt.Sprintf("sidecar %s: direct-dial mode (break-glass): %s\n", name, s.opts.Rationale)
		} else {
			msg = fmt.Sprintf("sidecar %s: WARNING: direct-dial mode with no rationale\n", name)
		}
		fmt.Fprint(os.Stderr, msg)
		if s.opts.StartupLogFile != "" {
			f, ferr := os.OpenFile(s.opts.StartupLogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
			if ferr == nil {
				fmt.Fprint(f, msg) //nolint:errcheck
				f.Close()          //nolint:errcheck
			}
		}
	}

	return s.srv.Serve(ln)
}

// stop is the Group actor stop. Safe to call before execute binds.
func (s *Sidecar) stop() {
	s.shutdownOnce.Do(func() {
		<-s.started
		if s.srv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), s.opts.ShutdownTimeout)
			defer cancel()
			s.srv.Shutdown(ctx) //nolint:errcheck
		}
		if s.ln != nil {
			s.ln.Close() //nolint:errcheck
		}
	})
}

// WithSidecar returns a FilterOption that wires s into a new Group and attaches
// it to the filter. The sidecar starts when Envoy loads the filter config and
// stops (with graceful shutdown) when Envoy destroys the factory.
func WithSidecar(s *Sidecar) FilterOption {
	return func(cf *configFactory) {
		g := NewGroup()
		name := cf.name
		g.Add(
			func() error { return s.execute(name) },
			s.stop,
		)
		cf.group = g
	}
}
