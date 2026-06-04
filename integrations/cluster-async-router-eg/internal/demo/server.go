// Package demo provides the upstream HTTP server and request client used by
// the cluster-async-router-eg integration. There is no control plane: the host
// set is static, baked into the EnvoyPatchPolicy cluster config. The upstream
// echoes its own name (and the routing body it received) so the e2e can prove
// which host the dynamic-module cluster actually selected.
package demo

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"time"
)

// UpstreamResponse is what every upstream Pod returns. The e2e asserts on
// Upstream to confirm body-driven host selection landed on the right host.
type UpstreamResponse struct {
	Upstream string `json:"upstream"`
	Body     string `json:"body,omitempty"`
}

func RunUpstream(ctx context.Context, addr, name string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	// Accept any method/path. cluster-async-router POSTs with a JSON body but
	// we don't care what's in it from the upstream's perspective — the
	// dynamic-module filter has already parsed it by the time the request
	// arrives here.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var body string
		if r.Body != nil {
			raw, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			body = string(raw)
		}
		writeJSON(w, UpstreamResponse{Upstream: name, Body: body})
	})
	return runHTTP(ctx, addr, mux)
}

// RunUpstreamTLS runs the same JSON-echo upstream over HTTPS, but enforces
// that the client's TLS ClientHello carries SNI == expectedSNI. Mismatching
// SNI fails the handshake — Envoy reports upstream_cx_connect_fail and the
// downstream gets a 503. This is the tripwire that catches metadata→SNI
// regressions end-to-end: if HostSpec.Metadata stops reaching
// transport_socket_matches, no certificate is served and the test fails.
//
// A plaintext /healthz lives on healthzAddr so the kubelet readiness probe
// doesn't have to know how to do TLS-with-SNI.
func RunUpstreamTLS(ctx context.Context, addr, healthzAddr, name, expectedSNI, certFile, keyFile string) error {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return fmt.Errorf("load cert: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var body string
		if r.Body != nil {
			raw, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			body = string(raw)
		}
		writeJSON(w, UpstreamResponse{Upstream: name, Body: body})
	})

	tlsServer := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
				if chi.ServerName != expectedSNI {
					return nil, fmt.Errorf("upstream %s got SNI %q, want %q", name, chi.ServerName, expectedSNI)
				}
				return &cert, nil
			},
		},
	}

	healthzMux := http.NewServeMux()
	healthzMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	healthzServer := &http.Server{
		Addr:              healthzAddr,
		Handler:           healthzMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	tlsListener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen tls: %w", err)
	}

	errc := make(chan error, 2)
	go func() {
		log.Printf("listening (tls, sni=%s) on %s", expectedSNI, addr)
		errc <- tlsServer.ServeTLS(tlsListener, "", "")
	}()
	go func() {
		log.Printf("listening (healthz) on %s", healthzAddr)
		errc <- healthzServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tlsServer.Shutdown(shutdownCtx)
		_ = healthzServer.Shutdown(shutdownCtx)
		<-errc
		<-errc
		return nil
	case err := <-errc:
		_ = tlsServer.Close()
		_ = healthzServer.Close()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func runHTTP(ctx context.Context, addr string, handler http.Handler) error {
	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Printf("listening on %s", addr)
		errc <- server.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return err
		}
		err := <-errc
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write json response: %v", err)
	}
}
