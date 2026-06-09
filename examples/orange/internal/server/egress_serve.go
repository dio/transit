package server

// egress_serve.go — "orange egress serve" launches the Envoy proxy.
//
// Non-local mode (default): renders envoy.tmpl.yaml inline and launches Envoy.
// Requires --config / ORANGE_CONFIG. Signal handling forwards SIGINT/SIGTERM
// to Envoy for graceful drain.
//
// Local mode (--local): launches Envoy + an embedded redis-server + an
// in-process RLS gRPC server, then enters an interactive REPL for live config
// inspection. --config defaults to orange.yaml. redis-server must be installed.

import (
	"bufio"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

//go:embed static/envoy.tmpl.yaml
var envoyConfigTemplate string

func newEgressServeCmd() *cobra.Command {
	var (
		envoyBin   string
		orangeCfg  string
		modulePath string
		trustedCA  string
		logLevel   string
		localMode  bool
		noRLS      bool
		rlsListen  string
		interval   time.Duration
		serverURL  string
		bundlePath string
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the Envoy proxy for this egress instance",
		Long: `Serve renders the embedded Envoy config template and launches Envoy with
the orange dynamic module — equivalent to "make run" but self-contained.

The rendered config is passed inline via --config-yaml; no file is written.

Normal mode:
  Requires --config / ORANGE_CONFIG. Envoy only.
  Signals (SIGINT/SIGTERM) are forwarded to Envoy for graceful drain.

Local mode (--local):
  Launches Envoy + redis-server + in-process RLS, then enters a REPL for
  live config inspection and datapath debugging. --config defaults to
  orange.yaml. redis-server must be installed (brew install redis).

Environment variable mapping:
  --envoy-bin    ENVOY_BIN                          see resolution order below
  --config       ORANGE_CONFIG                      required in normal mode; default orange.yaml in --local
  --module-path  ENVOY_DYNAMIC_MODULES_SEARCH_PATH  default: exe dir, then cwd
  --trusted-ca   ORANGE_TRUSTED_CA                  default: OS trust bundle

Envoy binary resolution order:
  1. --envoy-bin / ENVOY_BIN
  2. ~/.orange/bin/envoy
  3. .bin/envoy walking up from cwd (transit repo checkout)
  4. envoy in PATH (warn: system builds lack dynamic_modules)

Module path resolution order (for liborange.so / .dylib):
  1. --module-path / ENVOY_DYNAMIC_MODULES_SEARCH_PATH
  2. Directory of the orange binary (if liborange.* found there)
  3. cwd (covers "go run ./cmd/orange" from examples/orange/)`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// ── .env loading ──────────────────────────────────────────────────
			// Load .env from CWD (mirrors what "make run" does). Keys already
			// present in the shell environment are never overwritten.
			if n, err := applyDotEnv(".env"); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "warning: .env: %v\n", err)
			} else if n > 0 {
				fmt.Fprintf(os.Stderr, "egress:serve  loaded .env (%d var(s))\n", n)
			}

			// ── envoy binary (common to both modes) ───────────────────────────
			// Resolution order:
			//   1. --envoy-bin flag / ENVOY_BIN env var (explicit)
			//   2. ~/.orange/bin/envoy  (designated install location)
			//   3. .bin/envoy walking up from cwd (transit repo checkout)
			//   4. envoy in PATH (warn: system envoy lacks dynamic_modules)
			if envoyBin == "" {
				envoyBin = os.Getenv("ENVOY_BIN")
			}
			if envoyBin == "" {
				home, _ := os.UserHomeDir()
				envoyBin = filepath.Join(home, ".orange", "bin", "envoy")
			}
			if _, err := os.Stat(envoyBin); err != nil {
				// Walk up from cwd to find the repo's .bin/envoy.
				envoyBin = findBinEnvoy()
			}
			if envoyBin == "" {
				found, lerr := exec.LookPath("envoy")
				if lerr != nil {
					return fmt.Errorf("envoy binary not found\n" +
						"  tried: ~/.orange/bin/envoy, .bin/envoy (repo), PATH\n" +
						"  run 'make download-envoy' in the transit repo root, then:\n" +
						"    export ENVOY_BIN=$(pwd)/../../.bin/envoy  (from examples/orange)\n" +
						"    --envoy-bin <path>")
				}
				envoyBin = found
				fmt.Fprintf(os.Stderr, "warning: using system envoy %s — system builds lack dynamic_modules support\n", envoyBin)
				fmt.Fprintf(os.Stderr, "         run 'make download-envoy' and set ENVOY_BIN or --envoy-bin\n")
			}

			// ── trusted CA (common) ───────────────────────────────────────────
			if trustedCA == "" {
				trustedCA = os.Getenv("ORANGE_TRUSTED_CA")
			}
			if trustedCA == "" {
				trustedCA = detectTrustedCA()
			}
			if trustedCA == "" {
				return fmt.Errorf("cannot detect TLS trust bundle; set --trusted-ca or ORANGE_TRUSTED_CA")
			}
			if _, err := os.Stat(trustedCA); err != nil {
				return fmt.Errorf("trusted CA bundle %q not readable: %w", trustedCA, err)
			}

			// ── dynamic module search path (common) ───────────────────────────
			// Resolution order:
			//   1. --module-path flag / ENVOY_DYNAMIC_MODULES_SEARCH_PATH
			//   2. Directory of the orange binary (normal installed binary)
			//   3. cwd — covers "go run" where the exe is a temp path and
			//      liborange.so lives in the working directory
			if modulePath == "" {
				modulePath = os.Getenv("ENVOY_DYNAMIC_MODULES_SEARCH_PATH")
			}
			if modulePath == "" {
				if self, err := os.Executable(); err == nil {
					if hasOrangeModule(filepath.Dir(self)) {
						modulePath = filepath.Dir(self)
					}
				}
			}
			if modulePath == "" {
				modulePath = "." // cwd fallback for "go run" dev workflow
			}

			// ── local mode ────────────────────────────────────────────────────
			if localMode {
				// Resolve server URL: flag → env.
				if serverURL == "" {
					serverURL = os.Getenv("ORANGE_SERVER_URL")
				}

				var absOrangeCfg string
				if serverURL != "" {
					// Connected mode: config comes from the remote server via bundle.
					if bundlePath == "" {
						bundlePath = os.Getenv("ORANGE_EGRESS_BUNDLE")
					}
					if bundlePath == "" {
						return fmt.Errorf("--bundle or ORANGE_EGRESS_BUNDLE is required when --server-url / ORANGE_SERVER_URL is set")
					}
				} else {
					// Standalone mode: config comes from a local orange.yaml.
					if orangeCfg == "" {
						orangeCfg = os.Getenv("ORANGE_CONFIG")
					}
					if orangeCfg == "" {
						orangeCfg = "orange.yaml"
					}
					var err error
					absOrangeCfg, err = filepath.Abs(orangeCfg)
					if err != nil {
						return fmt.Errorf("resolve config path: %w", err)
					}
					if _, err := os.Stat(absOrangeCfg); err != nil {
						return fmt.Errorf("config file not found: %s (use --config or set ORANGE_CONFIG)", absOrangeCfg)
					}
				}

				fmt.Fprintf(os.Stderr, "egress:serve  envoy=%s\n", envoyBin)
				fmt.Fprintf(os.Stderr, "egress:serve  mode=local\n")
				if serverURL != "" {
					fmt.Fprintf(os.Stderr, "egress:serve  ORANGE_SERVER_URL=%s\n", serverURL)
					fmt.Fprintf(os.Stderr, "egress:serve  ORANGE_EGRESS_BUNDLE=%s\n", bundlePath)
				} else {
					fmt.Fprintf(os.Stderr, "egress:serve  ORANGE_CONFIG=%s\n", absOrangeCfg)
				}
				fmt.Fprintf(os.Stderr, "egress:serve  ENVOY_DYNAMIC_MODULES_SEARCH_PATH=%s\n", modulePath)
				fmt.Fprintf(os.Stderr, "egress:serve  ORANGE_TRUSTED_CA=%s\n", trustedCA)

				ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
				defer cancel()
				return runEgressServeLocal(ctx, localServeOpts{
					configPath: absOrangeCfg,
					envoyBin:   envoyBin,
					modulePath: modulePath,
					trustedCA:  trustedCA,
					logLevel:   logLevel,
					rlsListen:  rlsListen,
					interval:   interval,
					noRLS:      noRLS,
					serverURL:  serverURL,
					bundlePath: bundlePath,
				})
			}

			// ── normal mode ───────────────────────────────────────────────────
			// TODO: accept ORANGE_CONFIG_URL and ORANGE_CLIENT_BUNDLE as
			// alternatives once the CP-based config path is implemented.
			if orangeCfg == "" {
				orangeCfg = os.Getenv("ORANGE_CONFIG")
			}
			if orangeCfg == "" {
				return fmt.Errorf("--config or ORANGE_CONFIG is required (path to orange.yaml)")
			}
			absOrangeCfg, err := filepath.Abs(orangeCfg)
			if err != nil {
				return fmt.Errorf("resolve config path: %w", err)
			}

			// ── render config inline ──────────────────────────────────────────
			rendered := strings.ReplaceAll(envoyConfigTemplate, "${ORANGE_TRUSTED_CA}", trustedCA)

			// ── startup summary ───────────────────────────────────────────────
			fmt.Fprintf(os.Stderr, "egress:serve  envoy=%s\n", envoyBin)
			fmt.Fprintf(os.Stderr, "egress:serve  ORANGE_CONFIG=%s\n", absOrangeCfg)
			fmt.Fprintf(os.Stderr, "egress:serve  ENVOY_DYNAMIC_MODULES_SEARCH_PATH=%s\n", modulePath)
			fmt.Fprintf(os.Stderr, "egress:serve  ORANGE_TRUSTED_CA=%s\n", trustedCA)
			fmt.Fprintln(os.Stderr)

			// ── launch envoy ──────────────────────────────────────────────────
			// Use cmd.Start + manual signal forwarding (not exec.CommandContext)
			// so SIGTERM/SIGINT are forwarded to envoy for a graceful drain
			// rather than being hard-killed by the Go runtime.
			envoyProc := exec.Command(envoyBin, "--config-yaml", rendered, "--log-level", logLevel)
			envoyProc.Env = append(os.Environ(),
				"GODEBUG=cgocheck=0",
				"ORANGE_CONFIG="+absOrangeCfg,
				"ENVOY_DYNAMIC_MODULES_SEARCH_PATH="+modulePath,
			)
			envoyProc.Stdout = os.Stdout
			envoyProc.Stderr = os.Stderr
			envoyProc.Stdin = os.Stdin

			if err := envoyProc.Start(); err != nil {
				return fmt.Errorf("start envoy: %w", err)
			}

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
			go func() {
				for sig := range sigCh {
					_ = envoyProc.Process.Signal(sig)
				}
			}()

			waitErr := envoyProc.Wait()
			signal.Stop(sigCh)
			close(sigCh)
			return waitErr
		},
	}
	cmd.Flags().StringVar(&envoyBin, "envoy-bin", "", "envoy binary (env: ENVOY_BIN, default: ~/.orange/bin/envoy then PATH)")
	cmd.Flags().StringVar(&orangeCfg, "config", "", "orange config file (env: ORANGE_CONFIG; default orange.yaml in --local standalone)")
	cmd.Flags().StringVar(&modulePath, "module-path", "", "ENVOY_DYNAMIC_MODULES_SEARCH_PATH (default: dir of orange binary)")
	cmd.Flags().StringVar(&trustedCA, "trusted-ca", "", "TLS trust bundle path (env: ORANGE_TRUSTED_CA)")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "envoy log level")
	cmd.Flags().BoolVar(&localMode, "local", false, "local mode: Envoy + redis-server + RLS + REPL")
	cmd.Flags().BoolVar(&noRLS, "no-rls", false, "skip redis-server and RLS in --local mode")
	cmd.Flags().StringVar(&rlsListen, "rls-listen", ":8081", "RLS gRPC listen address (--local mode)")
	cmd.Flags().DurationVar(&interval, "interval", 30*time.Second, "RLS config poll interval (--local mode)")
	cmd.Flags().StringVar(&serverURL, "server-url", "", "orange server URL for config/secrets (env: ORANGE_SERVER_URL; --local mode, requires --bundle)")
	cmd.Flags().StringVar(&bundlePath, "bundle", "", "egress bundle dir or .tar.gz (env: ORANGE_EGRESS_BUNDLE; required when --server-url is set)")
	return cmd
}

// findBinEnvoy walks up from the current working directory looking for
// .bin/envoy — the binary that `make download-envoy` installs in the transit
// repo root. Returns "" when not found.
func findBinEnvoy() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	for d := cwd; ; d = filepath.Dir(d) {
		candidate := filepath.Join(d, ".bin", "envoy")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	return ""
}

// hasOrangeModule reports whether dir contains the orange dynamic module
// (liborange.so on Linux, liborange.dylib on macOS).
func hasOrangeModule(dir string) bool {
	for _, name := range []string{"liborange.so", "liborange.dylib"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// applyDotEnv reads a KEY=VALUE .env file and calls os.Setenv for each key
// that is not already present in the process environment. Returns the number
// of variables applied. Blank lines and # comments are ignored. Surrounding
// single or double quotes are stripped from values.
func applyDotEnv(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var n int
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k == "" {
			continue
		}
		// Strip uniform surrounding quotes.
		if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
			v = v[1 : len(v)-1]
		}
		// Never overwrite a value already set in the shell.
		if _, exists := os.LookupEnv(k); exists {
			continue
		}
		if err := os.Setenv(k, v); err != nil {
			return n, fmt.Errorf("setenv %s: %w", k, err)
		}
		n++
	}
	return n, scanner.Err()
}

// detectTrustedCA returns the OS-default TLS CA bundle path, mirroring the
// Makefile's ORANGE_TRUSTED_CA logic.
func detectTrustedCA() string {
	if runtime.GOOS == "darwin" {
		return "/etc/ssl/cert.pem"
	}
	for _, p := range []string{
		"/etc/ssl/certs/ca-certificates.crt",
		"/etc/ssl/cert.pem",
		"/etc/pki/tls/certs/ca-bundle.crt",
		"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
