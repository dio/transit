package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"buf.build/go/protovalidate"
	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	adminv1 "github.com/dio/transit/examples/orange/api/orange/config/admin/v1"
	configconnect "github.com/dio/transit/examples/orange/api/orange/config/admin/v1/adminv1connect"
	keyentryv1 "github.com/dio/transit/examples/orange/api/orange/keyentry/admin/v1"
	keyentryconnect "github.com/dio/transit/examples/orange/api/orange/keyentry/admin/v1/adminv1connect"
	routingv1 "github.com/dio/transit/examples/orange/api/orange/routing/v1"
)

// ── user-friendly YAML/JSON schema ───────────────────────────────────────────
//
// Simple form:
//
//	rules:
//	  - model: "claude-3-5-sonnet"
//	    target: anthropic            # provider name from workspace llm.providers
//
//	  - model: "gpt-4o"
//	    target: openai
//
// Chain (ordered fallback) — by provider:
//
//	rules:
//	  - model: "claude-3-5-sonnet"
//	    chain:
//	      - target: anthropic
//	      - target: anthropic-backup  # tried if anthropic returns 5xx
//
// Chain fallback to a different model (e.g. cheaper model on failure):
//
//	rules:
//	  - model: "claude-3-5-sonnet"
//	    chain:
//	      - target: anthropic
//	        backend_model: claude-3-5-sonnet-20241022
//	      - target: anthropic
//	        backend_model: claude-3-5-haiku-20241022  # cheaper fallback on failure
//
// Split (weighted A/B) — split by provider:
//
//	rules:
//	  - model: "claude-3-5-sonnet"
//	    split:
//	      - weight: 80
//	        target: anthropic
//	      - weight: 20
//	        target: anthropic-eu
//
// Split by model (same provider, different backend model names):
//
//	rules:
//	  - model: "claude-3-5-sonnet"
//	    split:
//	      - weight: 70
//	        target: anthropic
//	        backend_model: claude-3-5-sonnet-20241022
//	      - weight: 30
//	        target: anthropic
//	        backend_model: claude-3-5-haiku-20241022
//
// backend_model is optional on any target item (simple, chain, or split);
// when set it overrides the model name sent to the upstream. Useful when a
// provider uses versioned names or you want to A/B between two models:

type routingOverrideDoc struct {
	Rules []routingRuleDoc `yaml:"rules" json:"rules"`
}

type routingRuleDoc struct {
	Model        string          `yaml:"model" json:"model"`
	Target       string          `yaml:"target" json:"target"`
	BackendModel string          `yaml:"backend_model" json:"backend_model"`
	Chain        []chainItemDoc  `yaml:"chain" json:"chain"`
	Split        []splitItemDoc  `yaml:"split" json:"split"`
}

type chainItemDoc struct {
	Target       string `yaml:"target" json:"target"`
	BackendModel string `yaml:"backend_model" json:"backend_model"`
}

type splitItemDoc struct {
	Weight       int32  `yaml:"weight" json:"weight"`
	Target       string `yaml:"target" json:"target"`
	BackendModel string `yaml:"backend_model" json:"backend_model"`
}

func parseRoutingDoc(data []byte, asJSON bool) ([]*routingv1.RoutingOverride, error) {
	var doc routingOverrideDoc
	if asJSON {
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("parse JSON: %w", err)
		}
	} else {
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("parse YAML: %w", err)
		}
	}
	return routingDocToProto(doc)
}

func routingDocToProto(doc routingOverrideDoc) ([]*routingv1.RoutingOverride, error) {
	overrides := make([]*routingv1.RoutingOverride, 0, len(doc.Rules))
	for i, r := range doc.Rules {
		if r.Model == "" {
			return nil, fmt.Errorf("rule[%d]: model is required", i)
		}
		node, err := routingRuleToNode(r, i)
		if err != nil {
			return nil, err
		}
		overrides = append(overrides, &routingv1.RoutingOverride{
			ModelId: r.Model,
			Node:    node,
		})
	}
	return overrides, nil
}

func routingRuleToNode(r routingRuleDoc, idx int) (*routingv1.RoutingNode, error) {
	set := 0
	if r.Target != "" {
		set++
	}
	if len(r.Chain) > 0 {
		set++
	}
	if len(r.Split) > 0 {
		set++
	}
	if set == 0 {
		return nil, fmt.Errorf("rule[%d] %q: one of target, chain, or split is required", idx, r.Model)
	}
	if set > 1 {
		return nil, fmt.Errorf("rule[%d] %q: only one of target, chain, or split may be set", idx, r.Model)
	}

	if r.Target != "" {
		return &routingv1.RoutingNode{
			Kind: &routingv1.RoutingNode_Target{
				Target: &routingv1.RoutingTarget{
					Provider: r.Target,
					Model:    r.BackendModel,
				},
			},
		}, nil
	}

	if len(r.Chain) > 0 {
		children := make([]*routingv1.RoutingNode, len(r.Chain))
		for j, c := range r.Chain {
			if c.Target == "" {
				return nil, fmt.Errorf("rule[%d] chain[%d]: target is required", idx, j)
			}
			children[j] = &routingv1.RoutingNode{
				Kind: &routingv1.RoutingNode_Target{
					Target: &routingv1.RoutingTarget{Provider: c.Target, Model: c.BackendModel},
				},
			}
		}
		return &routingv1.RoutingNode{
			Kind: &routingv1.RoutingNode_Chain{
				Chain: &routingv1.ChainConfig{Children: children},
			},
		}, nil
	}

	// split
	children := make([]*routingv1.SplitChild, len(r.Split))
	for j, c := range r.Split {
		if c.Target == "" {
			return nil, fmt.Errorf("rule[%d] split[%d]: target is required", idx, j)
		}
		children[j] = &routingv1.SplitChild{
			Weight: c.Weight,
			Node: &routingv1.RoutingNode{
				Kind: &routingv1.RoutingNode_Target{
					Target: &routingv1.RoutingTarget{Provider: c.Target, Model: c.BackendModel},
				},
			},
		}
	}
	return &routingv1.RoutingNode{
		Kind: &routingv1.RoutingNode_Split{
			Split: &routingv1.SplitConfig{Children: children},
		},
	}, nil
}

// loadRoutingOverrides reads from a file path or inline JSON string.
// File format is detected by extension (.json → JSON, everything else → YAML).
func loadRoutingOverrides(filePath, inlineJSON string) ([]*routingv1.RoutingOverride, error) {
	if inlineJSON != "" {
		return parseRoutingDoc([]byte(inlineJSON), true)
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filePath, err)
	}
	return parseRoutingDoc(data, strings.HasSuffix(filePath, ".json"))
}

func newKeyEntryRoutingModelsCmd() *cobra.Command {
	var wsID string
	cmd := &cobra.Command{
		Use:   "models <provider-name>",
		Short: "List backend model names for a provider (valid backend_model values)",
		Long: `Fetches the active config snapshot and lists all models belonging to the
given provider, showing the client-facing model ID alongside the backend model
name (the value to use as 'backend_model' in a routing rule).

If backend_model is omitted in a rule, the client-facing model ID is forwarded
to the upstream as-is.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			if wsID == "" {
				wsID = os.Getenv("ORANGE_WS_ID")
			}
			if wsID == "" {
				return fmt.Errorf("--ws is required (or set ORANGE_WS_ID)")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			yamlPayload, err := latestSnapshotYAML(context.Background(), rc, wsID)
			if err != nil {
				return err
			}
			entries, err := parseBackendModels(yamlPayload, args[0])
			if err != nil {
				return err
			}
			printBackendModels(rc.Printer, args[0], entries)
			return nil
		},
	}
	cmd.Flags().StringVar(&wsID, "ws", "", "workspace ID (env: ORANGE_WS_ID)")
	return cmd
}

// validateRoutingRequest runs protovalidate on the UpdateKeyRequest and returns
// a human-readable error listing every violation on a separate line.
func validateRoutingRequest(req *keyentryv1.UpdateKeyRequest) error {
	if err := protovalidate.Validate(req); err != nil {
		return fmt.Errorf("request validation failed:\n%w", err)
	}
	return nil
}

// ── cobra: keyentry-routing ───────────────────────────────────────────────────

func newKeyEntryRoutingCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keyentry-routing",
		Short: "Manage routing overrides for a token slot",
		Long: `Routing overrides let a key entry redirect LLM traffic to a specific
provider instead of the workspace default.

Each rule maps one client-facing model ID to a routing node (target, chain, or split).
Rules are evaluated in order; the first match wins.

─── Typical authoring flow ──────────────────────────────────────────────────

  Step 1 — discover available providers (valid 'target' values):

    orange admin keyentry-routing targets --ws=<ws-id>

    Output:
      TARGET (provider name)
      anthropic
      openai
      anthropic-eu

  Step 2 — list backend model names for a provider:

    orange admin keyentry-routing models anthropic --ws=<ws-id>

    Output:
      CLIENT MODEL ID           BACKEND MODEL NAME
      claude-3-5-sonnet         claude-3-5-sonnet-20241022
      claude-3-haiku            claude-3-haiku-20241022

  Step 3 — attach provider credentials to the key entry (if not already):

    orange admin keyentry-secret create <key-entry-id> --target=anthropic
    # prompts for the secret value

  Step 4 — write routing.yaml:

    rules:
      # Simple: route one model to one provider
      - model: "claude-3-5-sonnet"
        target: anthropic

      # With explicit backend model name (useful for versioned names)
      - model: "claude-3-5-sonnet"
        target: anthropic
        backend_model: claude-3-5-sonnet-20241022

      # Chain: try first, fall back on 5xx
      - model: "gpt-4o"
        chain:
          - target: openai
          - target: openai-backup        # fallback by provider

      # Chain: fall back to a cheaper model on failure
      - model: "claude-3-opus"
        chain:
          - target: anthropic
            backend_model: claude-3-opus-20240229
          - target: anthropic
            backend_model: claude-3-haiku-20241022  # cheaper fallback

      # Split: weighted A/B between providers
      - model: "claude-3-haiku"
        split:
          - weight: 80
            target: anthropic
          - weight: 20
            target: anthropic-eu

      # Split: A/B between backend models on the same provider
      - model: "claude-3-5-sonnet"
        split:
          - weight: 70
            target: anthropic
            backend_model: claude-3-5-sonnet-20241022
          - weight: 30
            target: anthropic
            backend_model: claude-3-5-haiku-20241022

  Step 5 — validate the file locally before applying:

    orange admin keyentry-routing validate --file=routing.yaml

    Parses and prints the rules that would be applied, no server call needed.
    Add --ws=<ws-id> to also cross-check each target against workspace providers.

  Step 6 — apply:

    orange admin keyentry-routing set <key-entry-id> --file=routing.yaml

  Step 7 — verify what was stored:

    orange admin keyentry-routing get <key-entry-id>

  Step 8 — revert to workspace default (remove overrides):

    orange admin keyentry-routing delete <key-entry-id>

─────────────────────────────────────────────────────────────────────────────

The 'target' value matches an llm.providers key in the workspace config.
Run 'keyentry-routing targets' to list available providers.
The optional 'backend_model' overrides the model name sent to the upstream.`,
	}
	cmd.AddCommand(
		newKeyEntryRoutingGetCmd(),
		newKeyEntryRoutingSetCmd(),
		newKeyEntryRoutingDeleteCmd(),
		newKeyEntryRoutingValidateCmd(),
		newKeyEntryRoutingTargetsCmd(),
		newKeyEntryRoutingModelsCmd(),
	)
	return cmd
}

func newKeyEntryRoutingGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "get <key-entry-id>",
		Short:        "Show routing overrides for a token slot",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := keyentryconnect.NewKeyEntryAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.GetKey(context.Background(), connect.NewRequest(&keyentryv1.GetKeyRequest{KeyEntryId: args[0]}))
			if err != nil {
				return err
			}
			return printRoutingOverrides(rc.Printer, resp.Msg.GetKey())
		},
	}
}

func newKeyEntryRoutingSetCmd() *cobra.Command {
	var filePath, inlineJSON string
	cmd := &cobra.Command{
		Use:          "set <key-entry-id>",
		Short:        "Replace routing overrides from a YAML file or inline JSON",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			if filePath == "" && inlineJSON == "" {
				return fmt.Errorf("provide --file=<path.yaml> or --json='{\"rules\":[...]}'")
			}
			overrides, err := loadRoutingOverrides(filePath, inlineJSON)
			if err != nil {
				return err
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := keyentryconnect.NewKeyEntryAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			// Read existing key to preserve its description (UpdateKey is a full replacement).
			existing, err := client.GetKey(context.Background(), connect.NewRequest(&keyentryv1.GetKeyRequest{KeyEntryId: args[0]}))
			if err != nil {
				return err
			}
			req := &keyentryv1.UpdateKeyRequest{
				KeyEntryId:      args[0],
				Description:     existing.Msg.GetKey().Description,
				RoutingOverrides: overrides,
			}
			if err := validateRoutingRequest(req); err != nil {
				return err
			}
			resp, err := client.UpdateKey(context.Background(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			return printRoutingOverrides(rc.Printer, resp.Msg.GetKey())
		},
	}
	cmd.Flags().StringVar(&filePath, "file", "", "path to YAML (or .json) file with routing rules")
	cmd.Flags().StringVar(&inlineJSON, "json", "", `inline JSON: '{"rules":[{"model":"...","target":"..."}]}'`)
	return cmd
}

func newKeyEntryRoutingDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "delete <key-entry-id>",
		Short:        "Remove all routing overrides from a token slot (reverts to workspace default)",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := keyentryconnect.NewKeyEntryAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			existing, err := client.GetKey(context.Background(), connect.NewRequest(&keyentryv1.GetKeyRequest{KeyEntryId: args[0]}))
			if err != nil {
				return err
			}
			req := &keyentryv1.UpdateKeyRequest{
				KeyEntryId:      args[0],
				Description:     existing.Msg.GetKey().Description,
				RoutingOverrides: []*routingv1.RoutingOverride{},
			}
			if err := validateRoutingRequest(req); err != nil {
				return err
			}
			resp, err := client.UpdateKey(context.Background(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			return printRoutingOverrides(rc.Printer, resp.Msg.GetKey())
		},
	}
}

func newKeyEntryRoutingTargetsCmd() *cobra.Command {
	var wsID string
	cmd := &cobra.Command{
		Use:   "targets",
		Short: "List LLM providers available as routing targets in a workspace",
		Long: `Fetches the active config snapshot for the workspace and lists the
llm.providers keys — these are the valid 'target' values for routing rules.

To add credentials for a provider to a specific key entry:
  orange admin keyentry-secret create <key-entry-id> --target=<provider-name>`,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if wsID == "" {
				wsID = os.Getenv("ORANGE_WS_ID")
			}
			if wsID == "" {
				return fmt.Errorf("--ws is required (or set ORANGE_WS_ID)")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			providers, err := workspaceProviders(context.Background(), rc, wsID)
			if err != nil {
				return err
			}
			printProviderTargets(rc.Printer, providers)
			return nil
		},
	}
	cmd.Flags().StringVar(&wsID, "ws", "", "workspace ID (env: ORANGE_WS_ID)")
	return cmd
}

func newKeyEntryRoutingValidateCmd() *cobra.Command {
	var filePath, inlineJSON, wsID string
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Parse and validate a routing file locally (no server call unless --ws is set)",
		Long: `Parses the routing YAML or JSON and prints the rules that would be applied.
No server call is made unless --ws is provided, in which case each target value
is cross-checked against the workspace config snapshot.

This is the recommended step between writing routing.yaml and running 'set'.`,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if filePath == "" && inlineJSON == "" {
				return fmt.Errorf("provide --file=<path.yaml> or --json='{\"rules\":[...]}'")
			}
			overrides, err := loadRoutingOverrides(filePath, inlineJSON)
			if err != nil {
				return err
			}
			if len(overrides) == 0 {
				fmt.Println("no rules found")
				return nil
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			if wsID == "" {
				wsID = os.Getenv("ORANGE_WS_ID")
			}
			var providers []string
			if wsID != "" {
				providers, err = workspaceProviders(context.Background(), rc, wsID)
				if err != nil {
					return fmt.Errorf("fetch workspace providers: %w", err)
				}
			}
			return printValidatedRouting(rc.Printer, overrides, providers)
		},
	}
	cmd.Flags().StringVar(&filePath, "file", "", "path to YAML (or .json) file with routing rules")
	cmd.Flags().StringVar(&inlineJSON, "json", "", `inline JSON: '{"rules":[{"model":"...","target":"..."}]}'`)
	cmd.Flags().StringVar(&wsID, "ws", "", "workspace ID to cross-check targets (env: ORANGE_WS_ID)")
	return cmd
}

// printValidatedRouting prints the parsed routing rules. When providers is non-empty,
// each target is cross-checked against the list and a warning is printed for unknowns.
func printValidatedRouting(p *Printer, overrides []*routingv1.RoutingOverride, providers []string) error {
	providerSet := make(map[string]bool, len(providers))
	for _, name := range providers {
		providerSet[name] = true
	}

	rows := make([]string, 0, len(overrides))
	var warnings []string
	for _, o := range overrides {
		desc := describeRoutingNode(o.GetNode())
		rows = append(rows, fmt.Sprintf("%s\t%s", o.GetModelId(), desc))
		if len(providers) > 0 {
			collectTargetWarnings(o.GetNode(), o.GetModelId(), providerSet, &warnings)
		}
	}

	p.Table("MODEL\tROUTING (parsed)", rows)

	if len(warnings) > 0 {
		fmt.Println()
		for _, w := range warnings {
			fmt.Printf("  WARNING: %s\n", w)
		}
		return fmt.Errorf("%d target(s) not found in workspace config", len(warnings))
	}
	if len(providers) > 0 {
		fmt.Println("\nall targets validated against workspace config ✓")
	} else {
		fmt.Println("\nfile is valid (use --ws=<ws-id> to also cross-check targets against workspace)")
	}
	return nil
}

// collectTargetWarnings walks a RoutingNode and appends a warning for any
// provider not in the providerSet.
func collectTargetWarnings(n *routingv1.RoutingNode, model string, providerSet map[string]bool, out *[]string) {
	if n == nil {
		return
	}
	switch k := n.Kind.(type) {
	case *routingv1.RoutingNode_Target:
		p := k.Target.GetProvider()
		if p != "" && !providerSet[p] {
			*out = append(*out, fmt.Sprintf("model %q: target %q not in workspace providers", model, p))
		}
	case *routingv1.RoutingNode_Chain:
		for _, c := range k.Chain.GetChildren() {
			collectTargetWarnings(c, model, providerSet, out)
		}
	case *routingv1.RoutingNode_Split:
		for _, c := range k.Split.GetChildren() {
			collectTargetWarnings(c.GetNode(), model, providerSet, out)
		}
	}
}

// latestSnapshotYAML fetches the YAML payload of the latest successfully
// compiled snapshot for wsID.
func latestSnapshotYAML(ctx context.Context, rc *RunCtx, wsID string) (string, error) {
	cfgClient := configconnect.NewConfigAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)

	listResp, err := cfgClient.ListSnapshots(ctx, connect.NewRequest(&adminv1.ListSnapshotsRequest{WorkspaceId: wsID}))
	if err != nil {
		return "", err
	}
	snaps := listResp.Msg.GetSnapshots()
	if len(snaps) == 0 {
		return "", fmt.Errorf("no config snapshots for workspace %s — publish one first", wsID)
	}
	var latestVer uint64
	for _, s := range snaps {
		if s.GetCompiledOk() && s.GetVersion() > latestVer {
			latestVer = s.GetVersion()
		}
	}
	if latestVer == 0 {
		return "", fmt.Errorf("no successfully compiled snapshot for workspace %s", wsID)
	}
	getResp, err := cfgClient.GetSnapshot(ctx, connect.NewRequest(&adminv1.GetSnapshotRequest{
		WorkspaceId: wsID,
		Version:     latestVer,
	}))
	if err != nil {
		return "", err
	}
	payload := getResp.Msg.GetYamlPayload()
	if payload == "" {
		return "", fmt.Errorf("snapshot v%d has no YAML payload (binary format not supported here)", latestVer)
	}
	return payload, nil
}

// workspaceProviders fetches the latest config snapshot and returns the
// llm.providers map keys — valid target names for routing rules.
func workspaceProviders(ctx context.Context, rc *RunCtx, wsID string) ([]string, error) {
	payload, err := latestSnapshotYAML(ctx, rc, wsID)
	if err != nil {
		return nil, err
	}
	return parseProviderNames(payload)
}

// configSnapshot holds just the fields we need for routing authoring.
type configSnapshot struct {
	LLM struct {
		Providers map[string]struct{} `yaml:"providers"`
		Models    map[string]struct {
			Provider string `yaml:"provider"`
			Name     string `yaml:"name"`
		} `yaml:"models"`
	} `yaml:"llm"`
}

// parseProviderNames extracts the llm.providers map keys from config YAML.
func parseProviderNames(configYAML string) ([]string, error) {
	var snap configSnapshot
	if err := yaml.Unmarshal([]byte(configYAML), &snap); err != nil {
		return nil, fmt.Errorf("parse config YAML: %w", err)
	}
	names := make([]string, 0, len(snap.LLM.Providers))
	for name := range snap.LLM.Providers {
		names = append(names, name)
	}
	return names, nil
}

type backendModelEntry struct {
	ClientID    string // client-facing model ID (the map key)
	BackendName string // name sent to the upstream (Name field, or ClientID if empty)
}

// parseBackendModels returns all models belonging to provider from config YAML.
func parseBackendModels(configYAML, provider string) ([]backendModelEntry, error) {
	var snap configSnapshot
	if err := yaml.Unmarshal([]byte(configYAML), &snap); err != nil {
		return nil, fmt.Errorf("parse config YAML: %w", err)
	}
	var entries []backendModelEntry
	for clientID, m := range snap.LLM.Models {
		if m.Provider != provider {
			continue
		}
		backend := m.Name
		if backend == "" {
			backend = clientID // default: same as client-facing ID
		}
		entries = append(entries, backendModelEntry{ClientID: clientID, BackendName: backend})
	}
	if len(entries) == 0 {
		// Check if the provider exists at all.
		if _, ok := snap.LLM.Providers[provider]; !ok {
			return nil, fmt.Errorf("provider %q not found in workspace config — run 'keyentry-routing targets' to list valid providers", provider)
		}
	}
	return entries, nil
}

// printProviderTargets prints workspace LLM provider names as routing target candidates.
func printProviderTargets(p *Printer, providers []string) {
	if len(providers) == 0 {
		fmt.Println("no LLM providers configured in the workspace config snapshot")
		return
	}
	rows := make([]string, len(providers))
	for i, name := range providers {
		rows[i] = name
	}
	p.Table("TARGET (provider name)", rows)
	fmt.Println("\nUse these names as the 'target' field in routing rules.")
	fmt.Println("Run 'keyentry-routing models <provider>' to see backend model names for a provider.")
	fmt.Println("To attach credentials for a provider to a key entry:")
	fmt.Println("  orange admin keyentry-secret create <key-entry-id> --target=<provider-name>")
}

// printBackendModels prints models for a provider as backend_model authoring candidates.
func printBackendModels(p *Printer, provider string, entries []backendModelEntry) {
	if len(entries) == 0 {
		fmt.Printf("provider %q has no models defined in the workspace config\n", provider)
		return
	}
	rows := make([]string, len(entries))
	for i, e := range entries {
		same := ""
		if e.BackendName == e.ClientID {
			same = "(same as client ID)"
		}
		rows[i] = fmt.Sprintf("%s\t%s\t%s", e.ClientID, e.BackendName, same)
	}
	p.Table("CLIENT MODEL ID\tBACKEND MODEL NAME\tNOTE", rows)
	fmt.Printf("\nUse BACKEND MODEL NAME as 'backend_model' in routing rules for target '%s'.\n", provider)
}

// ── REPL: keyentry-routing (admin) ────────────────────────────────────────────

// cmdKeyEntryRouting routes keyentry-routing REPL subcommands.
//
//	keyentry-routing get <key-entry-id>
//	keyentry-routing set <key-entry-id> file=<path>
//	keyentry-routing set <key-entry-id> json='{"rules":[...]}'
//	keyentry-routing delete <key-entry-id>
func (s *replState) cmdKeyEntryRouting(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	ctx := context.Background()
	client := keyentryconnect.NewKeyEntryAdminServiceClient(s.rc.HTTPClient, s.rc.ServerURL, s.rc.ConnectOpts...)

	switch sub {
	case "get":
		if len(args) < 2 {
			return fmt.Errorf("usage: keyentry-routing get <key-entry-id>")
		}
		resp, err := client.GetKey(ctx, connect.NewRequest(&keyentryv1.GetKeyRequest{KeyEntryId: args[1]}))
		if err != nil {
			return err
		}
		return printRoutingOverrides(s.rc.Printer, resp.Msg.GetKey())

	case "set":
		if len(args) < 2 {
			return fmt.Errorf("usage: keyentry-routing set <key-entry-id> file=<path>|json='...'")
		}
		keID := args[1]
		overrides, err := routingOverridesFromKV(args[2:])
		if err != nil {
			return err
		}
		existing, err := client.GetKey(ctx, connect.NewRequest(&keyentryv1.GetKeyRequest{KeyEntryId: keID}))
		if err != nil {
			return err
		}
		req := &keyentryv1.UpdateKeyRequest{
			KeyEntryId:      keID,
			Description:     existing.Msg.GetKey().Description,
			RoutingOverrides: overrides,
		}
		if err := validateRoutingRequest(req); err != nil {
			return err
		}
		resp, err := client.UpdateKey(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		return printRoutingOverrides(s.rc.Printer, resp.Msg.GetKey())

	case "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: keyentry-routing delete <key-entry-id>")
		}
		existing, err := client.GetKey(ctx, connect.NewRequest(&keyentryv1.GetKeyRequest{KeyEntryId: args[1]}))
		if err != nil {
			return err
		}
		req := &keyentryv1.UpdateKeyRequest{
			KeyEntryId:      args[1],
			Description:     existing.Msg.GetKey().Description,
			RoutingOverrides: []*routingv1.RoutingOverride{},
		}
		if err := validateRoutingRequest(req); err != nil {
			return err
		}
		resp, err := client.UpdateKey(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		return printRoutingOverrides(s.rc.Printer, resp.Msg.GetKey())

	case "validate":
		overrides, err := routingOverridesFromKV(args[1:])
		if err != nil {
			return err
		}
		var providers []string
		wsID := s.wsID
		for _, kv := range args[1:] {
			if strings.HasPrefix(kv, "ws=") {
				wsID = strings.TrimPrefix(kv, "ws=")
			}
		}
		if wsID != "" {
			providers, _ = workspaceProviders(ctx, s.rc, wsID)
		}
		return printValidatedRouting(s.rc.Printer, overrides, providers)

	case "targets":
		wsID := s.wsID
		if len(args) > 1 && !containsEq(args[1]) {
			wsID = args[1]
		}
		if wsID == "" {
			return fmt.Errorf("no workspace in context — provide ws-id or run 'use ws <id>'")
		}
		providers, err := workspaceProviders(ctx, s.rc, wsID)
		if err != nil {
			return err
		}
		printProviderTargets(s.rc.Printer, providers)

	case "models":
		if len(args) < 2 {
			return fmt.Errorf("usage: keyentry-routing models <provider-name> [<ws-id>]")
		}
		provider := args[1]
		wsID := s.wsID
		if len(args) > 2 && !containsEq(args[2]) {
			wsID = args[2]
		}
		if wsID == "" {
			return fmt.Errorf("no workspace in context — provide ws-id or run 'use ws <id>'")
		}
		payload, err := latestSnapshotYAML(ctx, s.rc, wsID)
		if err != nil {
			return err
		}
		entries, err := parseBackendModels(payload, provider)
		if err != nil {
			return err
		}
		printBackendModels(s.rc.Printer, provider, entries)

	default:
		return fmt.Errorf("unknown keyentry-routing subcommand %q — try: get, set, delete, validate, targets, models", sub)
	}
	return nil
}

// ── REPL: keyentry-routing (user self-service) ────────────────────────────────

// cmdUserKeyEntryRouting lets the user manage routing overrides on their own token slots.
// Requires keyentry:write[ws] scope.
//
//	keyentry-routing get <key-entry-id>
//	keyentry-routing set <key-entry-id> file=<path>
//	keyentry-routing set <key-entry-id> json='{"rules":[...]}'
//	keyentry-routing delete <key-entry-id>
func (s *userReplState) cmdUserKeyEntryRouting(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	ctx := context.Background()
	client := keyentryconnect.NewKeyEntryAdminServiceClient(s.rc.HTTPClient, s.rc.ServerURL, s.rc.ConnectOpts...)

	switch sub {
	case "get":
		if len(args) < 2 {
			return fmt.Errorf("usage: keyentry-routing get <key-entry-id>")
		}
		resp, err := client.GetKey(ctx, connect.NewRequest(&keyentryv1.GetKeyRequest{KeyEntryId: args[1]}))
		if err != nil {
			return err
		}
		return printRoutingOverrides(s.rc.Printer, resp.Msg.GetKey())

	case "set":
		if len(args) < 2 {
			return fmt.Errorf("usage: keyentry-routing set <key-entry-id> file=<path>|json='...'")
		}
		keID := args[1]
		overrides, err := routingOverridesFromKV(args[2:])
		if err != nil {
			return err
		}
		existing, err := client.GetKey(ctx, connect.NewRequest(&keyentryv1.GetKeyRequest{KeyEntryId: keID}))
		if err != nil {
			return err
		}
		req := &keyentryv1.UpdateKeyRequest{
			KeyEntryId:      keID,
			Description:     existing.Msg.GetKey().Description,
			RoutingOverrides: overrides,
		}
		if err := validateRoutingRequest(req); err != nil {
			return err
		}
		resp, err := client.UpdateKey(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		return printRoutingOverrides(s.rc.Printer, resp.Msg.GetKey())

	case "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: keyentry-routing delete <key-entry-id>")
		}
		existing, err := client.GetKey(ctx, connect.NewRequest(&keyentryv1.GetKeyRequest{KeyEntryId: args[1]}))
		if err != nil {
			return err
		}
		req := &keyentryv1.UpdateKeyRequest{
			KeyEntryId:      args[1],
			Description:     existing.Msg.GetKey().Description,
			RoutingOverrides: []*routingv1.RoutingOverride{},
		}
		if err := validateRoutingRequest(req); err != nil {
			return err
		}
		resp, err := client.UpdateKey(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		return printRoutingOverrides(s.rc.Printer, resp.Msg.GetKey())

	case "validate":
		overrides, err := routingOverridesFromKV(args[1:])
		if err != nil {
			return err
		}
		var providers []string
		wsID := s.wsID
		for _, kv := range args[1:] {
			if strings.HasPrefix(kv, "ws=") {
				wsID = strings.TrimPrefix(kv, "ws=")
			}
		}
		if wsID != "" {
			providers, _ = workspaceProviders(ctx, s.rc, wsID)
		}
		return printValidatedRouting(s.rc.Printer, overrides, providers)

	case "targets":
		wsID := s.wsID
		if len(args) > 1 && !containsEq(args[1]) {
			wsID = args[1]
		}
		if wsID == "" {
			return fmt.Errorf("no workspace in context — run 'use ws <id>' first")
		}
		providers, err := workspaceProviders(ctx, s.rc, wsID)
		if err != nil {
			return err
		}
		printProviderTargets(s.rc.Printer, providers)

	case "models":
		if len(args) < 2 {
			return fmt.Errorf("usage: keyentry-routing models <provider-name> [<ws-id>]")
		}
		provider := args[1]
		wsID := s.wsID
		if len(args) > 2 && !containsEq(args[2]) {
			wsID = args[2]
		}
		if wsID == "" {
			return fmt.Errorf("no workspace in context — run 'use ws <id>' first")
		}
		payload, err := latestSnapshotYAML(ctx, s.rc, wsID)
		if err != nil {
			return err
		}
		entries, err := parseBackendModels(payload, provider)
		if err != nil {
			return err
		}
		printBackendModels(s.rc.Printer, provider, entries)

	default:
		return fmt.Errorf("unknown keyentry-routing subcommand %q — try: get, set, delete, validate, targets, models", sub)
	}
	return nil
}

// routingOverridesFromKV parses file=<path> or json='...' from REPL token list.
func routingOverridesFromKV(kvs []string) ([]*routingv1.RoutingOverride, error) {
	filePath := kvGet(kvs, "file")
	inlineJSON := kvGet(kvs, "json")
	if filePath == "" && inlineJSON == "" {
		return nil, fmt.Errorf("provide file=<path> or json='...'")
	}
	return loadRoutingOverrides(filePath, inlineJSON)
}

// ── print helpers ─────────────────────────────────────────────────────────────

func printRoutingOverrides(p *Printer, key *keyentryv1.Key) error {
	if p.Format != FormatTable {
		return p.Proto(key)
	}
	overrides := key.GetRoutingOverrides()
	if len(overrides) == 0 {
		fmt.Printf("key %s: no routing overrides (uses workspace default)\n", shortID(key.GetKeyEntryId()))
		return nil
	}
	rows := make([]string, len(overrides))
	for i, o := range overrides {
		rows[i] = fmt.Sprintf("%s\t%s", o.GetModelId(), describeRoutingNode(o.GetNode()))
	}
	p.Table("MODEL\tROUTING", rows)
	return nil
}

func describeRoutingNode(n *routingv1.RoutingNode) string {
	if n == nil {
		return "(none)"
	}
	switch k := n.Kind.(type) {
	case *routingv1.RoutingNode_Target:
		t := k.Target
		if t.GetModel() != "" {
			return t.GetProvider() + " → " + t.GetModel()
		}
		return t.GetProvider()
	case *routingv1.RoutingNode_Chain:
		parts := make([]string, len(k.Chain.GetChildren()))
		for i, c := range k.Chain.GetChildren() {
			parts[i] = describeRoutingNode(c)
		}
		return "chain[" + strings.Join(parts, " → ") + "]"
	case *routingv1.RoutingNode_Split:
		parts := make([]string, len(k.Split.GetChildren()))
		for i, c := range k.Split.GetChildren() {
			parts[i] = fmt.Sprintf("%d%%:%s", c.GetWeight(), describeRoutingNode(c.GetNode()))
		}
		return "split[" + strings.Join(parts, " | ") + "]"
	default:
		return "(unknown)"
	}
}
