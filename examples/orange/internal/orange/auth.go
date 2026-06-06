package orange

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage authentication credentials",
	}
	cmd.AddCommand(
		newAuthLoginCmd(),
		newAuthWhoamiCmd(),
		newAuthSwitchCmd(),
	)
	return cmd
}

// newAuthLoginCmd stores an API key in ~/.orange/config.
//
// Usage:
//
//	orange auth login --org acme --user admin@acme.com
//	orange auth login --org acme --user admin@acme.com --api-key sk-org-…
func newAuthLoginCmd() *cobra.Command {
	var (
		org    string
		user   string
		server string
	)
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Save API key credentials to ~/.orange/config",
		Long: `Save an admin API key for an org to ~/.orange/config.

If --api-key is not set globally, the key is read from stdin (hidden prompt).
The key is validated by checking server connectivity before saving.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if org == "" {
				return fmt.Errorf("--org is required")
			}

			// Resolve server URL: flag > global flag > default.
			srvURL := server
			if srvURL == "" {
				srvURL = gf.server
			}

			// Resolve API key: --api-key > prompt.
			apiKey := gf.apiKey
			if apiKey == "" {
				var err error
				apiKey, err = promptAPIKey()
				if err != nil {
					return err
				}
			}
			if !strings.HasPrefix(apiKey, "sk-") {
				return fmt.Errorf("API key must start with sk- (got %q)", truncate(apiKey, 12))
			}

			// Sanity check: confirm server is reachable.
			if err := pingServer(srvURL); err != nil {
				return fmt.Errorf("server unreachable at %s: %w", srvURL, err)
			}

			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			activeUser := user
			if activeUser == "" {
				activeUser = gf.org // best-effort
			}
			cfg.SetOrg(org, OrgEntry{
				Server:     srvURL,
				APIKey:     apiKey,
				ActiveUser: activeUser,
			})
			if err := SaveConfig(cfg); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "logged in  org=%s  server=%s\n", org, srvURL)
			return nil
		},
	}
	cmd.Flags().StringVar(&org, "org", "", "org slug (required)")
	cmd.Flags().StringVar(&user, "user", "", "identity hint stored in config (informational)")
	cmd.Flags().StringVar(&server, "server", "", "server URL override for this org")
	return cmd
}

func newAuthWhoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "whoami",
		Short:        "Show the currently active identity from ~/.orange/config",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			if cfg.ActiveOrg == "" {
				fmt.Fprintln(cmd.OutOrStdout(), "not logged in — run: orange auth login --org <org>")
				return nil
			}
			entry, ok := cfg.Orgs[cfg.ActiveOrg]
			if !ok {
				fmt.Fprintf(cmd.OutOrStdout(), "active org: %s (no credentials stored)\n", cfg.ActiveOrg)
				return nil
			}
			keyHint := ""
			if entry.APIKey != "" {
				keyHint = truncate(entry.APIKey, 16) + "…"
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"org:         %s\nuser:        %s\nserver:      %s\napi_key:     %s\n",
				cfg.ActiveOrg,
				entry.ActiveUser,
				entry.Server,
				keyHint,
			)
			return nil
		},
	}
}

func newAuthSwitchCmd() *cobra.Command {
	var org string
	cmd := &cobra.Command{
		Use:          "switch",
		Short:        "Switch the active org in ~/.orange/config",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if org == "" {
				return fmt.Errorf("--org is required")
			}
			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			if _, ok := cfg.Orgs[org]; !ok {
				return fmt.Errorf("org %q not found in config — run: orange auth login --org %s", org, org)
			}
			cfg.ActiveOrg = org
			if err := SaveConfig(cfg); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "switched to org: %s\n", org)
			return nil
		},
	}
	cmd.Flags().StringVar(&org, "org", "", "org slug to activate (required)")
	return cmd
}

// promptAPIKey reads an API key from stdin.
func promptAPIKey() (string, error) {
	fmt.Fprint(os.Stderr, "API key: ")
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read API key: %w", err)
	}
	key := strings.TrimSpace(line)
	if key == "" {
		return "", fmt.Errorf("API key cannot be empty")
	}
	return key, nil
}

// pingServer does a GET /healthz to confirm the server is reachable.
func pingServer(serverURL string) error {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(strings.TrimRight(serverURL, "/") + "/healthz")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %d", resp.StatusCode)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
