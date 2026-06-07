// Package egress provides the runtime primitives used by an egress proxy to
// authenticate itself to the management plane and load its bootstrap bundle.
//
// These types are shared between the debug emulator (orange egress emulate) and
// any real egress implementation so that both exercise exactly the same bundle
// loading and assertion-signing paths.
package egress

import (
	"archive/tar"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// BundleData holds the parsed contents of an egress bootstrap bundle.
//
// The bundle is produced by "orange admin egress bundle" and contains
// everything an egress needs to identify itself and consume config:
//
//   - IdentityCert: X.509 cert whose CN is "egress.workspace.<workspace_id>".
//     Used only for display / future serial-binding; NOT used for mTLS.
//   - EgressKey: PKCS#8 Ed25519 private key used to sign every request to the CP.
//   - Paseto{1,2}Pub: public keys for offline PASETO token validation.
//   - ServerURL / EgressID / WorkspaceID: parsed from config.yaml.
type BundleData struct {
	IdentityCert string
	EgressKey    string
	Paseto1Pub   string
	Paseto2Pub   string
	ServerURL    string
	EgressID     string
	WorkspaceID  string
}

type bundleConfigYAML struct {
	ServerURL   string `yaml:"server_url"`
	EgressID    string `yaml:"egress_id"`
	WorkspaceID string `yaml:"workspace_id"`
}

// LoadBundle reads bundle files from a directory or a .tar.gz archive.
// Both formats are produced by "orange admin egress bundle": use --out dir/ for
// loose files or the default <egress-id>.tar.gz for a portable archive.
// Missing optional files (paseto-1.pub, paseto-2.pub, identity.crt) are
// silently skipped; only config.yaml and egress.key are required.
func LoadBundle(path string) (*BundleData, error) {
	files := map[string]string{}
	isTar := strings.HasSuffix(path, ".tar.gz") || strings.HasSuffix(path, ".tgz")
	if isTar {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open archive: %w", err)
		}
		defer func() {
			_ = f.Close()
		}()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("gzip reader: %w", err)
		}
		defer func() {
			_ = gz.Close()
		}()
		tr := tar.NewReader(gz)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("tar read: %w", err)
			}
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", hdr.Name, err)
			}
			// Use the base name so paths like "bundle/egress.key" and "egress.key"
			// both land under the same key.
			files[filepath.Base(hdr.Name)] = string(data)
		}
	} else {
		for _, name := range []string{"identity.crt", "egress.key", "paseto-1.pub", "paseto-2.pub", "config.yaml"} {
			data, err := os.ReadFile(filepath.Join(path, name))
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, fmt.Errorf("read %s: %w", name, err)
			}
			files[name] = string(data)
		}
	}

	cfgYAML, ok := files["config.yaml"]
	if !ok {
		return nil, fmt.Errorf("bundle missing config.yaml")
	}
	var cfg bundleConfigYAML
	if err := yaml.Unmarshal([]byte(cfgYAML), &cfg); err != nil {
		return nil, fmt.Errorf("parse config.yaml: %w", err)
	}
	if cfg.EgressID == "" {
		return nil, fmt.Errorf("config.yaml: egress_id is empty")
	}
	if cfg.WorkspaceID == "" {
		return nil, fmt.Errorf("config.yaml: workspace_id is empty")
	}
	if cfg.ServerURL == "" {
		return nil, fmt.Errorf("config.yaml: server_url is empty")
	}

	return &BundleData{
		IdentityCert: files["identity.crt"],
		EgressKey:    files["egress.key"],
		Paseto1Pub:   files["paseto-1.pub"],
		Paseto2Pub:   files["paseto-2.pub"],
		ServerURL:    cfg.ServerURL,
		EgressID:     cfg.EgressID,
		WorkspaceID:  cfg.WorkspaceID,
	}, nil
}

// AssertionTransport injects X-Egress-Assertion into every outbound request.
// It implements the egress-to-CP handshake without mTLS.
//
// Assertion format:
//
//	X-Egress-Assertion: <egress_id>.<workspace_id>.<base64url(sig)>.<unix_ts>
//
// The signed message is:
//
//	"egress:<egress_id>:<workspace_id>:<unix_ts>"
//
// The "egress:" prefix prevents cross-protocol reuse of the same key. The
// workspace_id binding prevents a key from one workspace fetching another's
// config. The unix_ts enables replay prevention (CP rejects stale assertions).
// A fresh timestamp is computed per request so assertions are not reusable.
type AssertionTransport struct {
	Base        http.RoundTripper
	PrivKey     ed25519.PrivateKey
	EgressID    string
	WorkspaceID string
}

func (t *AssertionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	msg := "egress:" + t.EgressID + ":" + t.WorkspaceID + ":" + ts
	sig := ed25519.Sign(t.PrivKey, []byte(msg))
	assertion := t.EgressID + "." + t.WorkspaceID + "." + base64.RawURLEncoding.EncodeToString(sig) + "." + ts

	clone := req.Clone(req.Context())
	clone.Header.Set("X-Egress-Assertion", assertion)
	return t.Base.RoundTrip(clone)
}

// ParseEd25519PrivateKey decodes a PEM-encoded PKCS#8 Ed25519 private key.
// The key is stored in the bundle as egress.key.
func ParseEd25519PrivateKey(pemStr string) (ed25519.PrivateKey, error) {
	if pemStr == "" {
		return nil, fmt.Errorf("egress.key is empty")
	}
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKCS8: %w", err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("expected Ed25519, got %T", key)
	}
	return priv, nil
}

// ParseCertSubject returns the Subject.CommonName of a PEM X.509 certificate.
// The identity cert is issued with CN="egress.workspace.<workspace_id>".
// Returns a descriptive string on any error — cert display is informational only.
func ParseCertSubject(pemStr string) string {
	if pemStr == "" {
		return "(none)"
	}
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return "(invalid PEM)"
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "(parse error: " + err.Error() + ")"
	}
	return cert.Subject.CommonName
}
