package server

// egress_serve_local.go — implementation of "orange egress serve --local".
//
// Responsibilities:
//   1. Start redis-server on a random local port.
//   2. Start an in-process RLS gRPC server backed by that Redis.
//   3. Watch orange.yaml for changes; reload RLS immediately on save.
//   4. Launch the Envoy subprocess (same template as normal mode).
//   5. Enter an interactive REPL for live config + datapath inspection.
//
// Lifecycle: context cancellation (SIGINT/SIGTERM or REPL "exit") shuts down
// RLS, kills redis-server (via exec.CommandContext), sends SIGTERM to Envoy,
// and waits for Envoy to drain before returning.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/chzyer/readline"
	pb "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	goredis "github.com/redis/go-redis/v9"
	"google.golang.org/grpc"

	"github.com/dio/transit/examples/orange/internal/client"
	orangeconfig "github.com/dio/transit/examples/orange/internal/config"
	"github.com/dio/transit/examples/orange/internal/rls"
)

type localServeOpts struct {
	configPath string // absolute path to orange.yaml (or remote snapshot temp file)
	envoyBin   string
	modulePath string
	trustedCA  string
	logLevel   string
	rlsListen  string
	interval   time.Duration
	noRLS      bool
	serverURL  string // when set: fetch config from this orange server via bundle
	bundlePath string // egress bundle dir or .tar.gz; required when serverURL != ""
}

type serveLocalState struct {
	configPath string
	snapshotFn rls.SnapshotFunc
	provider   *rls.PollProvider // nil when --no-rls
	resolver   *orangeconfig.CachedResolver
	redisPort  string // empty when --no-rls
	envoyCmd   *exec.Cmd
	cancel     context.CancelFunc
	rl         *readline.Instance

	// LLM test state (shared across commands within a session).
	llmBaseURL string
	llmKey     string
	llmModel   string // empty = auto per API
	llmStream  bool
	llmClient  *http.Client

	// Image generation state.
	imgModel string // empty = auto (dall-e-3)
	imgDir   string

	// MCP test state.
	mcpBaseURL   string
	mcpSessionID string
	mcpProfile   string
	mcpServer    string
}

func runEgressServeLocal(parentCtx context.Context, opts localServeOpts) error {
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	// ── Connected mode: replace local config with a remote snapshot ───────────
	if opts.serverURL != "" {
		tmpPath, err := setupRemoteSnapshot(ctx, &opts)
		if err != nil {
			return err
		}
		defer os.Remove(tmpPath) //nolint:errcheck
	}

	snapshotFn := localRLSSnapshotFn(opts.configPath)

	// Build the secret resolver. In connected mode, load bundle credentials so
	// the REPL's "secrets" command can resolve orange:// refs against the server.
	var resolverHTTPClient *http.Client
	if opts.bundlePath != "" && opts.serverURL != "" {
		bundle, err := client.LoadBundle(opts.bundlePath)
		if err == nil {
			bundle.ServerURL = opts.serverURL
			resolverHTTPClient, _ = client.NewHTTPClient(bundle)
		}
	}
	resolver := orangeconfig.NewDefaultResolver(resolverHTTPClient, opts.serverURL, 5*time.Minute)

	var (
		redisPort string
		provider  *rls.PollProvider
	)

	if !opts.noRLS {
		redisBin, err := exec.LookPath("redis-server")
		if err != nil {
			return fmt.Errorf("redis-server not found in PATH: %w\nhint: brew install redis  (or pass --no-rls to skip)", err)
		}

		port, err := freePort()
		if err != nil {
			return fmt.Errorf("find free port for redis: %w", err)
		}
		redisPort = fmt.Sprintf("%d", port)
		redisAddr := "127.0.0.1:" + redisPort

		// Use CommandContext so redis-server is killed when ctx is cancelled.
		redisProc := exec.CommandContext(ctx, redisBin,
			"--port", redisPort,
			"--loglevel", "warning",
			"--daemonize", "no",
			"--save", "",
		)
		redisProc.Stderr = os.Stderr
		if err := redisProc.Start(); err != nil {
			return fmt.Errorf("start redis-server: %w", err)
		}
		// Reap the process when it exits so we don't leave zombies.
		go func() { _ = redisProc.Wait() }()

		fmt.Fprintf(os.Stderr, "egress:local  redis addr=%s\n", redisAddr)
		fmt.Fprintf(os.Stderr, "egress:local  redis inspect: redis-cli -p %s\n", redisPort)

		redisClient := goredis.NewClient(&goredis.Options{Addr: redisAddr})
		if err := waitRedis(ctx, redisClient, 5*time.Second); err != nil {
			_ = redisClient.Close()
			return err
		}

		loader := rls.OrangeLoader(snapshotFn)
		provider = rls.NewPollProvider(loader, opts.interval)
		if err := provider.LoadOnce(ctx); err != nil {
			_ = redisClient.Close()
			return fmt.Errorf("initial rls load: %w", err)
		}
		if raw, _ := snapshotFn(); raw != nil && len(raw.RateLimit.Policies) > 0 {
			fmt.Fprintf(os.Stderr, "egress:local  rls: scopes=%d tiers=%d\n",
				len(raw.RateLimit.Policies), len(raw.RateLimit.Tiers))
		}

		svc := rls.NewService(provider, rls.NewRateLimiter(redisClient, rls.NewNoopStatsManager()))
		grpcSrv := grpc.NewServer()
		pb.RegisterRateLimitServiceServer(grpcSrv, svc)

		lis, err := net.Listen("tcp", opts.rlsListen)
		if err != nil {
			_ = redisClient.Close()
			return fmt.Errorf("rls listen %s: %w", opts.rlsListen, err)
		}
		fmt.Fprintf(os.Stderr, "egress:local  rls gRPC=%s\n", opts.rlsListen)

		go provider.Start(ctx)
		go func() { _ = grpcSrv.Serve(lis) }()
		go func() {
			<-ctx.Done()
			grpcSrv.GracefulStop()
			_ = redisClient.Close()
		}()

		// File watcher reloads RLS immediately on config save.
		go orangeconfig.WatchFile(ctx, opts.configPath, func() {
			provider.Reload(ctx)
			fmt.Fprintf(os.Stderr, "\n[watch] config reloaded from %s\n", filepath.Base(opts.configPath))
		})
	}

	// ── Envoy subprocess ──────────────────────────────────────────────────────
	rendered := strings.ReplaceAll(envoyConfigTemplate, "${ORANGE_TRUSTED_CA}", opts.trustedCA)
	envoyCmd := exec.Command(opts.envoyBin, "--config-yaml", rendered, "--log-level", opts.logLevel)
	envoyCmd.Env = append(os.Environ(),
		"GODEBUG=cgocheck=0",
		"ORANGE_CONFIG="+opts.configPath,
		"ENVOY_DYNAMIC_MODULES_SEARCH_PATH="+opts.modulePath,
	)
	// Forward bundle credentials so liborange.so can resolve orange:// refs.
	if opts.serverURL != "" {
		envoyCmd.Env = append(envoyCmd.Env, "ORANGE_SERVER_URL="+opts.serverURL)
	}
	if opts.bundlePath != "" {
		envoyCmd.Env = append(envoyCmd.Env, "ORANGE_EGRESS_BUNDLE="+opts.bundlePath)
	}
	envoyCmd.Stdout = os.Stdout
	envoyCmd.Stderr = os.Stderr
	if err := envoyCmd.Start(); err != nil {
		return fmt.Errorf("start envoy: %w", err)
	}
	fmt.Fprintf(os.Stderr, "egress:local  envoy pid=%d\n", envoyCmd.Process.Pid)
	fmt.Fprintln(os.Stderr)

	// Envoy exit → cancel everything (brings down RLS + redis).
	envoyExitCh := make(chan error, 1)
	go func() {
		envoyExitCh <- envoyCmd.Wait()
		cancel()
	}()

	// ctx done → SIGTERM envoy for graceful drain.
	go func() {
		<-ctx.Done()
		_ = envoyCmd.Process.Signal(syscall.SIGTERM)
	}()

	// ── REPL ──────────────────────────────────────────────────────────────────
	histFile := os.ExpandEnv("$HOME/.orange/egress_serve_history")
	_ = os.MkdirAll(os.ExpandEnv("$HOME/.orange"), 0o700)

	rl, err := readline.NewEx(&readline.Config{
		Prompt:          "egress:local> ",
		HistoryFile:     histFile,
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
	})
	if err != nil {
		cancel()
		<-envoyExitCh
		return err
	}
	defer rl.Close()

	// ctx done → close readline to unblock rl.Readline().
	go func() {
		<-ctx.Done()
		rl.Close()
	}()

	state := &serveLocalState{
		configPath: opts.configPath,
		snapshotFn: snapshotFn,
		provider:   provider,
		resolver:   resolver,
		redisPort:  redisPort,
		envoyCmd:   envoyCmd,
		cancel:     cancel,
		rl:         rl,

		llmBaseURL: "http://localhost:8080",
		llmKey:     firstConfigKey(snapshotFn),
		llmClient:  &http.Client{}, // no global timeout; rely on context

		imgDir: "/tmp",

		mcpBaseURL: "http://localhost:8080/mcp",
	}

	fmt.Println("egress:local REPL  •  type 'help' for commands, 'exit' or Ctrl+D to quit")
	fmt.Println()

	for {
		rl.SetPrompt(state.prompt())
		line, err := rl.Readline()
		if errors.Is(err, readline.ErrInterrupt) {
			if line == "" {
				// Ctrl+C on empty line → graceful shutdown.
				cancel()
				break
			}
			continue
		}
		if errors.Is(err, io.EOF) || err != nil {
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if dispErr := state.dispatch(ctx, line); dispErr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", dispErr)
		}
	}

	// Wait for Envoy to finish draining.
	<-envoyExitCh
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

// freePort returns an available TCP port on the loopback interface.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port, nil
}

// waitRedis pings client repeatedly until it responds or the timeout elapses.
func waitRedis(ctx context.Context, client *goredis.Client, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if client.Ping(ctx).Err() == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled waiting for redis-server")
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("redis-server did not become ready within %s", timeout)
}

// ── REPL ─────────────────────────────────────────────────────────────────────

func (s *serveLocalState) prompt() string {
	return fmt.Sprintf("egress:local [%s]> ", filepath.Base(s.configPath))
}

func (s *serveLocalState) dispatch(ctx context.Context, line string) error {
	toks := strings.Fields(line)
	if len(toks) == 0 {
		return nil
	}
	switch toks[0] {
	case "exit", "quit":
		s.cancel()
		s.rl.Close()
		return nil

	case "help", "?":
		printServeLocalHelp()

	case "snapshot", "snap":
		full := len(toks) > 1 && toks[1] == "full"
		return s.cmdSnapshot(full)

	case "rls":
		return s.cmdRLS()

	case "secrets", "sec":
		return s.cmdSecrets(ctx)

	case "reload":
		return s.cmdReload(ctx)

	case "redis":
		return s.cmdRedis()

	case "envoy":
		return s.cmdEnvoy()

	case "config", "cfg":
		fmt.Printf("config: %s\n", s.configPath)

	case "llm":
		return s.cmdLLM(ctx, toks[1:])

	case "img":
		return s.cmdImg(ctx, toks[1:])

	case "mcp":
		return s.cmdMCP(ctx, toks[1:])

	default:
		return fmt.Errorf("unknown command %q — type 'help' for a list", toks[0])
	}
	return nil
}

// ── snapshot ──────────────────────────────────────────────────────────────────

func (s *serveLocalState) cmdSnapshot(full bool) error {
	raw, err := s.snapshotFn()
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	fmt.Printf("config:     %s\n", s.configPath)
	fmt.Printf("providers:  %d\n", len(raw.LLM.Providers))
	fmt.Printf("models:     %d\n", len(raw.LLM.Models))
	fmt.Printf("mcp:        %d\n", len(raw.MCP.Servers))
	fmt.Printf("profiles:   %d\n", len(raw.Profiles))
	fmt.Printf("keys:       %d\n", len(raw.Keys))
	fmt.Printf("rl-scopes:  %d\n", len(raw.RateLimit.Policies))
	fmt.Printf("rl-tiers:   %d\n", len(raw.RateLimit.Tiers))
	if full {
		fmt.Println()
		for name := range raw.LLM.Providers {
			fmt.Printf("  provider  %s\n", name)
		}
		for name := range raw.LLM.Models {
			fmt.Printf("  model     %s\n", name)
		}
		for name := range raw.MCP.Servers {
			fmt.Printf("  mcp       %s\n", name)
		}
		for id := range raw.Profiles {
			fmt.Printf("  profile   %s\n", id)
		}
		for k := range raw.Keys {
			fmt.Printf("  key       %s\n", k)
		}
	}
	return nil
}

// ── rls ───────────────────────────────────────────────────────────────────────

func (s *serveLocalState) cmdRLS() error {
	if s.provider == nil {
		fmt.Println("rls: not running (--no-rls)")
		return nil
	}
	raw, err := s.snapshotFn()
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	printRLSPolicies("current", raw)
	return nil
}

// ── secrets ───────────────────────────────────────────────────────────────────

func (s *serveLocalState) cmdSecrets(ctx context.Context) error {
	raw, err := s.snapshotFn()
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	refs := collectSecretRefs(raw)
	if len(refs) == 0 {
		fmt.Println("no secret refs in config")
		return nil
	}
	fmt.Printf("%d secret ref(s):\n", len(refs))
	for _, ref := range refs {
		val, err := s.resolver.Resolve(ctx, ref.secretRef)
		if err != nil {
			fmt.Printf("  [%s] %s => ERROR: %v\n", ref.location, ref.secretRef, err)
		} else {
			fmt.Printf("  [%s] %s => %s\n", ref.location, ref.secretRef, maskSecret(val))
		}
	}
	return nil
}

// ── reload ────────────────────────────────────────────────────────────────────

func (s *serveLocalState) cmdReload(ctx context.Context) error {
	if s.provider != nil {
		s.provider.Reload(ctx)
	}
	raw, err := s.snapshotFn()
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	fmt.Printf("reloaded  providers=%d models=%d mcp=%d profiles=%d keys=%d rl-scopes=%d\n",
		len(raw.LLM.Providers), len(raw.LLM.Models), len(raw.MCP.Servers),
		len(raw.Profiles), len(raw.Keys), len(raw.RateLimit.Policies))
	return nil
}

// ── redis ─────────────────────────────────────────────────────────────────────

func (s *serveLocalState) cmdRedis() error {
	if s.redisPort == "" {
		fmt.Println("redis: not running (--no-rls)")
		return nil
	}
	fmt.Printf("redis-cli -p %s\n", s.redisPort)
	fmt.Printf("redis-cli -p %s KEYS '*'\n", s.redisPort)
	return nil
}

// ── envoy ─────────────────────────────────────────────────────────────────────

func (s *serveLocalState) cmdEnvoy() error {
	if s.envoyCmd.Process == nil {
		fmt.Println("envoy: not started")
		return nil
	}
	fmt.Printf("envoy pid=%d\n", s.envoyCmd.Process.Pid)
	return nil
}

// ── help ──────────────────────────────────────────────────────────────────────

func printServeLocalHelp() {
	fmt.Print(`
egress:local REPL — Envoy + redis-server + RLS running locally

Config inspection (reads orange.yaml live on every call):
  snapshot [full]    config summary; 'full' lists providers/models/keys/etc.
  rls                rate-limit policy tree as currently enforced by RLS
  secrets            resolve secret refs in config (values masked)
  config / cfg       show config file path

Reload:
  reload             force RLS to re-read config + print summary
                     (file watcher also triggers this automatically on save)

LLM testing (routes through local Envoy on :8080):
  llm <message>              chat/completions (OpenAI, gpt-4o-mini)
  llm stream <message>       streaming chat/completions
  llm resp <message>         /v1/responses (OpenAI Responses API, gpt-4o-mini)
  llm msg <message>          /v1/messages (Anthropic, claude-haiku-4-5)
  llm models                 list models
  llm set key <slug>         switch API key (e.g. demo/dio/sk-fallback)
  llm set model <name>       pin model ('auto' to reset)
  llm set stream on|off      toggle default streaming
  llm status                 show current LLM settings
  llm help                   full LLM command reference

Image generation (routes through local Envoy on :8080):
  img <prompt>               POST /v1/images/generations → PNG saved to imgdir
  img models                 list image-capable models
  img set model <name>       pin image model ('auto' to reset, default: dall-e-3)
  img set dir <path>         output dir for images (default: /tmp)
  img status                 show current image settings
  img help                   full image command reference

MCP (routes through local Envoy on :8080/mcp):
  mcp profile list           list profiles from orange.yaml
  mcp server  list           list servers  from orange.yaml
  mcp profile <name>         set active profile + initialize session
  mcp server  <name>         set active server  + initialize session
  mcp initialize             (re)initialize current target
  mcp list                   list available tools
  mcp call <tool> [<json>]   call a tool with JSON arguments
  mcp stream                 stream SSE events from current session
  mcp delete                 delete current session
  mcp set base <url>         change MCP base URL
  mcp status                 show current MCP settings + session ID
  mcp help                   full MCP command reference

Services:
  redis              print redis-cli command for the local Redis
  envoy              show Envoy process info (pid)

Other:
  help / ?           this help
  exit / quit        send SIGTERM to Envoy, stop RLS + Redis, and exit

Note: Envoy reads ORANGE_CONFIG on startup. Editing orange.yaml takes effect
      for RLS immediately; for Envoy it depends on the orange module's reload.

`)
}
