package server

// egress_serve.go — "orange egress serve" launches the Envoy proxy for a local
// egress instance. It renders the embedded envoy.tmpl.yaml (substituting
// ${ORANGE_TRUSTED_CA}) and passes the result inline via Envoy's --config-yaml
// flag, so no config file is written to disk.
//
// Signal handling: SIGINT/SIGTERM are forwarded to the envoy process so it can
// drain connections gracefully rather than being hard-killed.

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

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
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the Envoy proxy for this egress instance",
		Long: `Serve renders the embedded Envoy config template and launches Envoy with
the orange dynamic module — equivalent to "make run" but self-contained.

The rendered config is passed inline via --config-yaml; no file is written.

Environment variable mapping:
  --envoy-bin    ENVOY_BIN                          default: ~/.orange/bin/envoy, then PATH
  --config       ORANGE_CONFIG                      required for now; later: ORANGE_CONFIG_URL / ORANGE_CLIENT_BUNDLE
  --module-path  ENVOY_DYNAMIC_MODULES_SEARCH_PATH  default: directory of the orange binary
  --trusted-ca   ORANGE_TRUSTED_CA                  default: OS trust bundle

For development with "go run" the orange binary resolves to a temp path, so
set --module-path (or ENVOY_DYNAMIC_MODULES_SEARCH_PATH) to the directory
containing liborange.so (typically examples/orange/).`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// ── envoy binary ──────────────────────────────────────────────────
			if envoyBin == "" {
				envoyBin = os.Getenv("ENVOY_BIN")
			}
			if envoyBin == "" {
				home, _ := os.UserHomeDir()
				envoyBin = filepath.Join(home, ".orange", "bin", "envoy")
			}
			if _, err := os.Stat(envoyBin); err != nil {
				found, lerr := exec.LookPath("envoy")
				if lerr != nil {
					return fmt.Errorf("envoy not found at %s and not in PATH; install envoy or set --envoy-bin", envoyBin)
				}
				envoyBin = found
			}

			// ── orange config ─────────────────────────────────────────────────
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

			// ── trusted CA ────────────────────────────────────────────────────
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

			// ── render config inline ──────────────────────────────────────────
			rendered := strings.ReplaceAll(envoyConfigTemplate, "${ORANGE_TRUSTED_CA}", trustedCA)

			// ── dynamic module search path ────────────────────────────────────
			if modulePath == "" {
				modulePath = os.Getenv("ENVOY_DYNAMIC_MODULES_SEARCH_PATH")
			}
			if modulePath == "" {
				if self, err := os.Executable(); err == nil {
					modulePath = filepath.Dir(self)
				}
			}
			if modulePath == "" {
				modulePath = "."
			}

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
	cmd.Flags().StringVar(&orangeCfg, "config", "", "orange config file (env: ORANGE_CONFIG)")
	cmd.Flags().StringVar(&modulePath, "module-path", "", "ENVOY_DYNAMIC_MODULES_SEARCH_PATH (default: dir of orange binary)")
	cmd.Flags().StringVar(&trustedCA, "trusted-ca", "", "TLS trust bundle path (env: ORANGE_TRUSTED_CA)")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "envoy log level")
	return cmd
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
