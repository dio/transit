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
	"os"
	"strings"
	"time"

	"github.com/chzyer/readline"

	"github.com/dio/transit/examples/orange/internal/egress"
)

type egressReplState struct {
	bundle  *egress.BundleData
	watcher *egress.Watcher
	rl      *readline.Instance
}

// runEgressEmulateREPL is the entry point for --repl mode. It sets up a
// shared egress.Client (bundle credentials + connect transport), wraps it in
// a Watcher for poll-state tracking, and enters the readline loop.
func runEgressEmulateREPL(parent context.Context, bundle *egress.BundleData, interval time.Duration) error {
	// Parse the private key here only for the startup fingerprint display.
	// egress.NewClient parses it again internally.
	privKey, err := egress.ParseEd25519PrivateKey(bundle.EgressKey)
	if err != nil {
		return fmt.Errorf("parse egress.key: %w", err)
	}

	client, err := egress.NewClient(bundle)
	if err != nil {
		return err
	}
	watcher := egress.NewWatcher(client)

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
		st := watcher.ReadSnapshot()
		providers, servers, profiles := 0, 0, 0
		if st.Raw != nil {
			providers = len(st.Raw.LLM.Providers)
			servers = len(st.Raw.MCP.Servers)
			profiles = len(st.Raw.Profiles)
		}
		fmt.Fprintf(os.Stderr,
			"\n[poll] config updated: version=%d providers=%d servers=%d profiles=%d\n",
			version, providers, servers, profiles)
		rl.SetPrompt(state.prompt())
	}

	// Start the poll goroutine before entering the readline loop so the first
	// fetch fires immediately and the prompt reflects live state.
	watcher.StartPoll(parent, interval, notifyFn)

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
	version, status, hasSnap := s.watcher.PromptFields()
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
	st := s.watcher.ReadSnapshot()
	if st.Snap == nil {
		fmt.Println("no snapshot received yet — poll is running")
		return nil
	}
	fmt.Printf("version:      %d\n", st.LastVersion)
	fmt.Printf("checksum:     %x\n", st.LastChecksum)
	fmt.Printf("format:       %s\n", st.Snap.GetFormat())
	fmt.Printf("compression:  %s\n", st.Snap.GetCompression())
	fmt.Printf("payload:      %d bytes\n", len(st.Snap.GetPayload()))
	if st.Raw != nil {
		fmt.Printf("providers:    %d\n", len(st.Raw.LLM.Providers))
		fmt.Printf("models:       %d\n", len(st.Raw.LLM.Models))
		fmt.Printf("servers:      %d\n", len(st.Raw.MCP.Servers))
		fmt.Printf("profiles:     %d\n", len(st.Raw.Profiles))
		fmt.Printf("keys:         %d\n", len(st.Raw.Keys))
	}
	if full && st.Raw != nil {
		fmt.Println()
		for name := range st.Raw.LLM.Providers {
			fmt.Printf("  provider  %s\n", name)
		}
		for name := range st.Raw.LLM.Models {
			fmt.Printf("  model     %s\n", name)
		}
		for name := range st.Raw.MCP.Servers {
			fmt.Printf("  server    %s\n", name)
		}
		for id := range st.Raw.Profiles {
			fmt.Printf("  profile   %s\n", id)
		}
	}
	return nil
}

// ── secrets ───────────────────────────────────────────────────────────────────

func (s *egressReplState) cmdSecrets(ctx context.Context) error {
	st := s.watcher.ReadSnapshot()
	if st.Raw == nil {
		fmt.Println("no snapshot yet")
		return nil
	}
	refs := collectSecretRefs(st.Raw)
	if len(refs) == 0 {
		fmt.Println("no secret refs in snapshot")
		return nil
	}
	fmt.Printf("%d secret ref(s):\n", len(refs))
	for _, ref := range refs {
		val, err := s.watcher.Client().Resolver.Resolve(ctx, ref.secretRef)
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
	val, err := s.watcher.Client().Resolver.Resolve(ctx, ref)
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
	st := s.watcher.ReadSnapshot()
	if changed {
		fmt.Printf("fetch: new snapshot version=%d\n", st.LastVersion)
	} else {
		fmt.Printf("fetch: unchanged (version=%d)\n", st.LastVersion)
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
		st := s.watcher.ReadSnapshot()
		fmt.Printf("status:    %s\n", st.PollStatus)
		if st.PollErr != nil {
			fmt.Printf("last err:  %v\n", st.PollErr)
		}
		if !st.LastHeartbeat.IsZero() {
			fmt.Printf("heartbeat: %s ago\n", time.Since(st.LastHeartbeat).Round(time.Second))
		} else {
			fmt.Println("heartbeat: never")
		}
		if !st.LastFetch.IsZero() {
			fmt.Printf("fetch:     %s ago (version=%d)\n", time.Since(st.LastFetch).Round(time.Second), st.LastVersion)
		} else {
			fmt.Println("fetch:     never")
		}

	case "history":
		entries := s.watcher.HistoryEntries()
		if len(entries) == 0 {
			fmt.Println("no snapshots received yet")
			return nil
		}
		fmt.Printf("%-32s  %-8s  %-18s  %s\n", "RECEIVED", "VERSION", "CHECKSUM", "PROV/SRV/PROF")
		for _, e := range entries {
			chk := fmt.Sprintf("%x", e.Checksum)
			if len(chk) > 16 {
				chk = chk[:16] + "…"
			}
			fmt.Printf("%-32s  %-8d  %-18s  %d/%d/%d\n",
				e.ReceivedAt.Format(time.RFC3339),
				e.Version, chk,
				e.Providers, e.Servers, e.Profiles,
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
  orange admin repl                           admin-scoped tasks/records
  ORANGE_API_KEY=<key> orange --repl          user-scoped records

`)
}
