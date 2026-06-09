// Package orange implements all subcommands of the orange CLI.
package server

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
	user    string
	apiKey  string
	output  string
	quiet   bool
	noColor bool
	repl    bool
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

  orange admin <resource> <verb>   management plane operations
  orange auth  <verb>              authentication
  orange server                    run the server
  orange localdata                 access embedded Postgres (dev)
  orange --repl [ws=<id>]          interactive user REPL

Run 'orange <command> --help' for command-specific help.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if gf.repl {
				rc, err := resolveRunCtx()
				if err != nil {
					return err
				}
				return runUserREPL(rc, args)
			}
			// No subcommand and --repl not set: print help.
			return cmd.Help()
		},
	}

	// Persistent flags available on every subcommand.
	pf := root.PersistentFlags()
	pf.StringVar(&gf.server, "server", "http://localhost:3000", "management plane server URL (env: ORANGE_SERVER)")
	pf.StringVar(&gf.org, "org", "", "active org slug (env: ORANGE_ORG)")
	pf.StringVar(&gf.user, "user", "", "active user within the org (env: ORANGE_USER)")
	pf.StringVar(&gf.apiKey, "api-key", "", "API key (env: ORANGE_API_KEY)")
	pf.StringVarP(&gf.output, "output", "o", "table", "output format: table | json | yaml")
	pf.BoolVarP(&gf.quiet, "quiet", "q", false, "suppress headers and decorations")
	pf.BoolVar(&gf.noColor, "no-color", false, "disable ANSI color output")

	// Local flag: start the user-facing interactive REPL.
	root.Flags().BoolVar(&gf.repl, "repl", false, "start the interactive user REPL")

	// Register subcommands.
	root.AddCommand(newServerCmd())
	root.AddCommand(newBootstrapCmd())
	root.AddCommand(newAuthCmd())
	root.AddCommand(newLocalDataCmd())
	root.AddCommand(newAdminCmd())
	root.AddCommand(newEgressProxyCmd())
	root.AddCommand(newRLSCmd())
	root.AddCommand(newTokenCmd())

	return root
}

// resolveRunCtx builds a RunCtx from the current global flags and config.
// It falls back to ~/.orange/config when --api-key / --server are not given.
func resolveRunCtx() (*RunCtx, error) {
	apiKey := gf.apiKey
	if apiKey == "" {
		apiKey = os.Getenv("ORANGE_API_KEY")
	}
	serverURL := gf.server
	if serverURL == "http://localhost:3000" {
		if v := os.Getenv("ORANGE_SERVER"); v != "" {
			serverURL = v
		}
	}

	if apiKey == "" {
		cfg, err := LoadConfig()
		if err != nil {
			return nil, err
		}
		// --org / ORANGE_ORG overrides the active org in config.
		org := gf.org
		if org == "" {
			org = os.Getenv("ORANGE_ORG")
		}
		if org != "" {
			cfg.ActiveOrg = org
		}
		// --user / ORANGE_USER overrides the active user within the resolved org.
		user := gf.user
		if user == "" {
			user = os.Getenv("ORANGE_USER")
		}
		if user != "" && cfg.ActiveOrg != "" {
			if e, ok := cfg.Orgs[cfg.ActiveOrg]; ok {
				e.ActiveUser = user
			}
		}
		orgEntry, userEntry, err := cfg.ActiveEntry()
		if err != nil {
			return nil, err
		}
		apiKey = userEntry.APIKey
		if serverURL == "http://localhost:3000" && orgEntry.Server != "" {
			serverURL = orgEntry.Server
		}
	}

	if apiKey == "" {
		return nil, fmt.Errorf("no API key — set --api-key, ORANGE_API_KEY, or run: orange auth login --org <org> --user <user>")
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
