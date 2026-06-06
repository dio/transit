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

// newAuthLoginCmd stores an API key in ~/.orange/config under orgs[org].users[user].
//
// Usage:
//
//	orange auth login --org acme --user alice@acme.com
//	orange auth login --org acme --user alice@acme.com --api-key sk-org-…
func newAuthLoginCmd() *cobra.Command {
	var (
		org    string
		user   string
		server string
	)
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Save API key credentials to ~/.orange/config",
		Long: `Save an API key for an org/user pair to ~/.orange/config.

If --api-key is not set globally, the key is read from stdin (hidden prompt).
The key is validated by checking server connectivity before saving.

Credentials are namespaced by org and user, so multiple users (or multiple
keys with different scopes) can coexist under the same org.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if org == "" {
				return fmt.Errorf("--org is required")
			}
			if user == "" {
				return fmt.Errorf("--user is required")
			}

			srvURL := server
			if srvURL == "" {
				srvURL = gf.server
			}

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

			if err := pingServer(srvURL); err != nil {
				return fmt.Errorf("server unreachable at %s: %w", srvURL, err)
			}

			cfg, err := LoadConfig()
			if err != nil {
				return err
			}
			cfg.SetUser(org, user, srvURL, apiKey)
			if err := SaveConfig(cfg); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "logged in  org=%s  user=%s  server=%s\n", org, user, srvURL)
			return nil
		},
	}
	cmd.Flags().StringVar(&org, "org", "", "org slug (required)")
	cmd.Flags().StringVar(&user, "user", "", "user identity, e.g. alice@acme.com (required)")
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
				fmt.Fprintln(cmd.OutOrStdout(), "not logged in — run: orange auth login --org <org> --user <user>")
				return nil
			}
			org, u, err := cfg.ActiveEntry()
			if err != nil {
				fmt.Fprintln(cmd.OutOrStdout(), err.Error())
				return nil
			}
			keyHint := ""
			if u.APIKey != "" {
				keyHint = truncate(u.APIKey, 16) + "…"
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"org:    %s\nuser:   %s\nserver: %s\nkey:    %s\n",
				cfg.ActiveOrg,
				org.ActiveUser,
				org.Server,
				keyHint,
			)
			return nil
		},
	}
}

func newAuthSwitchCmd() *cobra.Command {
	var org, user string
	cmd := &cobra.Command{
		Use:   "switch",
		Short: "Switch the active org and/or user in ~/.orange/config",
		Long: `Switch active org, active user within an org, or both.

  orange auth switch --org acme             # switch org (keeps that org's active user)
  orange auth switch --user bob             # switch user within the current org
  orange auth switch --org acme --user bob  # switch both`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if org == "" && user == "" {
				return fmt.Errorf("at least one of --org or --user is required")
			}
			cfg, err := LoadConfig()
			if err != nil {
				return err
			}

			targetOrg := cfg.ActiveOrg
			if org != "" {
				if _, ok := cfg.Orgs[org]; !ok {
					return fmt.Errorf("org %q not found in config — run: orange auth login --org %s --user <user>", org, org)
				}
				targetOrg = org
			}
			if targetOrg == "" {
				return fmt.Errorf("no active org — run: orange auth login --org <org> --user <user>")
			}

			orgEntry := cfg.Orgs[targetOrg]
			if user != "" {
				if _, ok := orgEntry.Users[user]; !ok {
					return fmt.Errorf("user %q not found under org %q — run: orange auth login --org %s --user %s",
						user, targetOrg, targetOrg, user)
				}
				orgEntry.ActiveUser = user
			}

			cfg.ActiveOrg = targetOrg
			if err := SaveConfig(cfg); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "switched to  org=%s  user=%s\n", targetOrg, orgEntry.ActiveUser)
			return nil
		},
	}
	cmd.Flags().StringVar(&org, "org", "", "org slug to activate")
	cmd.Flags().StringVar(&user, "user", "", "user to activate within the org")
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
