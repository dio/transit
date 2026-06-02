package up

import (
	"net/http"
	nethttppprof "net/http/pprof"
	"time"
)

// AdminServerOptions configures the admin HTTP server.
type AdminServerOptions struct {
	// ListenAddr defaults to "127.0.0.1:6060".
	ListenAddr      string
	ShutdownTimeout time.Duration
}

// AdminServer is a local-only HTTP server for operational debug endpoints.
// Register handlers before passing it to WithAdminServer.
//
// Example:
//
//	admin := up.NewAdminServer(up.AdminServerOptions{})
//	admin.RegisterPprof()
//	up.Register("my-filter", handler, up.WithAdminServer(admin))
type AdminServer struct {
	mux     *http.ServeMux
	sidecar *Sidecar
}

// NewAdminServer creates an AdminServer. Call RegisterPprof or Handle/HandleFunc
// to add endpoints, then pass to WithAdminServer.
func NewAdminServer(opts AdminServerOptions) *AdminServer {
	if opts.ListenAddr == "" {
		opts.ListenAddr = "127.0.0.1:6060"
	}
	mux := http.NewServeMux()
	s := NewSidecar(mux, SidecarOptions{
		ListenAddr:      opts.ListenAddr,
		ShutdownTimeout: opts.ShutdownTimeout,
		Rationale:       "admin server; serves only local debug traffic",
	})
	return &AdminServer{mux: mux, sidecar: s}
}

// Handle registers a handler for the given pattern.
func (a *AdminServer) Handle(pattern string, h http.Handler) {
	a.mux.Handle(pattern, h)
}

// HandleFunc registers a handler function for the given pattern.
func (a *AdminServer) HandleFunc(pattern string, fn http.HandlerFunc) {
	a.mux.HandleFunc(pattern, fn)
}

// RegisterPprof adds the standard /debug/pprof/ suite to the admin mux.
// Explicit registration on the admin's own mux avoids polluting http.DefaultServeMux,
// which in an Envoy .so plugin risks colliding with Envoy's own Go runtime.
func (a *AdminServer) RegisterPprof() {
	a.mux.HandleFunc("/debug/pprof/", nethttppprof.Index)
	a.mux.HandleFunc("/debug/pprof/cmdline", nethttppprof.Cmdline)
	a.mux.HandleFunc("/debug/pprof/profile", nethttppprof.Profile)
	a.mux.HandleFunc("/debug/pprof/symbol", nethttppprof.Symbol)
	a.mux.HandleFunc("/debug/pprof/trace", nethttppprof.Trace)
}

// Ready returns a channel closed after net.Listen succeeds.
func (a *AdminServer) Ready() <-chan struct{} { return a.sidecar.Ready() }

// ListenAddr returns the resolved listen address. Valid after Ready() closes.
func (a *AdminServer) ListenAddr() string { return a.sidecar.ListenAddr() }

// WithAdminServer returns a FilterOption that wires a into the filter's Group
// lifecycle. The server starts when Envoy loads the filter config and stops
// (with graceful shutdown) when Envoy destroys the factory.
func WithAdminServer(a *AdminServer) FilterOption {
	return WithSidecar(a.sidecar)
}
