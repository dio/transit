package server

// provider.go — "orange admin provider" commands.
//
// provider set <name> is a one-shot helper that:
//   1. Stores the API key in the orange secret service under ws/<ws-id>/api-keys/<name>.
//   2. Patches the local orange.yaml to add/update llm.providers.<name> with
//      secret_ref: orange://<ws-id>/api-keys/<name>.
//   3. Optionally publishes the patched YAML as a new config snapshot (--publish).

import (
	"context"
	"fmt"
	"os"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	configadminv1 "github.com/dio/transit/examples/orange/api/orange/config/admin/v1"
	configadminv1connect "github.com/dio/transit/examples/orange/api/orange/config/admin/v1/adminv1connect"
	secretadminv1 "github.com/dio/transit/examples/orange/api/orange/secret/admin/v1"
	secretadminv1connect "github.com/dio/transit/examples/orange/api/orange/secret/admin/v1/adminv1connect"
)

func newProviderCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "provider",
		Short: "Manage LLM providers",
	}
	cmd.AddCommand(newProviderSetCmd())
	return cmd
}

func newProviderSetCmd() *cobra.Command {
	var (
		kind        string
		endpoint    string
		authType    string
		workspaceID string
		value       string
		configFile  string
		publish     bool
	)
	cmd := &cobra.Command{
		Use:   "set <name>",
		Short: "Store a provider API key and patch orange.yaml",
		Long: `Store a provider's API key in the orange secret service and update
the local orange.yaml to reference it via orange://.

Three steps in one call:
  1. Store --value under ws/<ws>/api-keys/<name> in the secret service.
  2. Patch --config (orange.yaml) to add/update llm.providers.<name>.
  3. Publish the patched YAML as a new snapshot for the workspace (--publish).

Kind shortcuts (endpoint and auth-type are inferred when omitted):
  anthropic  → https://api.anthropic.com, auth: anthropic
  openai     → https://api.openai.com,    auth: bearer
  gemini     → https://generativelanguage.googleapis.com, auth: gemini

Examples:
  orange admin provider set anthropic \
    --kind=anthropic --ws=<ws-id> \
    --value=sk-ant-... \
    --config=orange.yaml --publish

  echo "sk-ant-..." | orange admin provider set anthropic \
    --kind=anthropic --ws=<ws-id> --config=orange.yaml --publish`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]

			if workspaceID == "" {
				workspaceID = os.Getenv("ORANGE_WS_ID")
			}
			if workspaceID == "" {
				return fmt.Errorf("--ws / ORANGE_WS_ID is required")
			}
			if kind == "" {
				return fmt.Errorf("--kind is required (anthropic, openai, gemini, or a custom kind)")
			}

			// Infer defaults from well-known kinds.
			switch kind {
			case "anthropic":
				if endpoint == "" {
					endpoint = "https://api.anthropic.com"
				}
				if authType == "" {
					authType = "anthropic"
				}
			case "openai":
				if endpoint == "" {
					endpoint = "https://api.openai.com"
				}
				if authType == "" {
					authType = "bearer"
				}
			case "gemini":
				if endpoint == "" {
					endpoint = "https://generativelanguage.googleapis.com"
				}
				if authType == "" {
					authType = "gemini"
				}
			default:
				if endpoint == "" {
					return fmt.Errorf("--endpoint is required for kind %q", kind)
				}
				if authType == "" {
					authType = "bearer"
				}
			}

			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}

			realm := "ws/" + workspaceID + "/api-keys"
			secretRef := "orange://" + workspaceID + "/api-keys/" + name

			// ── Step 1: store secret ──────────────────────────────────────────
			material, err := readSecretValue(value)
			if err != nil {
				return fmt.Errorf("read secret value: %w", err)
			}
			if len(material) > 0 {
				sc := secretadminv1connect.NewSecretAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
				if _, err := sc.CreateVersion(context.Background(), connect.NewRequest(&secretadminv1.CreateVersionRequest{
					Realm:    realm,
					SecretId: name,
					Material: material,
					Enable:   true,
				})); err != nil {
					return fmt.Errorf("store secret: %w", err)
				}
				fmt.Fprintf(os.Stderr, "provider:set  stored  realm=%s  name=%s\n", realm, name)
			} else {
				fmt.Fprintf(os.Stderr, "provider:set  no --value given; skipping secret store\n")
			}
			fmt.Fprintf(os.Stderr, "provider:set  secret_ref=%s\n", secretRef)

			// ── Step 2: patch config file ─────────────────────────────────────
			var patchedYAML []byte
			if configFile == "" {
				// No file: print the fragment and exit.
				printProviderFragment(name, kind, endpoint, authType, secretRef)
				return nil
			}

			existing, err := os.ReadFile(configFile)
			if err != nil {
				return fmt.Errorf("read %s: %w", configFile, err)
			}
			patchedYAML, err = patchProviderInYAML(existing, name, kind, endpoint, authType, secretRef)
			if err != nil {
				return fmt.Errorf("patch %s: %w", configFile, err)
			}
			if err := os.WriteFile(configFile, patchedYAML, 0o644); err != nil {
				return fmt.Errorf("write %s: %w", configFile, err)
			}
			fmt.Fprintf(os.Stderr, "provider:set  patched  file=%s\n", configFile)

			// ── Step 3: publish ───────────────────────────────────────────────
			if publish {
				cc := configadminv1connect.NewConfigAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
				if _, err := cc.PublishSnapshot(context.Background(), connect.NewRequest(&configadminv1.PublishSnapshotRequest{
					WorkspaceId: workspaceID,
					YamlConfig:  string(patchedYAML),
					PublishedBy: "orange-cli",
				})); err != nil {
					return fmt.Errorf("publish snapshot: %w", err)
				}
				fmt.Fprintf(os.Stderr, "provider:set  published snapshot  ws=%s\n", workspaceID)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&kind, "kind", "", "provider kind: anthropic, openai, gemini, or custom")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "upstream base URL (inferred from --kind for well-known providers)")
	cmd.Flags().StringVar(&authType, "auth-type", "", "auth type: anthropic, bearer, gemini, gcp, aws (inferred from --kind)")
	cmd.Flags().StringVar(&workspaceID, "ws", "", "workspace ID (env: ORANGE_WS_ID)")
	cmd.Flags().StringVar(&value, "value", "", "API key material; use '-' to read from stdin")
	cmd.Flags().StringVar(&configFile, "config", "", "orange.yaml to patch in place (when omitted, prints fragment to stdout)")
	cmd.Flags().BoolVar(&publish, "publish", false, "publish a new config snapshot after patching --config")
	return cmd
}

// patchProviderInYAML parses data as YAML, adds or replaces llm.providers[name],
// and returns the re-serialised bytes. Unknown fields at every level are preserved.
func patchProviderInYAML(data []byte, name, kind, endpoint, authType, secretRef string) ([]byte, error) {
	var root map[string]any
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse YAML: %w", err)
	}
	if root == nil {
		root = make(map[string]any)
	}

	llm, _ := root["llm"].(map[string]any)
	if llm == nil {
		llm = make(map[string]any)
		root["llm"] = llm
	}
	providers, _ := llm["providers"].(map[string]any)
	if providers == nil {
		providers = make(map[string]any)
		llm["providers"] = providers
	}

	entry := map[string]any{
		"kind":     kind,
		"endpoint": endpoint,
		"auth": map[string]any{
			"type":       authType,
			"secret_ref": secretRef,
		},
	}
	if kind == "anthropic" {
		entry["extra"] = map[string]any{"anthropic_version": "2023-06-01"}
	}
	providers[name] = entry

	return yaml.Marshal(root)
}

// printProviderFragment prints the YAML snippet to add to orange.yaml manually.
func printProviderFragment(name, kind, endpoint, authType, secretRef string) {
	entry := map[string]any{
		"kind":     kind,
		"endpoint": endpoint,
		"auth": map[string]any{
			"type":       authType,
			"secret_ref": secretRef,
		},
	}
	if kind == "anthropic" {
		entry["extra"] = map[string]any{"anthropic_version": "2023-06-01"}
	}
	fragment, _ := yaml.Marshal(map[string]any{
		"llm": map[string]any{
			"providers": map[string]any{name: entry},
		},
	})
	fmt.Printf("# Add to orange.yaml under llm.providers:\n%s", fragment)
}
