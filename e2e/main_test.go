// Package e2e runs integration tests against a real Envoy binary.
//
// TestMain builds a combined .so from all e2e filters (echo, guard, e2e-logger),
// starts the access log sink, starts Envoy, and tears everything down when done.
//
// Prerequisites:
//   - Envoy binary at .bin/envoy (run: make download-envoy) or set ENVOY_BIN
//
// Run:
//
//	make e2e
//
// Or manually:
//
//	ENVOY_BIN=.bin/envoy go test ./e2e/... -v -timeout=30s
//
// Tests skip automatically when ENVOY_BIN is not present.
// Set TRANSIT_SKIP_BUILD=1 to reuse a previously built .so (faster iteration).
package e2e

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/rsa"
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
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/dio/transit/e2e/sinks/accessloggersink"
	"github.com/dio/transit/e2e/sinks/alssink"
	"github.com/dio/transit/e2e/sinks/otelsink"
)

//go:embed testdata/envoy.tmpl.yaml
var envoyConfigTmpl string

var (
	echoAddr                 string
	guardAddr                string
	accessLoggerAddr         string
	correlatorAddr           string
	bodyAddr                 string
	mutableBodyAddr          string
	compressAddr             string
	metadataAddr             string
	tracerAddr               string
	alsAddr                  string
	upstreamFilterAddr       string
	upstreamAuthAddr         string
	upstreamAuthGroupAddr    string
	lbPolicyAddr             string
	clusterExtensionAddr     string
	clusterExtensionTLSAddr  string
	clusterExtensionMTLSAddr string
	clusterSchedulerAddr     string
	asyncCalloutAddr         string
	adminAddr                string
)

var otelSink *otelsink.Sink
var alsSink *alssink.Sink

var (
	envoyCmd    *exec.Cmd
	projectRoot string
)

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	projectRoot = filepath.Dir(file)

	bin := envoyBin()
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}

	echoPort := freePort()
	guardPort := freePort()
	accessLoggerPort := freePort()
	correlatorPort := freePort()
	bodyPort := freePort()
	mutableBodyPort := freePort()
	compressPort := freePort()
	compressUpstreamPort := startGzipUpstream()
	metadataPort := freePort()
	tracerPort := freePort()
	alsPort := freePort()
	upstreamFilterPort := freePort()
	upstreamAuthPort := freePort()
	upstreamAuthGroupPort := freePort()
	upstreamFilterUpstreamPort := startPlainUpstream()
	lbPolicyPort := freePort()
	clusterExtensionPort := freePort()
	clusterExtensionTLSPort := freePort()
	clusterExtensionTLSUpstream, cleanupTLS := startTLSUpstream(clusterExtensionTLSServerName, false)
	clusterExtensionMTLSPort := freePort()
	clusterExtensionMTLSUpstream, cleanupMTLS := startTLSUpstream(clusterExtensionMTLSServerName, true)
	cleanupTLSUpstreams := func() {
		cleanupTLS()
		cleanupMTLS()
	}
	clusterSchedulerPort := freePort()
	asyncCalloutPort := freePort()
	asyncCalloutUpstreamPort := startAsyncCalloutUpstream()
	asyncCalloutForwardUpstreamPort := startForwardEchoUpstream()
	adminPort := freePort()

	echoAddr = fmt.Sprintf("http://localhost:%d", echoPort)
	guardAddr = fmt.Sprintf("http://localhost:%d", guardPort)
	accessLoggerAddr = fmt.Sprintf("http://localhost:%d", accessLoggerPort)
	correlatorAddr = fmt.Sprintf("http://localhost:%d", correlatorPort)
	bodyAddr = fmt.Sprintf("http://localhost:%d", bodyPort)
	mutableBodyAddr = fmt.Sprintf("http://localhost:%d", mutableBodyPort)
	compressAddr = fmt.Sprintf("http://localhost:%d", compressPort)
	metadataAddr = fmt.Sprintf("http://localhost:%d", metadataPort)
	tracerAddr = fmt.Sprintf("http://localhost:%d", tracerPort)
	alsAddr = fmt.Sprintf("http://localhost:%d", alsPort)
	upstreamFilterAddr = fmt.Sprintf("http://localhost:%d", upstreamFilterPort)
	upstreamAuthAddr = fmt.Sprintf("http://localhost:%d", upstreamAuthPort)
	upstreamAuthGroupAddr = fmt.Sprintf("http://localhost:%d", upstreamAuthGroupPort)
	lbPolicyAddr = fmt.Sprintf("http://localhost:%d", lbPolicyPort)
	clusterExtensionAddr = fmt.Sprintf("http://localhost:%d", clusterExtensionPort)
	clusterExtensionTLSAddr = fmt.Sprintf("http://localhost:%d", clusterExtensionTLSPort)
	clusterExtensionMTLSAddr = fmt.Sprintf("http://localhost:%d", clusterExtensionMTLSPort)
	clusterSchedulerAddr = fmt.Sprintf("http://localhost:%d", clusterSchedulerPort)
	asyncCalloutAddr = fmt.Sprintf("http://localhost:%d", asyncCalloutPort)
	adminAddr = fmt.Sprintf("http://localhost:%d", adminPort)

	otelSink = otelsink.New()
	otelSinkPort := otelSink.Start()
	fmt.Fprintf(os.Stderr, "e2e: OTLP sink at port %d\n", otelSinkPort)

	alsSink = alssink.New()
	alsSinkPort := alsSink.Start()
	fmt.Fprintf(os.Stderr, "e2e: ALS sink at port %d\n", alsSinkPort)

	sinkURL := accessloggersink.StartSink()
	fmt.Fprintf(os.Stderr, "e2e: access logger sink at %s\n", sinkURL)

	soPath := filepath.Join(projectRoot, "libe2e.so")

	if os.Getenv("TRANSIT_SKIP_BUILD") == "" {
		fmt.Fprintln(os.Stderr, "e2e: building libe2e.so ...")
		cmd := exec.Command("go", "build", "-trimpath", "-buildmode=c-shared", "-o", soPath, "./cmd")
		cmd.Dir = projectRoot
		cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			cleanupTLSUpstreams()
			fmt.Fprintf(os.Stderr, "e2e: build failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "e2e: build OK")
	} else {
		if _, err := os.Stat(soPath); err != nil {
			cleanupTLSUpstreams()
			fmt.Fprintf(os.Stderr, "e2e: TRANSIT_SKIP_BUILD=1 but libe2e.so not found at %s\n", soPath)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "e2e: reusing existing libe2e.so (TRANSIT_SKIP_BUILD=1)")
	}

	cfgPath := writeEnvoyConfig(envoyPorts{
		SinkURL:                            sinkURL,
		EchoPort:                           echoPort,
		GuardPort:                          guardPort,
		AccessLoggerPort:                   accessLoggerPort,
		CorrelatorPort:                     correlatorPort,
		BodyPort:                           bodyPort,
		MutableBodyPort:                    mutableBodyPort,
		CompressPort:                       compressPort,
		CompressUpstreamPort:               compressUpstreamPort,
		OtelSinkPort:                       otelSinkPort,
		MetadataPort:                       metadataPort,
		TracerPort:                         tracerPort,
		AlsPort:                            alsPort,
		AlsSinkPort:                        alsSinkPort,
		UpstreamFilterPort:                 upstreamFilterPort,
		UpstreamAuthPort:                   upstreamAuthPort,
		UpstreamAuthGroupPort:              upstreamAuthGroupPort,
		UpstreamFilterUpstreamPort:         upstreamFilterUpstreamPort,
		LbPolicyPort:                       lbPolicyPort,
		LbPolicyUpstreamPort:               upstreamFilterUpstreamPort,
		ClusterExtensionPort:               clusterExtensionPort,
		ClusterExtensionUpstreamPort:       upstreamFilterUpstreamPort,
		ClusterExtensionTLSPort:            clusterExtensionTLSPort,
		ClusterExtensionTLSUpstreamPort:    clusterExtensionTLSUpstream.port,
		ClusterExtensionTLSCAPath:          clusterExtensionTLSUpstream.caPath,
		ClusterExtensionMTLSPort:           clusterExtensionMTLSPort,
		ClusterExtensionMTLSUpstreamPort:   clusterExtensionMTLSUpstream.port,
		ClusterExtensionMTLSCAPath:         clusterExtensionMTLSUpstream.caPath,
		ClusterExtensionMTLSClientCertPath: clusterExtensionMTLSUpstream.clientCertPath,
		ClusterExtensionMTLSClientKeyPath:  clusterExtensionMTLSUpstream.clientKeyPath,
		ClusterSchedulerPort:               clusterSchedulerPort,
		AsyncCalloutPort:                   asyncCalloutPort,
		AsyncCalloutUpstreamPort:           asyncCalloutUpstreamPort,
		AsyncCalloutForwardUpstreamPort:    asyncCalloutForwardUpstreamPort,
		AdminPort:                          adminPort,
	})

	envoyCmd = exec.Command(bin,
		"-c", cfgPath,
		"--log-level", "warning",
		"--component-log-level", "dynamic_modules:info",
	)
	envoyCmd.Env = append(os.Environ(),
		"GODEBUG=cgocheck=0",
		"ENVOY_DYNAMIC_MODULES_SEARCH_PATH="+projectRoot,
	)
	envoyCmd.Stdout = os.Stderr
	envoyCmd.Stderr = os.Stderr

	if err := envoyCmd.Start(); err != nil {
		os.Remove(cfgPath)
		cleanupTLSUpstreams()
		fmt.Fprintf(os.Stderr, "e2e: envoy start failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "e2e: envoy pid=%d\n", envoyCmd.Process.Pid)

	if !waitReady(15 * time.Second) {
		envoyCmd.Process.Kill()
		envoyCmd.Wait()
		os.Remove(cfgPath)
		cleanupTLSUpstreams()
		fmt.Fprintln(os.Stderr, "e2e: envoy not ready in time")
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready")

	code := m.Run()

	envoyCmd.Process.Kill()
	envoyCmd.Wait()
	os.Remove(cfgPath)
	cleanupTLSUpstreams()
	os.Exit(code)
}

func envoyBin() string {
	if b := os.Getenv("ENVOY_BIN"); b != "" {
		return b
	}
	return filepath.Join(projectRoot, "..", ".bin", "envoy")
}

// freePort asks the OS for an unused TCP port and returns its number.
// There is an inherent TOCTOU gap between closing the listener and Envoy
// binding the port, but in practice this is reliable in isolated test
// environments.
func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("freePort: " + err.Error())
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startGzipUpstream starts a minimal HTTP server that always returns the text
// "hello codec" compressed with gzip, regardless of Accept-Encoding. Returns
// the port it is listening on.
func startGzipUpstream() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startGzipUpstream: " + err.Error())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		gz.Write([]byte("hello codec"))
		gz.Close()
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write(buf.Bytes())
	})
	go http.Serve(l, mux)
	return l.Addr().(*net.TCPAddr).Port
}

// startPlainUpstream starts a minimal HTTP server that always returns 200 with
// body "upstream ok". Returns the port it is listening on.
func startPlainUpstream() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startPlainUpstream: " + err.Error())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" {
			w.Header().Set("x-received-authorization", auth)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("upstream ok"))
	})
	go http.Serve(l, mux)
	return l.Addr().(*net.TCPAddr).Port
}

// startAsyncCalloutUpstream returns the final path segment for each request so
// async callout tests can verify multiple responses were merged.
func startAsyncCalloutUpstream() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startAsyncCalloutUpstream: " + err.Error())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.TrimPrefix(r.URL.Path, "/")))
	})
	go http.Serve(l, mux) //nolint:errcheck
	return l.Addr().(*net.TCPAddr).Port
}

// startForwardEchoUpstream starts an upstream that echoes received request
// headers as "x-received-<lowercase-name>" response headers and returns the
// request body. Used to verify that filter mutations reached upstream.
func startForwardEchoUpstream() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startForwardEchoUpstream: " + err.Error())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		for name, values := range r.Header {
			w.Header().Set("x-received-"+strings.ToLower(name), values[0])
		}
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
	go http.Serve(l, mux) //nolint:errcheck
	return l.Addr().(*net.TCPAddr).Port
}

const (
	clusterExtensionTLSServerName  = "cluster-tls.local"
	clusterExtensionMTLSServerName = "cluster-mtls.local"
)

type tlsUpstream struct {
	port           int
	caPath         string
	clientCertPath string
	clientKeyPath  string
}

// startTLSUpstream starts a local HTTPS upstream and writes the certificate
// material Envoy needs. When requireClientCert is true, the upstream refuses the
// handshake unless Envoy presents the generated client certificate.
func startTLSUpstream(serverName string, requireClientCert bool) (tlsUpstream, func()) {
	dir, err := os.MkdirTemp("", "transit-e2e-tls-*")
	if err != nil {
		panic("startTLSUpstream: " + err.Error())
	}
	material := generateLocalTLSMaterial(serverName)
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, material.caPEM, 0o600); err != nil {
		os.RemoveAll(dir)
		panic("startTLSUpstream: " + err.Error())
	}
	clientCertPath := filepath.Join(dir, "client.pem")
	clientKeyPath := filepath.Join(dir, "client-key.pem")
	if requireClientCert {
		if err := os.WriteFile(clientCertPath, material.clientCertPEM, 0o600); err != nil {
			os.RemoveAll(dir)
			panic("startTLSUpstream: " + err.Error())
		}
		if err := os.WriteFile(clientKeyPath, material.clientKeyPEM, 0o600); err != nil {
			os.RemoveAll(dir)
			panic("startTLSUpstream: " + err.Error())
		}
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.RemoveAll(dir)
		panic("startTLSUpstream: " + err.Error())
	}
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.TLS == nil {
				http.Error(w, "request did not use TLS", http.StatusInternalServerError)
				return
			}
			w.Header().Set("content-type", "text/plain")
			w.WriteHeader(http.StatusOK)
			clientCN := ""
			if len(r.TLS.PeerCertificates) > 0 {
				clientCN = r.TLS.PeerCertificates[0].Subject.CommonName
			}
			fmt.Fprintf(w, "tls upstream ok sni=%s client=%s", r.TLS.ServerName, clientCN)
		}),
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{material.serverCert},
			MinVersion:   tls.VersionTLS12,
		},
	}
	if requireClientCert {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(material.caPEM) {
			os.RemoveAll(dir)
			panic("startTLSUpstream: failed to load client CA")
		}
		server.TLSConfig.ClientAuth = tls.RequireAndVerifyClientCert
		server.TLSConfig.ClientCAs = pool
	}
	go server.Serve(tls.NewListener(l, server.TLSConfig)) //nolint:errcheck

	cleanup := func() {
		server.Close()
		os.RemoveAll(dir)
	}
	return tlsUpstream{
		port:           l.Addr().(*net.TCPAddr).Port,
		caPath:         caPath,
		clientCertPath: clientCertPath,
		clientKeyPath:  clientKeyPath,
	}, cleanup
}

type tlsMaterial struct {
	serverCert    tls.Certificate
	caPEM         []byte
	clientCertPEM []byte
	clientKeyPEM  []byte
}

func generateLocalTLSMaterial(serverName string) tlsMaterial {
	now := time.Now()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic("generateLocalTLSMaterial ca key: " + err.Error())
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "transit e2e test ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		panic("generateLocalTLSMaterial ca cert: " + err.Error())
	}

	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic("generateLocalTLSMaterial server key: " + err.Error())
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: serverName},
		DNSNames:     []string{serverName},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caTemplate, &serverKey.PublicKey, caKey)
	if err != nil {
		panic("generateLocalTLSMaterial server cert: " + err.Error())
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(serverKey)})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		panic("generateLocalTLSMaterial server key pair: " + err.Error())
	}

	clientKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic("generateLocalTLSMaterial client key: " + err.Error())
	}
	clientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "transit-e2e-client"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, caTemplate, &clientKey.PublicKey, caKey)
	if err != nil {
		panic("generateLocalTLSMaterial client cert: " + err.Error())
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	return tlsMaterial{
		serverCert:    cert,
		caPEM:         caPEM,
		clientCertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER}),
		clientKeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(clientKey)}),
	}
}

func waitReady(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(adminAddr + "/ready")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

type envoyPorts struct {
	SinkURL                            string
	EchoPort                           int
	GuardPort                          int
	AccessLoggerPort                   int
	CorrelatorPort                     int
	BodyPort                           int
	MutableBodyPort                    int
	CompressPort                       int
	CompressUpstreamPort               int
	OtelSinkPort                       int
	MetadataPort                       int
	TracerPort                         int
	AlsPort                            int
	AlsSinkPort                        int
	UpstreamFilterPort                 int
	UpstreamAuthPort                   int
	UpstreamAuthGroupPort              int
	UpstreamFilterUpstreamPort         int
	LbPolicyPort                       int
	LbPolicyUpstreamPort               int
	ClusterExtensionPort               int
	ClusterExtensionUpstreamPort       int
	ClusterExtensionTLSPort            int
	ClusterExtensionTLSUpstreamPort    int
	ClusterExtensionTLSCAPath          string
	ClusterExtensionMTLSPort           int
	ClusterExtensionMTLSUpstreamPort   int
	ClusterExtensionMTLSCAPath         string
	ClusterExtensionMTLSClientCertPath string
	ClusterExtensionMTLSClientKeyPath  string
	ClusterSchedulerPort               int
	AsyncCalloutPort                   int
	AsyncCalloutUpstreamPort           int
	AsyncCalloutForwardUpstreamPort    int
	AdminPort                          int
}

func writeEnvoyConfig(p envoyPorts) string {
	tmpl := template.Must(template.New("envoy").Parse(envoyConfigTmpl))
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, p); err != nil {
		panic("writeEnvoyConfig: " + err.Error())
	}
	f, err := os.CreateTemp("", "transit-e2e-*.yaml")
	if err != nil {
		panic(err)
	}
	buf.WriteTo(f)
	f.Close()
	return f.Name()
}

// helpers

func mustDo(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}
