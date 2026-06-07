package server

// egress_emulate_repl.go — interactive REPL mode for the egress emulator.
//
// Loaded via "orange egress emulate --repl". Drives the same Fetch/Heartbeat
// loop a real egress uses, with human-readable output and on-demand commands.
//
// See docs/orange-egress-emulate-repl.md for design rationale.

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/chzyer/readline"

	configv1connect "github.com/dio/transit/examples/orange/api/orange/config/v1/configv1connect"
	egressv1connect "github.com/dio/transit/examples/orange/api/orange/egress/v1/egressv1connect"
	"github.com/dio/transit/examples/orange/internal/config"
	"github.com/dio/transit/examples/orange/internal/egress"
	"github.com/dio/transit/examples/orange/internal/server/vtprotocodec"
)

type egressReplState struct {
	bundle  *egress.BundleData
	watcher *egressWatcher
	rl      *readline.Instance
}

// runEgressEmulateREPL is the entry point for --repl mode. It mirrors the
// setup in runEgressEmulate (bundle load, assertion transport, clients,
// resolver) and then starts the poll goroutine before entering the readline
// loop.
func runEgressEmulateREPL(parent context.Context, bundle *egress.BundleData, interval time.Duration) error {
	privKey, err := egress.ParseEd25519PrivateKey(bundle.EgressKey)
	if err != nil {
		return fmt.Errorf("parse egress.key: %w", err)
	}

	transport := &egress.AssertionTransport{
		Base:        http.DefaultTransport,
		PrivKey:     privKey,
		EgressID:    bundle.EgressID,
		WorkspaceID: bundle.WorkspaceID,
	}
	httpClient := &http.Client{Timeout: 15 * time.Second, Transport: transport}
	opts := []connect.ClientOption{connect.WithCodec(vtprotocodec.Codec{})}

	heartbeatClient := egressv1connect.NewEgressServiceClient(httpClient, bundle.ServerURL, opts...)
	snapshotClient := configv1connect.NewSnapshotServiceClient(httpClient, bundle.ServerURL, opts...)
	resolver := config.NewDefaultResolver(5 * time.Minute)
	watcher := newEgressWatcher(heartbeatClient, snapshotClient, resolver)

	// Startup summary identical to the non-REPL mode so the operator can
	// confirm the bundle was loaded correctly before any network calls.
	fmt.Printf("egress_id:     %s\n", bundle.EgressID)
	fmt.Printf("workspace_id:  %s\n", bundle.WorkspaceID)
	fmt.Printf("server_url:    %s\n", bundle.ServerURL)
	fmt.Printf("identity:      %s\n", egress.ParseCertSubject(bundle.IdentityCert))
	if pub, ok := privKey.Public().(ed25519.PublicKey); ok {
		fmt.Printf("signing key:   Ed25519 pub=%x…\n", pub[:8])
	}
	fmt.Printf("poll interval: %s\n\n", interval)

	histFile := os.ExpandEnv("$HOME/.orange/egress_repl_history")
	_ = os.MkdirAll(os.ExpandEnv("$HOME/.orange"), 0o700)

	rl, err := readline.NewEx(&readline.Config{
		Prompt:          "egress [no snapshot poll=idle]> ",
		HistoryFile:     histFile,
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
	})
	if err != nil {
		return err
	}
	defer rl.Close()

	state := &egressReplState{bundle: bundle, watcher: watcher, rl: rl}

	// notifyFn is called by the poll goroutine after each state swap.
	// Write to stderr so readline's stdout-owned prompt is not corrupted.
	notifyFn := func(version uint64) {
		st := watcher.readSnapshot()
		providers, servers, profiles := 0, 0, 0
		if st.raw != nil {
			providers = len(st.raw.LLM.Providers)
			servers = len(st.raw.MCP.Servers)
			profiles = len(st.raw.Profiles)
		}
		fmt.Fprintf(os.Stderr,
			"\n[poll] config updated: version=%d providers=%d servers=%d profiles=%d\n",
			version, providers, servers, profiles)
		rl.SetPrompt(state.prompt())
	}

	// Start the poll goroutine before entering the readline loop so the first
	// fetch fires immediately and the prompt reflects live state.
	watcher.startPoll(parent, interval, notifyFn)

	fmt.Println("egress emulator REPL  •  type 'help' for commands, 'exit' or Ctrl+D to quit")

	for {
		rl.SetPrompt(state.prompt())
		line, err := rl.Readline()
		if errors.Is(err, readline.ErrInterrupt) {
			if line == "" {
				break
			}
			continue
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if dispErr := state.dispatch(parent, line); dispErr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", dispErr)
		}
	}
	return nil
}

func (s *egressReplState) prompt() string {
	version, status, hasSnap := s.watcher.promptFields()
	if !hasSnap {
		return fmt.Sprintf("egress [no snapshot poll=%s]> ", status)
	}
	return fmt.Sprintf("egress [v=%d poll=%s]> ", version, status)
}

// dispatch splits the input line and routes to the appropriate handler.
func (s *egressReplState) dispatch(ctx context.Context, line string) error {
	toks := strings.Fields(line)
	if len(toks) == 0 {
		return nil
	}
	switch toks[0] {
	case "exit", "quit":
		os.Exit(0)

	case "help", "?":
		printEgressReplHelp()

	case "snapshot", "snap":
		full := len(toks) > 1 && toks[1] == "full"
		return s.cmdSnapshot(full)

	case "secrets", "sec":
		return s.cmdSecrets(ctx)

	case "resolve":
		if len(toks) < 2 {
			return fmt.Errorf("usage: resolve <ref>")
		}
		return s.cmdResolve(ctx, toks[1])

	case "fetch":
		return s.cmdFetch(ctx)

	case "heartbeat", "hb":
		return s.cmdHeartbeat(ctx)

	case "poll":
		sub := ""
		if len(toks) > 1 {
			sub = toks[1]
		}
		return s.cmdPoll(sub)

	case "status":
		return s.cmdStatus(ctx)

	default:
		return fmt.Errorf("unknown command %q — type 'help' for a list", toks[0])
	}
	return nil
}

// ── snapshot ──────────────────────────────────────────────────────────────────

func (s *egressReplState) cmdSnapshot(full bool) error {
	st := s.watcher.readSnapshot()
	if st.snap == nil {
		fmt.Println("no snapshot received yet — poll is running")
		return nil
	}
	fmt.Printf("version:      %d\n", st.lastVersion)
	fmt.Printf("checksum:     %x\n", st.lastChecksum)
	fmt.Printf("format:       %s\n", st.snap.GetFormat())
	fmt.Printf("compression:  %s\n", st.snap.GetCompression())
	fmt.Printf("payload:      %d bytes\n", len(st.snap.GetPayload()))
	if st.raw != nil {
		fmt.Printf("providers:    %d\n", len(st.raw.LLM.Providers))
		fmt.Printf("models:       %d\n", len(st.raw.LLM.Models))
		fmt.Printf("servers:      %d\n", len(st.raw.MCP.Servers))
		fmt.Printf("profiles:     %d\n", len(st.raw.Profiles))
		fmt.Printf("keys:         %d\n", len(st.raw.Keys))
	}
	if full && st.raw != nil {
		fmt.Println()
		for name := range st.raw.LLM.Providers {
			fmt.Printf("  provider  %s\n", name)
		}
		for name := range st.raw.LLM.Models {
			fmt.Printf("  model     %s\n", name)
		}
		for name := range st.raw.MCP.Servers {
			fmt.Printf("  server    %s\n", name)
		}
		for id := range st.raw.Profiles {
			fmt.Printf("  profile   %s\n", id)
		}
	}
	return nil
}

// ── secrets ───────────────────────────────────────────────────────────────────

func (s *egressReplState) cmdSecrets(ctx context.Context) error {
	st := s.watcher.readSnapshot()
	if st.raw == nil {
		fmt.Println("no snapshot yet")
		return nil
	}
	refs := collectSecretRefs(st.raw)
	if len(refs) == 0 {
		fmt.Println("no secret refs in snapshot")
		return nil
	}
	fmt.Printf("%d secret ref(s):\n", len(refs))
	for _, ref := range refs {
		val, err := s.watcher.resolver.Resolve(ctx, ref.secretRef)
		if err != nil {
			fmt.Printf("  [%s] %s => ERROR: %v\n", ref.location, ref.secretRef, err)
		} else {
			fmt.Printf("  [%s] %s => %s\n", ref.location, ref.secretRef, maskSecret(val))
		}
	}
	return nil
}

// ── resolve ───────────────────────────────────────────────────────────────────

func (s *egressReplState) cmdResolve(ctx context.Context, ref string) error {
	val, err := s.watcher.resolver.Resolve(ctx, ref)
	if err != nil {
		return err
	}
	fmt.Printf("%s => %s\n", ref, maskSecret(val))
	return nil
}

// ── fetch ─────────────────────────────────────────────────────────────────────

func (s *egressReplState) cmdFetch(ctx context.Context) error {
	changed, err := s.watcher.Fetch(ctx)
	if err != nil {
		return err
	}
	st := s.watcher.readSnapshot()
	if changed {
		fmt.Printf("fetch: new snapshot version=%d\n", st.lastVersion)
	} else {
		fmt.Printf("fetch: unchanged (version=%d)\n", st.lastVersion)
	}
	return nil
}

// ── heartbeat ─────────────────────────────────────────────────────────────────

func (s *egressReplState) cmdHeartbeat(ctx context.Context) error {
	if err := s.watcher.Heartbeat(ctx); err != nil {
		return err
	}
	fmt.Printf("heartbeat: ok  (%s)\n", time.Now().Format(time.RFC3339))
	return nil
}

// ── poll ──────────────────────────────────────────────────────────────────────

func (s *egressReplState) cmdPoll(sub string) error {
	switch sub {
	case "status", "":
		st := s.watcher.readSnapshot()
		fmt.Printf("status:    %s\n", st.pollStatus)
		if st.pollErr != nil {
			fmt.Printf("last err:  %v\n", st.pollErr)
		}
		if !st.lastHeartbeat.IsZero() {
			fmt.Printf("heartbeat: %s ago\n", time.Since(st.lastHeartbeat).Round(time.Second))
		} else {
			fmt.Println("heartbeat: never")
		}
		if !st.lastFetch.IsZero() {
			fmt.Printf("fetch:     %s ago (version=%d)\n", time.Since(st.lastFetch).Round(time.Second), st.lastVersion)
		} else {
			fmt.Println("fetch:     never")
		}

	case "history":
		entries := s.watcher.historyEntries()
		if len(entries) == 0 {
			fmt.Println("no snapshots received yet")
			return nil
		}
		fmt.Printf("%-32s  %-8s  %-18s  %s\n", "RECEIVED", "VERSION", "CHECKSUM", "PROV/SRV/PROF")
		for _, e := range entries {
			chk := fmt.Sprintf("%x", e.checksum)
			if len(chk) > 16 {
				chk = chk[:16] + "…"
			}
			fmt.Printf("%-32s  %-8d  %-18s  %d/%d/%d\n",
				e.receivedAt.Format(time.RFC3339),
				e.version, chk,
				e.providers, e.servers, e.profiles,
			)
		}

	default:
		return fmt.Errorf("unknown poll subcommand %q — try: status, history", sub)
	}
	return nil
}

// ── status ────────────────────────────────────────────────────────────────────

func (s *egressReplState) cmdStatus(ctx context.Context) error {
	// One-shot heartbeat for liveness, then poll status + snapshot summary.
	if err := s.watcher.Heartbeat(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "heartbeat: ERROR %v\n", err)
	} else {
		fmt.Printf("heartbeat: ok  (%s)\n", time.Now().Format(time.RFC3339))
	}
	fmt.Println()
	return s.cmdPoll("status")
}

// ── help ──────────────────────────────────────────────────────────────────────

func printEgressReplHelp() {
	fmt.Print(`
Egress emulator REPL — poll mode (Fetch + Heartbeat loop)

Snapshot:
  snapshot [full]        metadata of current snapshot; 'full' lists all entries
  secrets                list secret refs in snapshot (values masked)
  resolve <ref>          resolve one ref, e.g. env://ANTHROPIC_API_KEY

Manual triggers (independent of poll timer):
  fetch                  one-shot Fetch; prints changed/unchanged
  heartbeat / hb         one-shot Heartbeat

Poll loop:
  poll status            goroutine status, last heartbeat/fetch times
  poll history           last 20 snapshots received

Combined:
  status                 heartbeat + poll status

Other:
  help / ?               show this help
  exit / quit / Ctrl+D   exit

Config mutations (not via the egress path):
  orange admin --repl                         admin-scoped tasks/records
  ORANGE_API_KEY=<key> orange --repl          user-scoped records (planned)

`)
}
