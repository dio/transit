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
	"encoding/binary"
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
	"sync"
	"testing"
	"text/template"
	"time"

	"github.com/dio/transit/e2e/internal/grpctestproto"
	"github.com/dio/transit/e2e/sinks/accessloggersink"
	"github.com/dio/transit/e2e/sinks/alssink"
	"github.com/dio/transit/e2e/sinks/otelsink"
)

//go:embed testdata/envoy.tmpl.yaml
var envoyConfigTmpl string

var (
	echoAddr                      string
	metricsAddr                   string
	guardAddr                     string
	accessLoggerAddr              string
	correlatorAddr                string
	bodyAddr                      string
	mutableBodyAddr               string
	compressAddr                  string
	metadataAddr                  string
	tracerAddr                    string
	alsAddr                       string
	upstreamFilterAddr            string
	upstreamAuthAddr              string
	upstreamAuthGroupAddr         string
	lbPolicyAddr                  string
	clusterExtensionAddr          string
	clusterExtensionTLSAddr       string
	clusterExtensionMTLSAddr      string
	clusterSchedulerAddr          string
	asyncCalloutAddr              string
	asyncCalloutBodyAddr          string
	asyncCalloutLocalResponseAddr string
	mutableBodyUpstreamAddr       string
	grpcCalloutAddr               string
	lbPolicySelectionAddr         string
	accessLoggerLocalReplyAddr    string
	accessLoggerFlagsAddr         string
	embeddedServerAddr            string
	streamCompleteAddr            string
	streamCompleteLoopbackAddr    string
	streamFinalizedAddr           string
	streamFinalizedDeadAddr       string
	streamFinalizedLocalAddr      string
	streamFinalizedFallbackAddr   string
	streamFinalizedLoopbackAddr   string
	adminAddr                     string
)

var mutableBodyRecorder *recorderUpstream

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
	metricsPort := freePort()
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
	asyncCalloutBodyPort := freePort()
	asyncCalloutUpstreamPort := startAsyncCalloutUpstream()
	asyncCalloutForwardUpstreamPort := startForwardEchoUpstream()
	mutableBodyUpstreamPort := freePort()
	mutableBodyRecorder = startRecorderUpstream()
	asyncCalloutLocalResponsePort := freePort()
	grpcCalloutPort := freePort()
	grpcCalloutUpstreamPort := startGRPCCalloutUpstream()
	lbPolicySelectionPort := freePort()
	lbPolicyHost0Port := startIdentifiedUpstream("lb-host-0")
	lbPolicyHost1Port := startIdentifiedUpstream("lb-host-1")
	accessLoggerLocalReplyPort := freePort()
	accessLoggerFlagsPort := freePort()
	embeddedServerPort := freePort()
	embeddedServerLoopbackPort := freePort()
	streamCompletePort := freePort()
	streamCompleteLoopbackPort := freePort()
	streamFinalizedPort := freePort()
	streamFinalizedDeadPort := freePort()
	streamFinalizedLocalPort := freePort()
	streamFinalizedFallbackPort := freePort()
	streamFinalizedLoopbackPort := freePort()
	// deadUpstreamPort: nothing listens here; Envoy gets ECONNREFUSED, setting UF flag.
	deadUpstreamPort := freePort()
	adminPort := freePort()

	echoAddr = fmt.Sprintf("http://localhost:%d", echoPort)
	metricsAddr = fmt.Sprintf("http://localhost:%d", metricsPort)
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
	asyncCalloutBodyAddr = fmt.Sprintf("http://localhost:%d", asyncCalloutBodyPort)
	mutableBodyUpstreamAddr = fmt.Sprintf("http://localhost:%d", mutableBodyUpstreamPort)
	asyncCalloutLocalResponseAddr = fmt.Sprintf("http://localhost:%d", asyncCalloutLocalResponsePort)
	grpcCalloutAddr = fmt.Sprintf("http://localhost:%d", grpcCalloutPort)
	lbPolicySelectionAddr = fmt.Sprintf("http://localhost:%d", lbPolicySelectionPort)
	accessLoggerLocalReplyAddr = fmt.Sprintf("http://localhost:%d", accessLoggerLocalReplyPort)
	accessLoggerFlagsAddr = fmt.Sprintf("http://localhost:%d", accessLoggerFlagsPort)
	embeddedServerAddr = fmt.Sprintf("http://localhost:%d", embeddedServerPort)
	streamCompleteAddr = fmt.Sprintf("http://localhost:%d", streamCompletePort)
	streamCompleteLoopbackAddr = fmt.Sprintf("http://127.0.0.1:%d", streamCompleteLoopbackPort)
	streamFinalizedAddr = fmt.Sprintf("http://localhost:%d", streamFinalizedPort)
	streamFinalizedDeadAddr = fmt.Sprintf("http://localhost:%d", streamFinalizedDeadPort)
	streamFinalizedLocalAddr = fmt.Sprintf("http://localhost:%d", streamFinalizedLocalPort)
	streamFinalizedFallbackAddr = fmt.Sprintf("http://localhost:%d", streamFinalizedFallbackPort)
	streamFinalizedLoopbackAddr = fmt.Sprintf("http://127.0.0.1:%d", streamFinalizedLoopbackPort)
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
		MetricsPort:                        metricsPort,
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
		AsyncCalloutBodyPort:               asyncCalloutBodyPort,
		AsyncCalloutUpstreamPort:           asyncCalloutUpstreamPort,
		AsyncCalloutForwardUpstreamPort:    asyncCalloutForwardUpstreamPort,
		MutableBodyUpstreamPort:            mutableBodyUpstreamPort,
		MutableBodyRecorderPort:            mutableBodyRecorder.port,
		AsyncCalloutLocalResponsePort:      asyncCalloutLocalResponsePort,
		GRPCCalloutPort:                    grpcCalloutPort,
		GRPCCalloutUpstreamPort:            grpcCalloutUpstreamPort,
		LbPolicySelectionPort:              lbPolicySelectionPort,
		LbPolicyHost0Port:                  lbPolicyHost0Port,
		LbPolicyHost1Port:                  lbPolicyHost1Port,
		AccessLoggerLocalReplyPort:         accessLoggerLocalReplyPort,
		AccessLoggerFlagsPort:              accessLoggerFlagsPort,
		EmbeddedServerPort:                 embeddedServerPort,
		EmbeddedServerLoopbackPort:         embeddedServerLoopbackPort,
		StreamCompletePort:                 streamCompletePort,
		StreamCompleteLoopbackPort:         streamCompleteLoopbackPort,
		StreamFinalizedPort:                streamFinalizedPort,
		StreamFinalizedDeadPort:            streamFinalizedDeadPort,
		StreamFinalizedLocalPort:           streamFinalizedLocalPort,
		StreamFinalizedFallbackPort:        streamFinalizedFallbackPort,
		StreamFinalizedLoopbackPort:        streamFinalizedLoopbackPort,
		DeadUpstreamPort:                   deadUpstreamPort,
		AdminPort:                          adminPort,
	})

	envoyArgs := []string{
		"-c", cfgPath,
		"--log-level", "warning",
		"--component-log-level", "dynamic_modules:info",
	}
	if c := os.Getenv("ENVOY_CONCURRENCY"); c != "" {
		envoyArgs = append(envoyArgs, "--concurrency", c)
		fmt.Fprintf(os.Stderr, "e2e: envoy --concurrency %s\n", c)
	}
	envoyCmd = exec.Command(bin, envoyArgs...)
	envoyCmd.Env = append(os.Environ(),
		"GODEBUG=cgocheck=0",
		"ENVOY_DYNAMIC_MODULES_SEARCH_PATH="+projectRoot,
		fmt.Sprintf("E2E_EMBEDDED_SERVER_ADDR=127.0.0.1:%d", embeddedServerLoopbackPort),
		fmt.Sprintf("E2E_STREAM_COMPLETE_LOOPBACK_ADDR=127.0.0.1:%d", streamCompleteLoopbackPort),
		fmt.Sprintf("E2E_STREAM_FINALIZED_LOOPBACK_ADDR=127.0.0.1:%d", streamFinalizedLoopbackPort),
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
// "hello compression" compressed with gzip, regardless of Accept-Encoding. Returns
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
		gz.Write([]byte("hello compression"))
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
		w.Header().Set("x-upstream-source", "plain")
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

// startGRPCCalloutUpstream starts a minimal h2c gRPC server for the GRPCCallout
// e2e tests. It handles /e2e.Echo/Echo by decoding a framed EchoRequest proto
// and returning a framed EchoResponse proto with grpc-status: 0 in the trailers.
func startGRPCCalloutUpstream() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startGRPCCalloutUpstream: " + err.Error())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/e2e.Echo/Echo", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(body) < 5 || body[0] != 0 {
			http.Error(w, "invalid grpc frame", http.StatusBadRequest)
			return
		}
		msgLen := binary.BigEndian.Uint32(body[1:5])
		end := 5 + int(msgLen)
		if end < 5 || end > len(body) {
			http.Error(w, "truncated grpc frame", http.StatusBadRequest)
			return
		}
		var echoReq grpctestproto.EchoRequest
		if err := echoReq.UnmarshalProto(body[5:end]); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		echoResp := grpctestproto.EchoResponse{
			Text:     echoReq.Text,
			Sequence: echoReq.Sequence + 1,
		}
		msg := echoResp.MarshalProto(nil)
		frame := make([]byte, 5+len(msg))
		binary.BigEndian.PutUint32(frame[1:5], uint32(len(msg)))
		copy(frame[5:], msg)

		w.Header().Set("Content-Type", "application/grpc+proto")
		w.Header().Set("Trailer", "Grpc-Status")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(frame)
		w.Header().Set("Grpc-Status", "0")
	})
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{
		Handler:   mux,
		Protocols: protocols,
	}
	go srv.Serve(l) //nolint:errcheck
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

// startIdentifiedUpstream starts a minimal HTTP server that always responds with
// the given body string. Useful for multi-host LB policy tests where each host
// must return a distinguishable response.
func startIdentifiedUpstream(body string) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startIdentifiedUpstream: " + err.Error())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	})
	go http.Serve(l, mux) //nolint:errcheck
	return l.Addr().(*net.TCPAddr).Port
}

type recordedRequest struct {
	Method        string
	Path          string
	Headers       http.Header
	Body          []byte
	ContentLength int64 // from r.ContentLength, not r.Header — Go's server parses it separately
}

type recorderUpstream struct {
	port    int
	mu      sync.Mutex
	reqs    []recordedRequest
	arrived chan struct{}
}

// startRecorderUpstream starts an HTTP server that records every inbound request.
// Use WaitFor to block until n requests arrive, Len for an immediate count, and
// Reset to clear state between test cases.
func startRecorderUpstream() *recorderUpstream {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startRecorderUpstream: " + err.Error())
	}
	r := &recorderUpstream{
		port:    l.Addr().(*net.TCPAddr).Port,
		arrived: make(chan struct{}, 256),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.reqs = append(r.reqs, recordedRequest{
			Method:        req.Method,
			Path:          req.URL.Path,
			Headers:       req.Header.Clone(),
			Body:          body,
			ContentLength: req.ContentLength,
		})
		r.mu.Unlock()
		select {
		case r.arrived <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("recorder ok"))
	})
	go http.Serve(l, mux) //nolint:errcheck
	return r
}

// WaitFor blocks until at least n requests are recorded or the test fails with a timeout.
func (r *recorderUpstream) WaitFor(t *testing.T, n int, timeout time.Duration) []recordedRequest {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		r.mu.Lock()
		got := len(r.reqs)
		r.mu.Unlock()
		if got >= n {
			return r.Requests()
		}
		select {
		case <-r.arrived:
		case <-deadline.C:
			r.mu.Lock()
			got = len(r.reqs)
			r.mu.Unlock()
			if got >= n {
				return r.Requests()
			}
			t.Fatalf("recorderUpstream.WaitFor: timeout after %v: want %d requests, got %d", timeout, n, got)
			return nil
		}
	}
}

// Requests returns a snapshot of all captured requests.
func (r *recorderUpstream) Requests() []recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedRequest, len(r.reqs))
	copy(out, r.reqs)
	return out
}

// Len returns the current request count without blocking.
func (r *recorderUpstream) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

// WaitForNone waits for duration and fails the test if any request arrives
// during that window. Use this for negative assertions where an immediate
// Len()==0 check would race against a delayed upstream forward.
func (r *recorderUpstream) WaitForNone(t *testing.T, duration time.Duration) {
	t.Helper()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-r.arrived:
		t.Fatalf("recorderUpstream.WaitForNone: unexpected request received within %v", duration)
	case <-timer.C:
		// Re-check after timer expiry: under a busy scheduler both channels may
		// become ready before this goroutine is scheduled, and Go's select picks
		// one uniformly at random — the timer branch may win even though a request
		// was recorded during the wait window.
		if r.Len() > 0 {
			t.Fatalf("recorderUpstream.WaitForNone: unexpected request received within %v", duration)
		}
	}
}

// Reset clears all captured requests.
func (r *recorderUpstream) Reset() {
	r.mu.Lock()
	r.reqs = r.reqs[:0]
	r.mu.Unlock()
	for {
		select {
		case <-r.arrived:
		default:
			return
		}
	}
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
	MetricsPort                        int
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
	AsyncCalloutBodyPort               int
	AsyncCalloutUpstreamPort           int
	AsyncCalloutForwardUpstreamPort    int
	MutableBodyUpstreamPort            int
	MutableBodyRecorderPort            int
	AsyncCalloutLocalResponsePort      int
	GRPCCalloutPort                    int
	GRPCCalloutUpstreamPort            int
	LbPolicySelectionPort              int
	LbPolicyHost0Port                  int
	LbPolicyHost1Port                  int
	AccessLoggerLocalReplyPort         int
	AccessLoggerFlagsPort              int
	EmbeddedServerPort                 int
	EmbeddedServerLoopbackPort         int
	StreamCompletePort                 int
	StreamCompleteLoopbackPort         int
	StreamFinalizedPort                int
	StreamFinalizedDeadPort            int
	StreamFinalizedLocalPort           int
	StreamFinalizedFallbackPort        int
	StreamFinalizedLoopbackPort        int
	DeadUpstreamPort                   int
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
