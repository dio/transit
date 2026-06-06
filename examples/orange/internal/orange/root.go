// Package orange implements all subcommands of the orange CLI.
package orange

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"github.com/dio/transit/examples/orange/internal/vtprotocodec"
)

// globalFlags holds values bound to the persistent root flags.
type globalFlags struct {
	server  string
	org     string
	apiKey  string
	output  string
	quiet   bool
	noColor bool
}

var gf globalFlags

// RunCtx carries resolved runtime dependencies for subcommands.
type RunCtx struct {
	Printer     *Printer
	ConnectOpts []connect.ClientOption
	HTTPClient  *http.Client
	ServerURL   string
	APIKey      string
}

// NewCommand builds and returns the root cobra.Command.
func NewCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "orange",
		Short: "orange management plane CLI",
		Long: `orange — unified CLI for the orange management plane.

Run 'orange <command> --help' for command-specific help.
Run 'orange repl' (or bare 'orange') for the interactive shell.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// No subcommand: print help (REPL will replace this in a future step).
			return cmd.Help()
		},
	}

	// Persistent flags available on every subcommand.
	pf := root.PersistentFlags()
	pf.StringVar(&gf.server, "server", envOr("ORANGE_SERVER", "http://localhost:8080"), "management plane server URL (env: ORANGE_SERVER)")
	pf.StringVar(&gf.org, "org", envOr("ORANGE_ORG", ""), "org slug override (env: ORANGE_ORG)")
	pf.StringVar(&gf.apiKey, "api-key", envOr("ORANGE_API_KEY", ""), "admin API key (env: ORANGE_API_KEY)")
	pf.StringVarP(&gf.output, "output", "o", "table", "output format: table | json | yaml")
	pf.BoolVarP(&gf.quiet, "quiet", "q", false, "suppress headers and decorations")
	pf.BoolVar(&gf.noColor, "no-color", false, "disable ANSI color output")

	// Register subcommands.
	root.AddCommand(newServerCmd())
	root.AddCommand(newAuthCmd())
	root.AddCommand(newLocalDataCmd())

	return root
}

// resolveRunCtx builds a RunCtx from the current global flags and config.
// It falls back to ~/.orange/config when --api-key / --server are not given.
func resolveRunCtx() (*RunCtx, error) {
	apiKey := gf.apiKey
	serverURL := gf.server

	if apiKey == "" {
		cfg, err := LoadConfig()
		if err != nil {
			return nil, err
		}
		entry, err := cfg.ActiveEntry()
		if err != nil {
			return nil, err
		}
		apiKey = entry.APIKey
		if serverURL == "http://localhost:8080" && entry.Server != "" {
			serverURL = entry.Server
		}
	}

	if apiKey == "" {
		return nil, fmt.Errorf("no API key — set --api-key, ORANGE_API_KEY, or run: orange auth login")
	}

	pr := &Printer{
		Format:  Format(gf.output),
		Quiet:   gf.quiet,
		NoColor: gf.noColor,
		Out:     os.Stdout,
	}

	httpClient := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &bearerTransport{key: apiKey, base: http.DefaultTransport},
	}

	return &RunCtx{
		Printer:     pr,
		ConnectOpts: []connect.ClientOption{connect.WithCodec(vtprotocodec.Codec{})},
		HTTPClient:  httpClient,
		ServerURL:   serverURL,
		APIKey:      apiKey,
	}, nil
}

// bearerTransport injects Authorization: Bearer on every request.
type bearerTransport struct {
	key  string
	base http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.key)
	return t.base.RoundTrip(clone)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
