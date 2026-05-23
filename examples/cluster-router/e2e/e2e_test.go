// Package e2e runs the cluster-router example against a real Envoy process.
package e2e

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	_ "embed"
	"encoding/json"
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
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/examples/internal/e2etest"
)

//go:embed testdata/envoy.tmpl.yaml
var envoyConfigTmpl string

var (
	proxyURL      string
	envoyCmd      *exec.Cmd
	examplesRoot  string
	control       *configServer
	upstreamA     *upstreamServer
	upstreamB     *upstreamServer
	upstreamC     *upstreamServer
	httpsProvider *upstreamServer
)

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	examplesRoot = filepath.Join(filepath.Dir(file), "../../")

	bin := e2etest.EnvoyBin(examplesRoot)
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: envoy not found at %s (run: make download-envoy)\n", bin)
		os.Exit(0)
	}

	exampleDir := filepath.Join(examplesRoot, "cluster-router")
	if err := e2etest.CheckSharedLibrary(examplesRoot, "cluster-router", "libcluster-router.so"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	upstreamA = startUpstream("upstream a")
	upstreamB = startUpstream("upstream b")
	upstreamC = startUpstream("upstream c")
	var cleanupHTTPSProvider func()
	httpsProvider, cleanupHTTPSProvider = startHTTPSProvider("https provider", "provider.local")
	initial := snapshot{
		Version: "initial",
		Models: map[string]model{
			"gpt-fast": {
				Target:     upstreamA.target(),
				Provider:   "openai",
				AuthHeader: "Bearer openai-token",
			},
			"claude-safe": {
				Target:     upstreamB.target(),
				Provider:   "anthropic",
				AuthHeader: "Bearer anthropic-token",
			},
		},
	}
	control = startConfigServer(initial)

	proxyPort := e2etest.FreePort()
	adminPort := e2etest.FreePort()
	proxyURL = fmt.Sprintf("http://127.0.0.1:%d", proxyPort)
	clusterConfigJSON := marshalJSON(map[string]any{
		"config_url":     control.url(),
		"refresh_millis": 100,
		"timeout_millis": 500,
		"initial":        initial,
	})
	httpsProviderInitial := snapshot{
		Version: "https-provider",
		Models: map[string]model{
			"gpt-secure": {
				Target:     httpsProvider.target(),
				Provider:   "openai",
				AuthHeader: "Bearer https-provider-token",
			},
		},
	}
	httpsProviderClusterConfigJSON := marshalJSON(map[string]any{
		"scope":          "https-provider",
		"timeout_millis": 500,
		"initial":        httpsProviderInitial,
	})

	cfgPath := e2etest.WriteEnvoyConfig("cluster-router-e2e", envoyConfigTmpl, envoyConfigData{
		ProxyPort:                      proxyPort,
		AdminPort:                      adminPort,
		ClusterConfigJSON:              clusterConfigJSON,
		HTTPSProviderClusterConfigJSON: httpsProviderClusterConfigJSON,
		HTTPSProviderCAPath:            httpsProvider.caPath,
	})

	envoyCmd = exec.Command(bin, "-c", cfgPath, "--log-level", "warning",
		"--component-log-level", "dynamic_modules:info")
	envoyCmd.Env = append(os.Environ(),
		"GODEBUG=cgocheck=0",
		"ENVOY_DYNAMIC_MODULES_SEARCH_PATH="+exampleDir,
	)
	envoyCmd.Stdout = os.Stderr
	envoyCmd.Stderr = os.Stderr
	if err := envoyCmd.Start(); err != nil {
		os.Remove(cfgPath)
		cleanupHTTPSProvider()
		fmt.Fprintf(os.Stderr, "e2e: envoy start failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "e2e: envoy pid=%d\n", envoyCmd.Process.Pid)

	adminURL := fmt.Sprintf("http://127.0.0.1:%d", adminPort)
	if !e2etest.WaitURL(adminURL+"/ready", 15*time.Second) {
		envoyCmd.Process.Kill()
		envoyCmd.Wait()
		os.Remove(cfgPath)
		cleanupHTTPSProvider()
		fmt.Fprintln(os.Stderr, "e2e: envoy not ready in time")
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready")

	code := m.Run()

	envoyCmd.Process.Kill()
	envoyCmd.Wait()
	os.Remove(cfgPath)
	cleanupHTTPSProvider()
	os.Exit(code)
}

func TestClusterRouterEndToEnd(t *testing.T) {
	t.Run("routes initial models and injects upstream headers", func(t *testing.T) {
		requireModel(t, "gpt-fast", "upstream a")
		requireLastRequest(t, upstreamA, observedRequest{
			Auth:     "Bearer openai-token",
			Provider: "openai",
			Version:  "initial",
		})

		requireModel(t, "claude-safe", "upstream b")
		requireLastRequest(t, upstreamB, observedRequest{
			Auth:     "Bearer anthropic-token",
			Provider: "anthropic",
			Version:  "initial",
		})
	})

	t.Run("dumps active config without secrets", func(t *testing.T) {
		body := requireDebugDump(t)
		for _, want := range []string{"gpt-fast", "claude-safe", "initial"} {
			require.Contains(t, body, want)
		}
		require.NotContains(t, body, "Bearer")
	})

	t.Run("rejects unknown models", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, proxyURL+"/", nil)
		require.NoError(t, err)
		req.Header.Set("x-model", "unknown-model")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.NotEqual(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("refreshes config and adds a new upstream", func(t *testing.T) {
		control.set(snapshot{
			Version: "updated",
			Models: map[string]model{
				"gpt-fast": {
					Target:     upstreamA.target(),
					Provider:   "openai",
					AuthHeader: "Bearer openai-token",
				},
				"claude-safe": {
					Target:     upstreamB.target(),
					Provider:   "anthropic",
					AuthHeader: "Bearer anthropic-token",
				},
				// gpt-slow deliberately reuses upstream A. This proves model
				// additions are not forced to create a fresh host every time.
				"gpt-slow": {
					Target:     upstreamA.target(),
					Provider:   "openai",
					AuthHeader: "Bearer slow-token",
				},
				// kimi-fast points at a host Envoy did not know at bootstrap.
				// The cluster extension resolves and adds it from the refreshed JSON.
				"kimi-fast": {
					Target:     upstreamC.target(),
					Provider:   "moonshot",
					AuthHeader: "Bearer moonshot-token",
				},
			},
		})

		eventually(t, 10*time.Second, func() bool {
			body, status, err := modelRequest("gpt-slow")
			return err == nil && status == http.StatusOK && strings.Contains(body, "upstream a")
		})
		requireLastRequest(t, upstreamA, observedRequest{
			Auth:     "Bearer slow-token",
			Provider: "openai",
			Version:  "updated",
		})

		eventually(t, 10*time.Second, func() bool {
			body, status, err := modelRequest("kimi-fast")
			return err == nil && status == http.StatusOK && strings.Contains(body, "upstream c")
		})
		requireLastRequest(t, upstreamC, observedRequest{
			Auth:     "Bearer moonshot-token",
			Provider: "moonshot",
			Version:  "updated",
		})

		body := requireDebugDump(t)
		for _, want := range []string{"gpt-slow", "kimi-fast", "updated"} {
			require.Contains(t, body, want)
		}
		require.NotContains(t, body, "Bearer")
	})

	t.Run("egresses to an HTTPS provider with SNI and validation", func(t *testing.T) {
		body, status, err := modelRequestPath("gpt-secure", "/https-provider/v1/chat/completions")
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, status)
		require.Contains(t, body, "https provider")
		requireLastRequest(t, httpsProvider, observedRequest{
			Auth:     "Bearer https-provider-token",
			Provider: "openai",
			Version:  "https-provider",
			SNI:      "provider.local",
		})
	})
}

func requireModel(t *testing.T, model string, want string) {
	t.Helper()
	body, status, err := modelRequest(model)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, want)
}

func modelRequest(model string) (string, int, error) {
	return modelRequestPath(model, "/v1/chat/completions")
}

func modelRequestPath(model string, path string) (string, int, error) {
	req, err := http.NewRequest(http.MethodPost, proxyURL+path, bytes.NewBufferString(`{"model":"`+model+`"}`))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-model", model)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body), resp.StatusCode, nil
}

func requireDebugDump(t *testing.T) string {
	t.Helper()
	resp, err := http.Get(proxyURL + "/__cluster-router/config") //nolint:noctx
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	return string(body)
}

func requireLastRequest(t *testing.T, upstream *upstreamServer, want observedRequest) {
	t.Helper()
	got, ok := upstream.last()
	require.True(t, ok, "%s saw no request", upstream.label)
	require.Equal(t, want, got, "%s headers", upstream.label)
}

func eventually(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	require.Eventually(t, fn, timeout, 100*time.Millisecond, "active config: %s", requireDebugDump(t))
}

type snapshot struct {
	Version string           `json:"version"`
	Models  map[string]model `json:"models"`
}

type model struct {
	Target     string `json:"target"`
	Provider   string `json:"provider"`
	AuthHeader string `json:"auth_header"`
}

type configServer struct {
	mu       sync.RWMutex
	body     []byte
	listener net.Listener
}

func startConfigServer(initial snapshot) *configServer {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startConfigServer: " + err.Error())
	}
	s := &configServer{listener: l}
	s.set(initial)

	mux := http.NewServeMux()
	mux.HandleFunc("/routes.json", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.RLock()
		defer s.mu.RUnlock()
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(s.body)
	})
	go http.Serve(l, mux) //nolint:errcheck
	return s
}

func (s *configServer) set(cfg snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.body = []byte(marshalJSON(cfg))
}

func (s *configServer) url() string {
	return "http://" + s.listener.Addr().String() + "/routes.json"
}

type upstreamServer struct {
	label    string
	port     int
	caPath   string
	mu       sync.Mutex
	requests []observedRequest
}

type observedRequest struct {
	Auth     string
	Provider string
	Version  string
	SNI      string
}

func startUpstream(label string) *upstreamServer {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("startUpstream: " + err.Error())
	}
	s := &upstreamServer{label: label, port: l.Addr().(*net.TCPAddr).Port}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		seen := observedRequest{
			Auth:     r.Header.Get("authorization"),
			Provider: r.Header.Get("x-llm-provider"),
			Version:  r.Header.Get("x-cluster-router-version"),
		}
		s.mu.Lock()
		s.requests = append(s.requests, seen)
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(label))
	})
	go http.Serve(l, mux) //nolint:errcheck
	return s
}

func startHTTPSProvider(label string, serverName string) (*upstreamServer, func()) {
	dir, err := os.MkdirTemp("", "cluster-router-provider-tls-*")
	if err != nil {
		panic("startHTTPSProvider: " + err.Error())
	}
	cert, caPEM := generateProviderCert(serverName)
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		panic("startHTTPSProvider: " + err.Error())
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = os.RemoveAll(dir)
		panic("startHTTPSProvider: " + err.Error())
	}
	s := &upstreamServer{
		label:  label,
		port:   l.Addr().(*net.TCPAddr).Port,
		caPath: caPath,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		seen := observedRequest{
			Auth:     r.Header.Get("authorization"),
			Provider: r.Header.Get("x-llm-provider"),
			Version:  r.Header.Get("x-cluster-router-version"),
		}
		if r.TLS != nil {
			seen.SNI = r.TLS.ServerName
		}
		s.mu.Lock()
		s.requests = append(s.requests, seen)
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(label))
	})
	server := &http.Server{
		Handler: mux,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		},
	}
	go server.Serve(tls.NewListener(l, server.TLSConfig)) //nolint:errcheck
	return s, func() {
		_ = server.Close()
		_ = os.RemoveAll(dir)
	}
}

func generateProviderCert(serverName string) (tls.Certificate, []byte) {
	now := time.Now()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic("generateProviderCert ca key: " + err.Error())
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cluster-router e2e provider ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		panic("generateProviderCert ca cert: " + err.Error())
	}
	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic("generateProviderCert server key: " + err.Error())
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
		panic("generateProviderCert server cert: " + err.Error())
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(serverKey)})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		panic("generateProviderCert server key pair: " + err.Error())
	}
	return cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}

func (s *upstreamServer) target() string {
	return net.JoinHostPort("localhost", fmt.Sprint(s.port))
}

func (s *upstreamServer) last() (observedRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		return observedRequest{}, false
	}
	return s.requests[len(s.requests)-1], true
}

type envoyConfigData struct {
	ProxyPort                      int
	AdminPort                      int
	ClusterConfigJSON              string
	HTTPSProviderClusterConfigJSON string
	HTTPSProviderCAPath            string
}

func marshalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
