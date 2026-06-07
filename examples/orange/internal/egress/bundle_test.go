package egress_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dio/transit/examples/orange/internal/egress"
)

// bundleTarGz is the path to the test bundle archive relative to this package.
const bundleTarGz = "testdata/019ea021-1f3f-7aba-ab6e-5c4ea24edea8.tar.gz"

const (
	wantEgressID    = "019ea021-1f3f-7aba-ab6e-5c4ea24edea8"
	wantWorkspaceID = "019ea021-1f3c-7e86-9f97-eeedb8ac7df8"
	wantServerURL   = "http://localhost:8080"
	wantCertSubject = "egress.workspace.019ea021-1f3c-7e86-9f97-eeedb8ac7df8"
)

// testEgressKeyPEM is the Ed25519 private key from the test bundle.
const testEgressKeyPEM = `-----BEGIN PRIVATE KEY-----
MC4CAQAwBQYDK2VwBCIEIGG7wTzSRjxz6CKgG73cXDE09rCCGnIMnb4yQQCrIDpI
-----END PRIVATE KEY-----`

// testIdentityCertPEM is the identity certificate from the test bundle.
const testIdentityCertPEM = `-----BEGIN CERTIFICATE-----
MIIBdDCCASagAwIBAgIRAKf6NpDFOPxewW2vYqyNHoEwBQYDK2VwMEAxPjA8BgNV
BAMTNWVncmVzcy53b3Jrc3BhY2UuMDE5ZWEwMjEtMWYzYy03ZTg2LTlmOTctZWVl
ZGI4YWM3ZGY4MB4XDTI2MDYwNzAzMjk0NVoXDTI2MDkwNTAzMjk0NVowQDE+MDwG
A1UEAxM1ZWdyZXNzLndvcmtzcGFjZS4wMTllYTAyMS0xZjNjLTdlODYtOWY5Ny1l
ZWVkYjhhYzdkZjgwKjAFBgMrZXADIQB/pNvP3Qn0hMJw9kgfKu5YcN27PWyl7Xpj
JA5hBXLeQ6M1MDMwDgYDVR0PAQH/BAQDAgeAMBMGA1UdJQQMMAoGCCsGAQUFBwMC
MAwGA1UdEwEB/wQCMAAwBQYDK2VwA0EA4k40+JdJSQPp3G2R9M0IB1o8YFrgw5Uc
yv+4xkzAyAsC5L5rO+ttIi8rs3Jm+JOir5ma/k+IBOL1rr+1ogJEAg==
-----END CERTIFICATE-----`

// TestLoadBundle_TarGz loads the real test archive and checks all fields.
func TestLoadBundle_TarGz(t *testing.T) {
	b, err := egress.LoadBundle(bundleTarGz)
	if err != nil {
		t.Fatalf("LoadBundle(%q): %v", bundleTarGz, err)
	}

	if b.EgressID != wantEgressID {
		t.Errorf("EgressID = %q, want %q", b.EgressID, wantEgressID)
	}
	if b.WorkspaceID != wantWorkspaceID {
		t.Errorf("WorkspaceID = %q, want %q", b.WorkspaceID, wantWorkspaceID)
	}
	if b.ServerURL != wantServerURL {
		t.Errorf("ServerURL = %q, want %q", b.ServerURL, wantServerURL)
	}
	if b.IdentityCert == "" {
		t.Error("IdentityCert is empty")
	}
	if b.EgressKey == "" {
		t.Error("EgressKey is empty")
	}
	if b.Paseto1Pub == "" {
		t.Error("Paseto1Pub is empty")
	}
	if b.Paseto2Pub == "" {
		t.Error("Paseto2Pub is empty")
	}
}

// TestLoadBundle_Dir extracts the archive to a temp directory and loads from there.
func TestLoadBundle_Dir(t *testing.T) {
	dir := extractBundleToDir(t, bundleTarGz)

	b, err := egress.LoadBundle(dir)
	if err != nil {
		t.Fatalf("LoadBundle(dir): %v", err)
	}
	if b.EgressID != wantEgressID {
		t.Errorf("EgressID = %q, want %q", b.EgressID, wantEgressID)
	}
	if b.WorkspaceID != wantWorkspaceID {
		t.Errorf("WorkspaceID = %q, want %q", b.WorkspaceID, wantWorkspaceID)
	}
}

func TestLoadBundle_MissingConfigYAML(t *testing.T) {
	dir := t.TempDir()
	// Write only egress.key, no config.yaml.
	if err := os.WriteFile(filepath.Join(dir, "egress.key"), []byte(testEgressKeyPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := egress.LoadBundle(dir)
	if err == nil {
		t.Fatal("expected error for missing config.yaml, got nil")
	}
}

func TestLoadBundle_EmptyFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		yaml   string
		errSub string
	}{
		{
			name:   "missing egress_id",
			yaml:   "server_url: \"http://x\"\nworkspace_id: \"w\"\n",
			errSub: "egress_id",
		},
		{
			name:   "missing workspace_id",
			yaml:   "server_url: \"http://x\"\negress_id: \"e\"\n",
			errSub: "workspace_id",
		},
		{
			name:   "missing server_url",
			yaml:   "egress_id: \"e\"\nworkspace_id: \"w\"\n",
			errSub: "server_url",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := egress.LoadBundle(dir)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.errSub)
			}
			if !strings.Contains(err.Error(), tc.errSub) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.errSub)
			}
		})
	}
}

func TestParseEd25519PrivateKey(t *testing.T) {
	key, err := egress.ParseEd25519PrivateKey(testEgressKeyPEM)
	if err != nil {
		t.Fatalf("ParseEd25519PrivateKey: %v", err)
	}
	if len(key) != ed25519.PrivateKeySize {
		t.Errorf("key size = %d, want %d", len(key), ed25519.PrivateKeySize)
	}
	pub, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatal("key.Public() is not ed25519.PublicKey")
	}
	if len(pub) != ed25519.PublicKeySize {
		t.Errorf("pub size = %d, want %d", len(pub), ed25519.PublicKeySize)
	}
}

func TestParseEd25519PrivateKey_Invalid(t *testing.T) {
	for _, tc := range []struct {
		name string
		pem  string
	}{
		{"empty", ""},
		{"garbage", "not-pem"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := egress.ParseEd25519PrivateKey(tc.pem)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestParseCertSubject(t *testing.T) {
	got := egress.ParseCertSubject(testIdentityCertPEM)
	if got != wantCertSubject {
		t.Errorf("ParseCertSubject = %q, want %q", got, wantCertSubject)
	}
}

func TestParseCertSubject_EmptyAndInvalid(t *testing.T) {
	if got := egress.ParseCertSubject(""); got != "(none)" {
		t.Errorf("empty PEM: got %q, want %q", got, "(none)")
	}
	if got := egress.ParseCertSubject("not-pem"); got != "(invalid PEM)" {
		t.Errorf("garbage PEM: got %q, want %q", got, "(invalid PEM)")
	}
}

// TestAssertionTransport verifies the transport injects a well-formed
// X-Egress-Assertion header whose Ed25519 signature is valid.
func TestAssertionTransport(t *testing.T) {
	privKey, err := egress.ParseEd25519PrivateKey(testEgressKeyPEM)
	if err != nil {
		t.Fatalf("parse private key: %v", err)
	}
	pub := privKey.Public().(ed25519.PublicKey)

	var capturedHeader string
	fake := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		capturedHeader = req.Header.Get("X-Egress-Assertion")
		return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
	})

	transport := &egress.AssertionTransport{
		Base:        fake,
		PrivKey:     privKey,
		EgressID:    wantEgressID,
		WorkspaceID: wantWorkspaceID,
	}

	before := time.Now().Unix()
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/", nil)
	if _, err := transport.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	after := time.Now().Unix()

	if capturedHeader == "" {
		t.Fatal("X-Egress-Assertion header not set")
	}

	// Format: <egress_id>.<workspace_id>.<base64url(sig)>.<unix_ts>
	parts := strings.SplitN(capturedHeader, ".", 4)
	if len(parts) != 4 {
		t.Fatalf("expected 4 parts, got %d: %q", len(parts), capturedHeader)
	}
	egressID, workspaceID, sigB64, tsStr := parts[0], parts[1], parts[2], parts[3]

	if egressID != wantEgressID {
		t.Errorf("egress_id = %q, want %q", egressID, wantEgressID)
	}
	if workspaceID != wantWorkspaceID {
		t.Errorf("workspace_id = %q, want %q", workspaceID, wantWorkspaceID)
	}

	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		t.Fatalf("parse timestamp %q: %v", tsStr, err)
	}
	if ts < before || ts > after+1 {
		t.Errorf("timestamp %d outside expected range [%d, %d]", ts, before, after+1)
	}

	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	msg := "egress:" + egressID + ":" + workspaceID + ":" + tsStr
	if !ed25519.Verify(pub, []byte(msg), sig) {
		t.Error("signature verification failed")
	}
}

// roundTripFunc adapts a function to the http.RoundTripper interface.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// extractBundleToDir extracts the named tar.gz to a temp directory and returns its path.
func extractBundleToDir(t *testing.T, archivePath string) string {
	t.Helper()
	b, err := egress.LoadBundle(archivePath)
	if err != nil {
		t.Fatalf("load bundle for extraction: %v", err)
	}
	dir := t.TempDir()
	files := map[string]string{
		"identity.crt": b.IdentityCert,
		"egress.key":   b.EgressKey,
		"paseto-1.pub": b.Paseto1Pub,
		"paseto-2.pub": b.Paseto2Pub,
		"config.yaml": "server_url: " + strconv.Quote(b.ServerURL) + "\n" +
			"egress_id: " + strconv.Quote(b.EgressID) + "\n" +
			"workspace_id: " + strconv.Quote(b.WorkspaceID) + "\n",
	}
	for name, content := range files {
		if content == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}
