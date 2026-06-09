package server

// rls.go — proxy-facing RLS subcommands.
//
// "orange rls" mirrors the "orange egress" pattern: it uses bundle credentials
// (egress.key / config.yaml) rather than an admin API key, because the RLS
// service authenticates to orange CP exactly the same way as the egress proxy.
//
// Subcommands:
//   orange rls serve          — run the Envoy RateLimitService v3 gRPC server
//   orange rls serve --local  — same, but with embedded miniredis + local YAML config
//   orange rls emulate        — poll CP and print translated rate-limit config (debug)

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	pb "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	goredis "github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"gopkg.in/yaml.v3"

	orangeconfig "github.com/dio/transit/examples/orange/internal/config"
	"github.com/dio/transit/examples/orange/internal/client"
	"github.com/dio/transit/examples/orange/internal/rls"
)

// newRLSCmd is the root for RLS subcommands. Like "orange egress", it operates
// with bundle credentials, not an admin API key.
func newRLSCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rls",
		Short: "Rate-limit service operations (uses bundle credentials, not admin API key)",
	}
	cmd.AddCommand(newRLSServeCmd())
	cmd.AddCommand(newRLSEmulateCmd())
	return cmd
}

func newRLSServeCmd() *cobra.Command {
	var (
		bundlePath string
		redisAddr  string
		listenAddr string
		interval   time.Duration
		localMode  bool
		configPath string
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the Envoy RateLimitService v3 gRPC server",
		Long: `Serve starts a gRPC RateLimitService (Envoy RLS v3) that enforces
rate limits against Redis using pipelined INCRBY + EXPIRE.

Normal mode (requires an egress bundle):
  1. Loads a bundle for CP authentication.
  2. Polls orange CP for rate-limit policy config.
  3. Enforces limits against an external Redis.

Local mode (--local, no bundle needed):
  1. Reads config from --config (or ORANGE_CONFIG).
  2. Starts an embedded miniredis on a random port.
  3. Prints the redis-cli command so you can inspect counters live.
  4. Watches the config file with fsnotify; reloads instantly on save.

Use "orange rls emulate" to inspect translated config without a gRPC server.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			var (
				snapshotFn rls.SnapshotFunc
				redisOpts  *goredis.Options
				absConfig  string // non-empty in local mode; used to start file watcher
			)

			if localMode {
				// ── local mode: embedded miniredis + YAML config file ─────────
				if configPath == "" {
					configPath = os.Getenv("ORANGE_CONFIG")
				}
				if configPath == "" {
					configPath = "orange.yaml"
				}
				if _, err := os.Stat(configPath); err != nil {
					return fmt.Errorf("config file not found: %s (use --config or set ORANGE_CONFIG)", configPath)
				}
				var err error
				absConfig, err = filepath.Abs(configPath)
				if err != nil {
					return fmt.Errorf("resolve config path: %w", err)
				}

				mr, err := miniredis.Run()
				if err != nil {
					return fmt.Errorf("start miniredis: %w", err)
				}
				defer mr.Close()

				fmt.Fprintf(os.Stderr, "rls:local  config=%s\n", absConfig)
				fmt.Fprintf(os.Stderr, "rls:local  miniredis addr=%s\n", mr.Addr())
				fmt.Fprintf(os.Stderr, "rls:local  inspect:  echo 'KEYS *' | redis-cli -p %s\n\n", mr.Port())

				redisOpts = &goredis.Options{Addr: mr.Addr()}
				snapshotFn = localRLSSnapshotFn(absConfig)
			} else {
				// ── normal mode: bundle + external Redis ──────────────────────
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
				c, err := client.NewClient(bundle)
				if err != nil {
					return err
				}
				redisOpts = &goredis.Options{Addr: redisAddr}
				snapshotFn = rlsSnapshotFn(ctx, c)
			}

			loader := rls.OrangeLoader(snapshotFn)
			provider := rls.NewPollProvider(loader, interval)
			if err := provider.LoadOnce(ctx); err != nil {
				return fmt.Errorf("initial rls load: %w", err)
			}
			raw, _ := snapshotFn()
			if raw != nil && len(raw.RateLimit.Policies) > 0 {
				fmt.Fprintf(os.Stderr, "rls: initial config loaded  scopes=%d tiers=%d\n", len(raw.RateLimit.Policies), len(raw.RateLimit.Tiers))
				printRLSPolicies("rls:config", raw)
			} else {
				slog.Info("rls: initial config loaded")
			}

			if absConfig != "" {
				// Watch the config file for writes; reload immediately on change rather
				// than waiting for the next --interval tick.
				go orangeconfig.WatchFile(ctx, absConfig, func() { provider.Reload(ctx) })
			}

			redisClient := goredis.NewClient(redisOpts)
			defer func() { _ = redisClient.Close() }()
			if err := redisClient.Ping(ctx).Err(); err != nil {
				return fmt.Errorf("redis ping %s: %w", redisOpts.Addr, err)
			}
			slog.Info("rls: redis connected", "addr", redisOpts.Addr)

			svc := rls.NewService(provider, rls.NewRateLimiter(redisClient, rls.NewNoopStatsManager()))

			grpcSrv := grpc.NewServer()
			pb.RegisterRateLimitServiceServer(grpcSrv, svc)

			lis, err := net.Listen("tcp", listenAddr)
			if err != nil {
				return fmt.Errorf("listen %s: %w", listenAddr, err)
			}
			slog.Info("rls: gRPC server listening", "addr", listenAddr)

			go provider.Start(ctx)

			errCh := make(chan error, 1)
			go func() { errCh <- grpcSrv.Serve(lis) }()

			select {
			case <-ctx.Done():
				slog.Info("rls: shutting down")
				grpcSrv.GracefulStop()
				return nil
			case err := <-errCh:
				return err
			}
		},
	}
	cmd.Flags().StringVar(&bundlePath, "bundle", "", "bundle dir or .tar.gz (env: ORANGE_EGRESS_BUNDLE)")
	cmd.Flags().StringVar(&redisAddr, "redis-addr", "localhost:6379", "Redis address (ignored in --local mode)")
	cmd.Flags().StringVar(&listenAddr, "listen", ":8081", "gRPC listen address")
	cmd.Flags().DurationVar(&interval, "interval", 30*time.Second, "config poll interval")
	cmd.Flags().BoolVar(&localMode, "local", false, "embedded miniredis + YAML config file (no bundle or external Redis needed)")
	cmd.Flags().StringVar(&configPath, "config", "", "orange config file for --local mode (env: ORANGE_CONFIG)")
	return cmd
}

// localRLSSnapshotFn returns a SnapshotFunc that reads and parses an orange YAML
// config file on each call. It is invoked on initial load, on each poll tick,
// and on demand by the file watcher via provider.Reload.
func localRLSSnapshotFn(path string) rls.SnapshotFunc {
	return func() (*orangeconfig.RawConfig, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
		var raw orangeconfig.RawConfig
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
		return &raw, nil
	}
}

func newRLSEmulateCmd() *cobra.Command {
	var (
		bundlePath string
		interval   time.Duration
		once       bool
	)
	cmd := &cobra.Command{
		Use:   "emulate",
		Short: "Poll orange CP and display translated rate-limit config",
		Long: `Emulate loads a bundle, polls orange CP for config, and prints the
translated rate-limit policy tree — the same config the serve command would
enforce. Useful for verifying policies before running the gRPC service.

Use --once for a single pass. Interrupt with CTRL-C.`,
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

			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			c, err := client.NewClient(bundle)
			if err != nil {
				return err
			}

			fmt.Printf("egress_id:    %s\n", bundle.EgressID)
			fmt.Printf("workspace_id: %s\n", bundle.WorkspaceID)
			fmt.Printf("server_url:   %s\n", bundle.ServerURL)
			fmt.Println()

			tick := func() {
				ts := time.Now().Format(time.RFC3339)

				serverTime, hbErr := c.Heartbeat(ctx)
				if hbErr != nil {
					fmt.Printf("[%s] heartbeat  ERROR %v\n", ts, hbErr)
				} else {
					fmt.Printf("[%s] heartbeat  OK server_time=%s\n", ts, serverTime.Format(time.RFC3339))
				}

				_, raw, changed, fetchErr := c.Fetch(ctx)
				if fetchErr != nil {
					fmt.Printf("[%s] fetch ERROR %v\n", ts, fetchErr)
					return
				}
				if !changed {
					fmt.Printf("[%s] fetch unchanged\n", ts)
					return
				}
				printRLSPolicies(ts, raw)
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
		},
	}
	cmd.Flags().StringVar(&bundlePath, "bundle", "", "bundle dir or .tar.gz (env: ORANGE_EGRESS_BUNDLE)")
	cmd.Flags().DurationVar(&interval, "interval", 30*time.Second, "poll interval")
	cmd.Flags().BoolVar(&once, "once", false, "single pass and exit")
	return cmd
}

// printRLSPolicies prints the rate-limit policies from a decoded snapshot.
func printRLSPolicies(ts string, raw *orangeconfig.RawConfig) {
	if raw == nil || len(raw.RateLimit.Policies) == 0 {
		fmt.Printf("[%s] ratelimit no policies in snapshot\n", ts)
		return
	}
	fmt.Printf("[%s] ratelimit scopes=%d tiers=%d\n", ts,
		len(raw.RateLimit.Policies), len(raw.RateLimit.Tiers))

	scopes := make([]string, 0, len(raw.RateLimit.Policies))
	for k := range raw.RateLimit.Policies {
		scopes = append(scopes, k)
	}
	sort.Strings(scopes)

	for _, scope := range scopes {
		entries := raw.RateLimit.Policies[scope]
		fmt.Printf("  scope %s (%d rule(s))\n", scope, len(entries))
		for _, e := range entries {
			models := strings.Join(e.Models, ",")
			if models == "" {
				models = "*"
			}
			var dims []string
			if e.Rule != "" {
				dims = append(dims, "rule="+e.Rule)
			}
			if e.RPM > 0 {
				dims = append(dims, fmt.Sprintf("rpm=%d", e.RPM))
			}
			if e.RPH > 0 {
				dims = append(dims, fmt.Sprintf("rph=%d", e.RPH))
			}
			if e.RPD > 0 {
				dims = append(dims, fmt.Sprintf("rpd=%d", e.RPD))
			}
			if e.InputTokensPerMinute > 0 {
				dims = append(dims, fmt.Sprintf("input_tpm=%d", e.InputTokensPerMinute))
			}
			if e.InputTokensPerHour > 0 {
				dims = append(dims, fmt.Sprintf("input_tph=%d", e.InputTokensPerHour))
			}
			if e.InputTokensPerDay > 0 {
				dims = append(dims, fmt.Sprintf("input_tpd=%d", e.InputTokensPerDay))
			}
			if e.OutputTokensPerMinute > 0 {
				dims = append(dims, fmt.Sprintf("output_tpm=%d", e.OutputTokensPerMinute))
			}
			if e.OutputTokensPerHour > 0 {
				dims = append(dims, fmt.Sprintf("output_tph=%d", e.OutputTokensPerHour))
			}
			if e.OutputTokensPerDay > 0 {
				dims = append(dims, fmt.Sprintf("output_tpd=%d", e.OutputTokensPerDay))
			}
			if e.CacheReadTokensPerHour > 0 {
				dims = append(dims, fmt.Sprintf("cache_read_tph=%d", e.CacheReadTokensPerHour))
			}
			if e.CacheReadTokensPerDay > 0 {
				dims = append(dims, fmt.Sprintf("cache_read_tpd=%d", e.CacheReadTokensPerDay))
			}
			if e.CacheWriteTokensPerHour > 0 {
				dims = append(dims, fmt.Sprintf("cache_write_tph=%d", e.CacheWriteTokensPerHour))
			}
			if e.CacheWriteTokensPerDay > 0 {
				dims = append(dims, fmt.Sprintf("cache_write_tpd=%d", e.CacheWriteTokensPerDay))
			}
			if !e.USDPerMinute.IsZero() {
				dims = append(dims, "usd_per_min="+e.USDPerMinute.String())
			}
			if !e.USDPerHour.IsZero() {
				dims = append(dims, "usd_per_hour="+e.USDPerHour.String())
			}
			if !e.USDPerDay.IsZero() {
				dims = append(dims, "usd_per_day="+e.USDPerDay.String())
			}
			if e.OnExceed != "" {
				dims = append(dims, "on_exceed="+e.OnExceed)
			}
			fmt.Printf("    [%s] %s\n", models, strings.Join(dims, " "))
		}
	}
}

// rlsSnapshotFn builds a SnapshotFunc from a context-bound egress client.
// The snapshot is cached so that transient fetch failures don't clear the
// last good config inside a PollProvider reload.
func rlsSnapshotFn(ctx context.Context, c *client.Client) func() (*orangeconfig.RawConfig, error) {
	var cached *orangeconfig.RawConfig
	return func() (*orangeconfig.RawConfig, error) {
		_, raw, _, err := c.Fetch(ctx)
		if err != nil {
			if cached != nil {
				slog.Warn("rls: fetch failed, using cached snapshot", "err", err)
				return cached, nil
			}
			return nil, err
		}
		if raw != nil {
			cached = raw
		}
		if cached == nil {
			return nil, fmt.Errorf("rls: no snapshot available yet")
		}
		return cached, nil
	}
}
