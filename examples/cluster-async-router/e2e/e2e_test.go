// Package e2e runs integration tests for the cluster-async-router example
// against a real Envoy instance.
package e2e

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	_ "embed"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/examples/internal/e2etest"
)

//go:embed testdata/envoy.tmpl.yaml
var envoyConfigTmpl string

var (
	proxyURL      string
	examplesRoot  string
	upstreamAPort int
	upstreamBPort int
	upstreamCPort int
	upstreamDPort int
	publicEnabled bool
)

// publicEnvVar opts in to hitting real httpbin.org / example.com upstreams
// over the dynamic-module cluster. Off by default to keep CI hermetic; flip on
// when you want to confirm the per-host TLS path still works against real
// public endpoints.
const publicEnvVar = "E2E_PUBLIC"

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	examplesRoot = filepath.Join(filepath.Dir(file), "../../")

	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}

	if err := e2etest.CheckSharedLibrary(examplesRoot, "cluster-async-router", "libcluster-async-router.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	tlsDir, err := os.MkdirTemp("", "cluster-async-router-tls-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: mkdir tls: %v\n", err)
		os.Exit(1)
	}
	caPEM, caKey, err := genCA()
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: gen ca: %v\n", err)
		os.Exit(1)
	}
	caFile := filepath.Join(tlsDir, "ca.pem")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: write ca: %v\n", err)
		os.Exit(1)
	}
	certC, err := genServerCert("host-c.test", caPEM, caKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: gen cert c: %v\n", err)
		os.Exit(1)
	}
	certD, err := genServerCert("host-d.test", caPEM, caKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: gen cert d: %v\n", err)
		os.Exit(1)
	}

	upstreamAPort = startUpstream("upstream-a")
	upstreamBPort = startUpstream("upstream-b")
	// The expectedSNI here is the contract: the server REJECTS the handshake
	// unless Envoy's ClientHello carries this exact server_name. host-c is
	// configured with sni=host-c.test in the cluster config; if that metadata
	// stops reaching the transport_socket_matches selector, the handshake fails
	// and TestPost_bodyTargetsC_TLS fails. Same for host-d.
	upstreamCPort = startTLSUpstream("upstream-c", "host-c.test", certC)
	upstreamDPort = startTLSUpstream("upstream-d", "host-d.test", certD)

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)

	systemCA := ""
	if os.Getenv(publicEnvVar) != "" {
		systemCA = findSystemCA()
		if systemCA == "" {
			fmt.Fprintln(os.Stderr, "e2e: E2E_PUBLIC set but no system CA bundle found; skipping public hosts")
		} else {
			publicEnabled = true
		}
	}

	cfgPath := e2etest.WriteEnvoyConfig("cluster-async-router", envoyConfigTmpl, map[string]any{
		"ProxyPort":     proxyPort,
		"UpstreamAPort": upstreamAPort,
		"UpstreamBPort": upstreamBPort,
		"UpstreamCPort": upstreamCPort,
		"UpstreamDPort": upstreamDPort,
		"AdminPort":     adminPort,
		"CAFile":        caFile,
		"Public":        publicEnabled,
		"SystemCAFile":  systemCA,
	})

	exampleDir := filepath.Join(examplesRoot, "cluster-async-router")
	stop, ok := e2etest.StartEnvoy(bin, cfgPath, exampleDir, adminPort, nil)
	if !ok {
		os.RemoveAll(tlsDir) //nolint:errcheck
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready")

	code := m.Run()
	stop()
	os.RemoveAll(tlsDir) //nolint:errcheck
	os.Exit(code)
}

var testClient = &http.Client{Timeout: 5 * time.Second}

func post(t *testing.T, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, proxyURL+"/", bytes.NewBufferString(body))
	require.NoError(t, err)
	req.Header.Set("content-type", "application/json")
	resp, err := testClient.Do(req)
	require.NoError(t, err)
	return resp
}

func TestPost_bodyTargetsA(t *testing.T) {
	resp := post(t, `{"target":"a"}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "upstream-a")
}

func TestPost_bodyTargetsB(t *testing.T) {
	resp := post(t, `{"target":"b"}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "upstream-b")
}

func TestPost_bodyTargetsC_TLS(t *testing.T) {
	resp := post(t, `{"target":"c"}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "upstream-c")
}

func TestPost_bodyTargetsD_TLS(t *testing.T) {
	resp := post(t, `{"target":"d"}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "upstream-d")
}

// Public TLS assertions only check that we got a real HTTP response from the
// upstream (status < 500). Both httpbin.org and example.com reject POST / with
// 405 / 403 respectively, but the application-layer rejection proves the
// per-host SNI handshake and cert validation succeeded — a TLS failure would
// surface as Envoy 503 upstream_connection_failure instead.
func TestPost_bodyTargetsHTTPBin_PublicTLS(t *testing.T) {
	if !publicEnabled {
		t.Skipf("skipped: set %s=1 to exercise real public TLS endpoints", publicEnvVar)
	}
	resp := post(t, `{"target":"httpbin"}`)
	defer resp.Body.Close()
	require.Less(t, resp.StatusCode, 500, "TLS handshake to httpbin.org failed")
}

func TestPost_bodyTargetsExample_PublicTLS(t *testing.T) {
	if !publicEnabled {
		t.Skipf("skipped: set %s=1 to exercise real public TLS endpoints", publicEnvVar)
	}
	resp := post(t, `{"target":"example"}`)
	defer resp.Body.Close()
	require.Less(t, resp.StatusCode, 500, "TLS handshake to example.com failed")
}

func TestPost_unknownTarget_fails(t *testing.T) {
	resp := post(t, `{"target":"nope"}`)
	defer resp.Body.Close()
	require.NotEqual(t, http.StatusOK, resp.StatusCode)
}

func TestPost_missingTarget_fails(t *testing.T) {
	resp := post(t, `{}`)
	defer resp.Body.Close()
	require.NotEqual(t, http.StatusOK, resp.StatusCode)
}

func startUpstream(name string) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startUpstream: " + err.Error())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, name)
	})
	go http.Serve(l, mux) //nolint:errcheck
	return l.Addr().(*net.TCPAddr).Port
}

// startTLSUpstream returns the local port of an HTTPS server that REQUIRES the
// client to send SNI == expectedSNI. Without this check, the test would pass
// even if HostSpec.Metadata → transport_socket_matches plumbing regressed and
// Envoy sent the wrong SNI: the server would happily present its only cert,
// Envoy would validate its SAN against its own configured SNI, and we'd never
// notice. Enforcing the SNI in GetCertificate turns any mismatch into a TLS
// handshake failure (surfaces as Envoy upstream_cx_connect_fail / non-200).
func startTLSUpstream(name, expectedSNI string, cert tls.Certificate) int {
	// 127.0.0.1:0 lets the kernel pick a free port — recorded below for the
	// Envoy cluster config template.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startTLSUpstream: " + err.Error())
	}
	mux := http.NewServeMux()
	// Identifying body — assertions in the TLS test cases grep for this to
	// confirm the request reached the right upstream, not just *an* upstream.
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, name)
	})
	srv := &http.Server{
		Handler: mux,
		TLSConfig: &tls.Config{
			// TLS 1.2 floor — Envoy and Go default to 1.3 so this only matters
			// if a future change forces 1.0/1.1. Keeps the test safe under
			// older toolchains without locking out 1.3.
			MinVersion: tls.VersionTLS12,
			// GetCertificate runs once per handshake with the parsed
			// ClientHello. We use it as a tripwire on chi.ServerName (the SNI
			// extension): if Envoy's transport_socket_matches selector picked
			// the wrong UpstreamTlsContext — or never applied any sni metadata
			// at all — the server_name we see here won't match expectedSNI and
			// we fail the handshake instead of silently serving the cert.
			GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
				if chi.ServerName != expectedSNI {
					// Returning an error here aborts the TLS handshake. Envoy
					// reports this as upstream_cx_connect_fail and the
					// downstream gets a 503, which the test assertions catch.
					return nil, fmt.Errorf("upstream %s got SNI %q, want %q", name, chi.ServerName, expectedSNI)
				}
				return &cert, nil
			},
		},
	}
	go srv.ServeTLS(l, "", "") //nolint:errcheck
	return l.Addr().(*net.TCPAddr).Port
}

// findSystemCA returns the first system trust bundle that exists, or "" if
// none of the common locations are present. Used only for the opt-in public
// TLS tests.
func findSystemCA() string {
	for _, p := range []string{
		"/etc/ssl/cert.pem",                  // macOS, FreeBSD
		"/etc/ssl/certs/ca-certificates.crt", // Debian, Ubuntu, Alpine
		"/etc/pki/tls/certs/ca-bundle.crt",   // RHEL, CentOS, Fedora
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func genCA() (pemBytes []byte, key *ecdsa.PrivateKey, err error) {
	key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cluster-async-router-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), key, nil
}

func genServerCert(dnsName string, caPEM []byte, caKey *ecdsa.PrivateKey) (tls.Certificate, error) {
	caBlock, _ := pem.Decode(caPEM)
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		return tls.Certificate{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: dnsName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{dnsName},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
	}, nil
}
