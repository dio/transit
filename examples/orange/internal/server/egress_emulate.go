// Package orange – egress proxy-facing CLI commands.
//
// This file implements "orange egress emulate", a debug tool that impersonates
// a running egress instance without deploying one. It exercises the same
// handshake, heartbeat, and config-fetch paths a real egress uses, making it
// possible to inspect snapshots and test secret resolution against a live CP.
package server

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/dio/transit/examples/orange/internal/config"
	"github.com/dio/transit/examples/orange/internal/client"
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
	cmd.AddCommand(newEgressVerifyCmd())
	return cmd
}

func newEgressEmulateCmd() *cobra.Command {
	var (
		bundlePath string
		interval   time.Duration
		once       bool
		replMode   bool
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
    X-Egress-Assertion: <egress_id>.<workspace_id>.<base64url(sig)>.<unix_ts>
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
			bundle, err := client.LoadBundle(bundlePath)
			if err != nil {
				return fmt.Errorf("load bundle: %w", err)
			}
			if replMode {
				return runEgressEmulateREPL(cmd.Context(), bundle, interval)
			}
			return runEgressEmulate(cmd.Context(), bundle, interval, once)
		},
	}
	cmd.Flags().StringVar(&bundlePath, "bundle", "", "bundle dir or .tar.gz path (env: ORANGE_EGRESS_BUNDLE)")
	cmd.Flags().DurationVar(&interval, "interval", 30*time.Second, "poll interval")
	cmd.Flags().BoolVar(&once, "once", false, "run a single pass and exit")
	cmd.Flags().BoolVar(&replMode, "repl", false, "run interactive REPL (poll mode); --once is ignored")
	return cmd
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
// The client.Client carries the SoTW cursor (lastVersion/lastChecksum) across
// ticks so that Fetch returns Unchanged when nothing has changed, letting the
// emulator run cheaply at short intervals.
func runEgressEmulate(parent context.Context, bundle *client.BundleData, interval time.Duration, once bool) error {
	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	privKey, err := client.ParseEd25519PrivateKey(bundle.EgressKey)
	if err != nil {
		return fmt.Errorf("parse egress.key: %w", err)
	}

	// Print a startup summary so the operator can confirm the bundle was loaded
	// correctly before any network calls are made.
	fmt.Printf("egress_id:    %s\n", bundle.EgressID)
	fmt.Printf("workspace_id: %s\n", bundle.WorkspaceID)
	fmt.Printf("server_url:   %s\n", bundle.ServerURL)
	// identity.crt is displayed but not used for auth (see client.AssertionTransport).
	fmt.Printf("identity:     %s\n", client.ParseCertSubject(bundle.IdentityCert))
	// Print only the first 8 bytes of the public key — enough to identify the
	// key without leaking the full value to the terminal.
	pub, ok := privKey.Public().(ed25519.PublicKey)
	if ok {
		fmt.Printf("signing key:  Ed25519 pub=%x…\n", pub[:8])
	}
	if bundle.Paseto1Pub != "" {
		fmt.Printf("token key slot1: present\n")
	}
	if bundle.Paseto2Pub != "" {
		fmt.Printf("token key slot2: present\n")
	}
	fmt.Println()

	c, err := client.NewClient(bundle)
	if err != nil {
		return err
	}

	tick := func() {
		ts := time.Now().Format(time.RFC3339)

		serverTime, err := c.Heartbeat(ctx)
		if err != nil {
			fmt.Printf("[%s] heartbeat  ERROR %v\n", ts, err)
		} else {
			fmt.Printf("[%s] heartbeat  OK server_time=%s\n", ts, serverTime.Format(time.RFC3339))
		}

		snap, raw, changed, err := c.Fetch(ctx)
		if err != nil {
			fmt.Printf("[%s] config/fetch ERROR %v\n", ts, err)
			return
		}
		if !changed {
			fmt.Printf("[%s] config/fetch unchanged\n", ts)
			return
		}

		fmt.Printf("[%s] config/fetch snapshot version=%d format=%s compression=%s payload=%d bytes checksum=%x\n",
			ts, snap.GetVersion(), snap.GetFormat(), snap.GetCompression(), len(snap.GetPayload()), snap.GetChecksum())

		refs := collectSecretRefs(raw)
		if len(refs) == 0 {
			fmt.Printf("[%s] config/secrets no secret refs in snapshot\n", ts)
		} else {
			fmt.Printf("[%s] config/secrets resolving %d ref(s):\n", ts, len(refs))
			for _, ref := range refs {
				val, err := c.Resolver.Resolve(ctx, ref.secretRef)
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

// newEgressVerifyCmd returns "orange egress verify <token>" — offline PASETO token
// verification using the PASETO public keys from an egress bundle. Accepts the
// raw Authorization header value ("Bearer sk-...") or a bare token ("sk-...").
func newEgressVerifyCmd() *cobra.Command {
	var bundlePath string
	cmd := &cobra.Command{
		Use:          "verify <token>",
		Short:        "Verify a PASETO Bearer token offline using bundle keys",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if bundlePath == "" {
				bundlePath = os.Getenv("ORANGE_EGRESS_BUNDLE")
			}
			if bundlePath == "" {
				bundlePath = "."
			}
			bundle, err := client.LoadBundle(bundlePath)
			if err != nil {
				return fmt.Errorf("load bundle: %w", err)
			}
			return runVerifyToken(cmd, bundle, args[0])
		},
	}
	cmd.Flags().StringVar(&bundlePath, "bundle", "", "bundle dir or .tar.gz (env: ORANGE_EGRESS_BUNDLE)")
	return cmd
}

// runVerifyToken verifies raw (an Authorization header value or bare token)
// against the PASETO public keys in bundle and prints decoded claims.
func runVerifyToken(cmd *cobra.Command, bundle *client.BundleData, raw string) error {
	pub1, err1 := loadOptionalPub(bundle.Paseto1Pub, 1)
	pub2, err2 := loadOptionalPub(bundle.Paseto2Pub, 2)
	if err1 != nil {
		return err1
	}
	if err2 != nil {
		return err2
	}

	claims, err := client.VerifyToken(raw, pub1, pub2)
	if err != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "INVALID  %v\n", err)
		return nil
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "VALID\n")
	fmt.Fprintf(out, "  workspace_slug  %s\n", claims.WorkspaceSlug)
	fmt.Fprintf(out, "  slot            %d\n", claims.Slot)
	fmt.Fprintf(out, "  key_entry_slug  %s\n", claims.KeyEntrySlug)
	fmt.Fprintf(out, "  tid             %x\n", claims.TID)
	if claims.Exp.IsZero() {
		fmt.Fprintf(out, "  expires         never\n")
	} else {
		mark := ""
		if claims.Expired() {
			mark = "  *** EXPIRED ***"
		}
		fmt.Fprintf(out, "  expires         %s%s\n", claims.Exp.Format(time.RFC3339), mark)
	}
	return nil
}

// loadOptionalPub parses a PEM public key; returns nil (no error) when pemStr is empty.
func loadOptionalPub(pemStr string, slot int) (ed25519.PublicKey, error) {
	if pemStr == "" {
		return nil, nil
	}
	pub, err := client.ParseEd25519PublicKey(pemStr)
	if err != nil {
		return nil, fmt.Errorf("parse slot %d public key: %w", slot, err)
	}
	return pub, nil
}
