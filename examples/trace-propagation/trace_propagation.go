// Package tracepropagation is a Transit Envoy dynamic module example that
// demonstrates W3C trace context propagation through a two-leg Envoy path.
//
// Architecture:
//
//	Client ──► Envoy:ProxyPort ──► trace-propagation-local:ServerPort
//	                               ↓ (embedded HTTP server copies trace headers)
//	                               Envoy:EgressPort ──► Backend sink
//
// The filter registers an embedded HTTP server via up.RegisterWithGroup.
// The server forwards every request to the configured egress URL, copying
// traceparent, tracestate, and x-request-id headers verbatim.
//
// Runtime overrides (for e2e):
//
//	TRACE_PROPAGATION_LISTEN_ADDR — address for the embedded server (default: 127.0.0.1:9192)
//	TRACE_PROPAGATION_EGRESS_URL  — egress base URL (default: http://127.0.0.1:9193)
package tracepropagation

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"

	"github.com/dio/transit/up"
)

// ExtensionName is the Envoy filter name used in envoy.yaml.
const ExtensionName = "trace-propagation"

var traceHeaders = []string{
	"traceparent",
	"tracestate",
	"x-request-id",
}

// CopyTraceHeaders copies W3C trace context headers from src to dst.
// Exported for unit testing.
func CopyTraceHeaders(dst, src http.Header) {
	for _, h := range traceHeaders {
		if v := src.Get(h); v != "" {
			dst.Set(h, v)
		}
	}
}

type proxyServer struct {
	egressURL string
}

func (s *proxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	req, err := http.NewRequest(r.Method, s.egressURL+r.RequestURI, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	CopyTraceHeaders(req.Header, r.Header)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body) //nolint:errcheck
}

func init() { Register() }

// Register wires the trace-propagation filter into Envoy via up.RegisterWithGroup.
// The filter itself is a no-op for HTTP — it starts the embedded proxy server.
func Register() {
	listenAddr := "127.0.0.1:9192"
	if v := os.Getenv("TRACE_PROPAGATION_LISTEN_ADDR"); v != "" {
		listenAddr = v
	}
	egressURL := "http://127.0.0.1:9193"
	if v := os.Getenv("TRACE_PROPAGATION_EGRESS_URL"); v != "" {
		egressURL = v
	}

	srv := &proxyServer{egressURL: egressURL}

	g := up.NewGroup()
	g.Add(
		func() error {
			ln, err := net.Listen("tcp", listenAddr)
			if err != nil {
				fmt.Fprintf(os.Stderr, "trace-propagation: listen %s: %v\n", listenAddr, err)
				return err
			}
			fmt.Fprintf(os.Stderr, "trace-propagation: listening on %s\n", ln.Addr())
			httpSrv := &http.Server{Handler: srv}
			return httpSrv.Serve(ln)
		},
		func() {},
	)

	up.RegisterWithGroup(ExtensionName, g, func(w *up.Writer, r *up.Request) {
		span := w.GetActiveSpan()
		if span == nil {
			return
		}
		span.SetOperation("trace-propagation.ingress")
		span.SetTag("http.method", r.Method)
		span.SetTag("http.path", r.Path)
	})
}
