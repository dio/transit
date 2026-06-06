// Package orange – egress proxy-facing CLI commands.
//
// This file implements "orange egress emulate", a debug tool that impersonates
// a running egress instance without deploying one. It exercises the same
// handshake, heartbeat, and config-fetch paths a real egress uses, making it
// possible to inspect snapshots and test secret resolution against a live CP.
package orange

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	configv1 "github.com/dio/transit/examples/orange/api/orange/config/v1"
	configv1connect "github.com/dio/transit/examples/orange/api/orange/config/v1/configv1connect"
	egressv1 "github.com/dio/transit/examples/orange/api/orange/egress/v1"
	egressv1connect "github.com/dio/transit/examples/orange/api/orange/egress/v1/egressv1connect"
	"github.com/dio/transit/examples/orange/internal/config"
	"github.com/dio/transit/examples/orange/internal/vtprotocodec"
)

// newEgressProxyCmd is the root for proxy-facing egress subcommands. It lives
// at "orange egress" (top-level), separate from "orange admin egress", because
// these operations authenticate with bundle credentials (egress.key) rather
// than an admin API key.
func newEgressProxyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "egress",
		Short: "Egress proxy operations (uses bundle credentials, not admin API key)",
	}
	cmd.AddCommand(newEgressEmulateCmd())
	return cmd
}

// egressBundleData holds the parsed contents of an egress bootstrap bundle.
//
// The bundle is produced by "orange admin egress bundle" and contains
// everything an egress needs to identify itself and consume config:
//
//   - identityCert: X.509 cert whose CN is "egress.workspace.<workspace_id>".
//     Currently used only for display (confirming the bundle belongs to the right
//     workspace). NOT used for mTLS — we use assertion signing instead (see
//     egressAssertionTransport). Planned: include the cert serial number in the
//     signed assertion message so the CP can bind the signature to a specific
//     issued certificate and reject assertions from revoked identities even if
//     the signing key has not been rotated yet.
//   - egressKey: PKCS#8 Ed25519 private key used to sign every request to the CP.
//   - paseto{1,2}Pub: public keys for offline PASETO token validation from clients.
//     The emulator loads these to confirm the bundle is complete; it does not
//     validate tokens (that is the real proxy's job).
//   - serverURL / egressID / workspaceID: parsed from config.yaml.
type egressBundleData struct {
	identityCert string
	egressKey    string
	paseto1Pub   string
	paseto2Pub   string
	serverURL    string
	egressID     string
	workspaceID  string
}

type bundleConfigYAML struct {
	ServerURL   string `yaml:"server_url"`
	EgressID    string `yaml:"egress_id"`
	WorkspaceID string `yaml:"workspace_id"`
}

func newEgressEmulateCmd() *cobra.Command {
	var (
		bundlePath string
		interval   time.Duration
		once       bool
	)
	cmd := &cobra.Command{
		Use:   "emulate",
		Short: "Emulate a running egress: send heartbeats and poll config",
		Long: `Emulate acts as a real egress instance for debugging.

It loads an egress bundle (directory or .tar.gz produced by 'orange admin egress bundle'),
authenticates using the signing key, then runs a loop that:

  1. Sends a Heartbeat to the management plane (marks egress online).
  2. Polls SnapshotService.Fetch for the current workspace config snapshot.
  3. Decodes the snapshot and attempts to resolve embedded secret refs.

Authentication — why assertion signing, not mTLS:
  mTLS requires the egress to hold its identity private key in a TLS context,
  which ties auth to the transport layer and complicates key rotation. Instead
  the egress signs a short-lived assertion with egress.key and sends it in the
  X-Egress-Assertion header. The CP verifies the Ed25519 signature against the
  paired public key in egress_keypairs. This is transport-independent and lets
  the signing key rotate without touching TLS certificate infrastructure.

  Assertion format:
    X-Egress-Assertion: <egress_id>.<base64url(sig)>.<unix_ts>
  where sig = Ed25519Sign(egress.key, "egress:<egress_id>:<workspace_id>:<unix_ts>").

  The "egress:" prefix prevents cross-protocol reuse of the same key for other
  signed messages. The workspace_id binding prevents a key from one workspace
  being used to fetch config for another. The unix_ts enables replay prevention
  (CP can reject assertions older than an acceptance window).

Config polling — why Fetch (poll) instead of Watch (stream):
  Watch is a long-lived server-stream optimised for production latency. For
  debugging, Fetch is preferable: each call is atomic, independently logged,
  and restartable. The emulator sends last_version + last_checksum on every
  call so the server skips re-sending an unchanged snapshot — the same
  incremental contract a real egress uses after a reconnect.

Use --once to do a single pass and exit. Interrupt with CTRL-C.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if bundlePath == "" {
				bundlePath = os.Getenv("ORANGE_EGRESS_BUNDLE")
			}
			if bundlePath == "" {
				bundlePath = "."
			}
			bundle, err := loadEgressBundle(bundlePath)
			if err != nil {
				return fmt.Errorf("load bundle: %w", err)
			}
			return runEgressEmulate(cmd.Context(), bundle, interval, once)
		},
	}
	cmd.Flags().StringVar(&bundlePath, "bundle", "", "bundle dir or .tar.gz path (env: ORANGE_EGRESS_BUNDLE)")
	cmd.Flags().DurationVar(&interval, "interval", 30*time.Second, "poll interval")
	cmd.Flags().BoolVar(&once, "once", false, "run a single pass and exit")
	return cmd
}

// loadEgressBundle reads bundle files from a directory or a .tar.gz archive.
// Both formats are produced by "orange admin egress bundle": use --out dir/ for
// loose files or the default <egress-id>.tar.gz for a portable archive.
// Missing optional files (paseto-1.pub, paseto-2.pub, identity.crt) are
// silently skipped; only config.yaml and egress.key are required.
func loadEgressBundle(path string) (*egressBundleData, error) {
	files := map[string]string{}
	isTar := strings.HasSuffix(path, ".tar.gz") || strings.HasSuffix(path, ".tgz")
	if isTar {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open archive: %w", err)
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("gzip reader: %w", err)
		}
		defer gz.Close()
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

	return &egressBundleData{
		identityCert: files["identity.crt"],
		egressKey:    files["egress.key"],
		paseto1Pub:   files["paseto-1.pub"],
		paseto2Pub:   files["paseto-2.pub"],
		serverURL:    cfg.ServerURL,
		egressID:     cfg.EgressID,
		workspaceID:  cfg.WorkspaceID,
	}, nil
}

// runEgressEmulate drives the emulation loop.
//
// On each tick it does three things in order:
//  1. Heartbeat  — keeps the egress marked online in the CP's heartbeat registry.
//  2. Fetch      — polls the current config snapshot for the workspace.
//  3. Resolve    — attempts to resolve every secret_ref found in the snapshot
//     using the built-in env://, file://, and literal:// resolvers. Resolved
//     values are masked before printing so the terminal does not leak secrets.
//
// lastVersion + lastChecksum are carried across ticks so that Fetch can return
// Unchanged when nothing has changed — the same SoTW incremental contract a
// production egress uses. This lets the emulator run cheaply at short intervals
// without re-decoding an identical snapshot on every poll.
func runEgressEmulate(parent context.Context, bundle *egressBundleData, interval time.Duration, once bool) error {
	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	privKey, err := parseEd25519PrivateKey(bundle.egressKey)
	if err != nil {
		return fmt.Errorf("parse egress.key: %w", err)
	}

	// Print a startup summary so the operator can confirm the bundle was loaded
	// correctly before any network calls are made.
	fmt.Printf("egress_id:    %s\n", bundle.egressID)
	fmt.Printf("workspace_id: %s\n", bundle.workspaceID)
	fmt.Printf("server_url:   %s\n", bundle.serverURL)
	// identity.crt is displayed but not used for auth (see egressAssertionTransport).
	fmt.Printf("identity:     %s\n", parseCertSubject(bundle.identityCert))
	// Print only the first 8 bytes of the public key — enough to identify the
	// key without leaking the full value to the terminal.
	fmt.Printf("signing key:  Ed25519 pub=%x…\n", privKey.Public().(ed25519.PublicKey)[:8])
	if bundle.paseto1Pub != "" {
		fmt.Printf("paseto slot1: present\n")
	}
	if bundle.paseto2Pub != "" {
		fmt.Printf("paseto slot2: present\n")
	}
	fmt.Println()

	transport := &egressAssertionTransport{
		base:        http.DefaultTransport,
		privKey:     privKey,
		egressID:    bundle.egressID,
		workspaceID: bundle.workspaceID,
	}
	httpClient := &http.Client{Timeout: 15 * time.Second, Transport: transport}
	opts := []connect.ClientOption{connect.WithCodec(vtprotocodec.Codec{})}

	heartbeatClient := egressv1connect.NewEgressServiceClient(httpClient, bundle.serverURL, opts...)
	snapshotClient := configv1connect.NewSnapshotServiceClient(httpClient, bundle.serverURL, opts...)

	// Use the default resolver (env://, file://, literal://) with a 5-minute
	// TTL. This matches what a production egress uses for non-service secrets.
	// A service-backed resolver (orange:// scheme) is out of scope for the
	// emulator — the goal here is to verify secret refs resolve, not to
	// exercise the full secret-service path.
	resolver := config.NewDefaultResolver(5 * time.Minute)

	// lastVersion and lastChecksum implement the SoTW incremental fetch
	// contract: after the first successful fetch, subsequent calls pass both
	// values so the server can return Unchanged instead of re-sending the full
	// payload. Reset to zero only if the emulator is restarted.
	var lastVersion uint64
	var lastChecksum []byte

	tick := func() {
		ts := time.Now().Format(time.RFC3339)

		hbResp, err := heartbeatClient.Heartbeat(ctx, connect.NewRequest(&egressv1.HeartbeatRequest{EgressId: bundle.egressID}))
		if err != nil {
			fmt.Printf("[%s] heartbeat  ERROR %v\n", ts, err)
		} else {
			fmt.Printf("[%s] heartbeat  OK server_time=%s\n", ts, hbResp.Msg.GetServerTime().AsTime().Format(time.RFC3339))
		}

		fetchResp, err := snapshotClient.Fetch(ctx, connect.NewRequest(&configv1.FetchRequest{
			WorkspaceId:  bundle.workspaceID,
			LastVersion:  lastVersion,
			LastChecksum: lastChecksum,
		}))
		if err != nil {
			fmt.Printf("[%s] config/fetch ERROR %v\n", ts, err)
			return
		}

		if fetchResp.Msg.GetUnchanged() != nil {
			fmt.Printf("[%s] config/fetch unchanged (version=%d)\n", ts, lastVersion)
			return
		}

		snap := fetchResp.Msg.GetSnapshot()
		if snap == nil {
			fmt.Printf("[%s] config/fetch empty response\n", ts)
			return
		}

		fmt.Printf("[%s] config/fetch snapshot version=%d format=%s compression=%s payload=%d bytes checksum=%x\n",
			ts, snap.GetVersion(), snap.GetFormat(), snap.GetCompression(), len(snap.GetPayload()), snap.GetChecksum())

		// Advance the cursor so the next tick sends the new version/checksum
		// and gets Unchanged if nothing has changed since.
		lastVersion = snap.GetVersion()
		lastChecksum = snap.GetChecksum()

		raw, err := config.DecodeRawFromProtoEnvelope(snap)
		if err != nil {
			fmt.Printf("[%s] config/decode ERROR %v\n", ts, err)
			return
		}

		refs := collectSecretRefs(raw)
		if len(refs) == 0 {
			fmt.Printf("[%s] config/secrets no secret refs in snapshot\n", ts)
		} else {
			fmt.Printf("[%s] config/secrets resolving %d ref(s):\n", ts, len(refs))
			for _, ref := range refs {
				val, err := resolver.Resolve(ctx, ref.secretRef)
				if err != nil {
					fmt.Printf("                 [%s] %s => ERROR: %v\n", ref.location, ref.secretRef, err)
				} else {
					// Mask to avoid leaking secrets to terminal/CI logs.
					fmt.Printf("                 [%s] %s => %s\n", ref.location, ref.secretRef, maskSecret(val))
				}
			}
		}
	}

	if once {
		tick()
		return ctx.Err()
	}

	fmt.Printf("polling every %s — press CTRL-C to stop\n\n", interval)
	tick()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
			tick()
		}
	}
}

// secretRefEntry is one secret_ref extracted from a decoded snapshot, tagged
// with its location in the config tree for diagnostic output.
type secretRefEntry struct {
	location  string // e.g. "provider/anthropic", "server/github", "profile/ws/srv"
	secretRef string // e.g. "env://ANTHROPIC_API_KEY", "file:///run/secrets/token"
}

// collectSecretRefs walks every auth block in the snapshot and returns all
// non-empty secret_ref strings. Providers, MCP servers, and per-profile auth
// overrides are all included because any of them can carry a secret ref that
// must resolve at request time.
func collectSecretRefs(raw *config.RawConfig) []secretRefEntry {
	var out []secretRefEntry
	for name, prov := range raw.LLM.Providers {
		if prov.Auth.SecretRef != "" {
			out = append(out, secretRefEntry{"provider/" + name, prov.Auth.SecretRef})
		}
	}
	for name, srv := range raw.MCP.Servers {
		if srv.Auth != nil && srv.Auth.SecretRef != "" {
			out = append(out, secretRefEntry{"server/" + name, srv.Auth.SecretRef})
		}
	}
	for profID, prof := range raw.Profiles {
		for srvID, auth := range prof.Auth {
			if auth.SecretRef != "" {
				out = append(out, secretRefEntry{"profile/" + profID + "/" + srvID, auth.SecretRef})
			}
		}
	}
	return out
}

// maskSecret replaces all but the first and last two characters with asterisks.
// Short values (≤ 6 chars) are fully masked. This keeps enough context to
// confirm the right secret was resolved without exposing its value in output.
func maskSecret(s string) string {
	if len(s) <= 6 {
		return strings.Repeat("*", len(s))
	}
	return s[:2] + strings.Repeat("*", len(s)-4) + s[len(s)-2:]
}

// parseEd25519PrivateKey decodes a PEM-encoded PKCS#8 Ed25519 private key.
// The key is stored in the bundle as egress.key; it was generated server-side
// by RotateKeyPair and the matching public key is registered in egress_keypairs.
func parseEd25519PrivateKey(pemStr string) (ed25519.PrivateKey, error) {
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

// parseCertSubject returns the Subject.CommonName of a PEM X.509 certificate.
// The identity cert is issued with CN="egress.workspace.<workspace_id>" so
// parsing the CN is sufficient to confirm the right bundle was loaded.
// Returns a descriptive string on any error rather than failing — the cert
// display is informational only.
func parseCertSubject(pemStr string) string {
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

// egressAssertionTransport injects X-Egress-Assertion into every outbound
// request. It implements the egress-to-CP handshake without mTLS.
//
// Why not mTLS:
//   mTLS would bind authentication to the TLS layer, requiring the egress to
//   hold its identity private key in a TLS context. That makes key rotation
//   expensive (new TLS handshake on every rotation) and couples auth to the
//   transport. Assertion signing is transport-independent and lets the signing
//   key rotate without rebuilding TLS sessions.
//
// Assertion format:
//
//	X-Egress-Assertion: <egress_id>.<base64url(sig)>.<unix_ts>
//
// The signed message is:
//
//	"egress:<egress_id>:<workspace_id>:<unix_ts>"
//
// Design choices:
//   - "egress:" prefix: prevents this key from being repurposed to sign
//     messages in other protocols that share the same key material.
//   - workspace_id in the message: binds the assertion to one workspace so a
//     compromised key cannot be used to fetch another workspace's config.
//   - unix_ts in both the message and the header: lets the CP enforce a short
//     acceptance window (e.g. ±30 s) to prevent replay attacks. The ts in the
//     header avoids the CP having to re-parse the base64 signature just to
//     extract the timestamp for window checking.
//   - Fresh timestamp per request: the assertion is not reusable. Signing once
//     at startup and caching the assertion would allow indefinite replay if
//     the header were captured in transit.
//
// Planned: add the identity cert's serial number to the signed message:
//   "egress:<egress_id>:<workspace_id>:<cert_serial>:<unix_ts>"
// This binds the assertion to a specific issued certificate. The CP can then
// reject assertions whose cert serial has been revoked in egress_identities —
// even if the signing key itself has not been rotated yet. This decouples
// identity revocation from key rotation, which matters when a cert expires or
// is compromised independently of the key.
type egressAssertionTransport struct {
	base        http.RoundTripper
	privKey     ed25519.PrivateKey
	egressID    string
	workspaceID string
}

func (t *egressAssertionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	msg := "egress:" + t.egressID + ":" + t.workspaceID + ":" + ts
	sig := ed25519.Sign(t.privKey, []byte(msg))
	assertion := t.egressID + "." + base64.RawURLEncoding.EncodeToString(sig) + "." + ts

	clone := req.Clone(req.Context())
	clone.Header.Set("X-Egress-Assertion", assertion)
	return t.base.RoundTrip(clone)
}
