package server

import (
	"context"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	policyv1 "github.com/dio/transit/examples/orange/api/orange/policy/admin/v1"
	policyconnect "github.com/dio/transit/examples/orange/api/orange/policy/admin/v1/adminv1connect"
)

// ── cobra: policy ─────────────────────────────────────────────────────────────

func newPolicyAdminCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rl-policy",
		Short: "Manage rate-limit policies (floor/flexible) and their rules",
	}
	cmd.AddCommand(
		newPolicyListCmd(),
		newPolicyGetCmd(),
		newPolicyCreateCmd(),
		newPolicyUpdateCmd(),
		newPolicyDeleteCmd(),
	)
	return cmd
}

func newPolicyListCmd() *cobra.Command {
	var scopeTypeStr, scopeID, policyTypeStr string
	cmd := &cobra.Command{
		Use:          "ls",
		Short:        "List policies",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			req := &policyv1.ListPoliciesRequest{}
			if scopeTypeStr != "" {
				st := parseScopeType(scopeTypeStr)
				req.ScopeType = &st
			}
			if scopeID != "" {
				req.ScopeId = &scopeID
			}
			if policyTypeStr != "" {
				pt := parsePolicyType(policyTypeStr)
				req.Type = &pt
			}
			client := policyconnect.NewPolicyAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.ListPolicies(context.Background(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			return printPolicies(rc.Printer, resp.Msg.GetPolicies()...)
		},
	}
	cmd.Flags().StringVar(&scopeTypeStr, "scope-type", "", "filter by scope type: project | workspace | key")
	cmd.Flags().StringVar(&scopeID, "scope-id", "", "filter by scope ID (project/workspace/key UUID)")
	cmd.Flags().StringVar(&policyTypeStr, "type", "", "filter by policy type: floor | flexible")
	return cmd
}

func newPolicyGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "get <policy-id>",
		Short:        "Get a policy and its rules",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := policyconnect.NewPolicyAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.GetPolicy(context.Background(), connect.NewRequest(&policyv1.GetPolicyRequest{
				PolicyId: args[0],
			}))
			if err != nil {
				return err
			}
			return rc.Printer.Proto(resp.Msg.GetPolicy())
		},
	}
	return cmd
}

func newPolicyCreateCmd() *cobra.Command {
	var scopeTypeStr, scopeID, policyTypeStr, description string
	var ruleJSONs []string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a policy with an ordered set of rules",
		Long: `Create a rate-limit policy attached to a project, workspace, or key.

Each --rule value is a JSON object matching PolicyRule fields:

  orange admin policy create \
    --scope-type workspace --scope-id <ws-id> \
    --type floor \
    --rule '{"models":["*"],"rpm":60}' \
    --rule '{"models":["claude-3-opus"],"rpm":10,"on_exceed":"throttle"}'`,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if scopeTypeStr == "" {
				return fmt.Errorf("--scope-type is required (project | workspace | key)")
			}
			if scopeID == "" {
				return fmt.Errorf("--scope-id is required")
			}
			if policyTypeStr == "" {
				return fmt.Errorf("--type is required (floor | flexible)")
			}
			rules, err := parseRuleJSONSlice(ruleJSONs)
			if err != nil {
				return err
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			req := &policyv1.CreatePolicyRequest{
				ScopeType: parseScopeType(scopeTypeStr),
				ScopeId:   scopeID,
				Type:      parsePolicyType(policyTypeStr),
				Rules:     rules,
			}
			if description != "" {
				req.Description = &description
			}
			client := policyconnect.NewPolicyAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.CreatePolicy(context.Background(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			return rc.Printer.Proto(resp.Msg.GetPolicy())
		},
	}
	cmd.Flags().StringVar(&scopeTypeStr, "scope-type", "", "scope type: project | workspace | key")
	cmd.Flags().StringVar(&scopeID, "scope-id", "", "scope UUID (project_id, workspace_id, or key_id)")
	cmd.Flags().StringVar(&policyTypeStr, "type", "", "policy type: floor | flexible")
	cmd.Flags().StringVar(&description, "description", "", "optional description")
	cmd.Flags().StringArrayVar(&ruleJSONs, "rule", nil, "JSON rule (repeatable, order matters)")
	return cmd
}

func newPolicyUpdateCmd() *cobra.Command {
	var description string
	var ruleJSONs []string
	cmd := &cobra.Command{
		Use:   "update <policy-id>",
		Short: "Update a policy's description and replace its rules",
		Long: `Update replaces the entire rule set and optionally the description.
All existing rules are removed and replaced by the supplied --rule flags.`,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			rules, err := parseRuleJSONSlice(ruleJSONs)
			if err != nil {
				return err
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			req := &policyv1.UpdatePolicyRequest{
				PolicyId: args[0],
				Rules:    rules,
			}
			if description != "" {
				req.Description = &description
			}
			client := policyconnect.NewPolicyAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.UpdatePolicy(context.Background(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			return rc.Printer.Proto(resp.Msg.GetPolicy())
		},
	}
	cmd.Flags().StringVar(&description, "description", "", "updated description")
	cmd.Flags().StringArrayVar(&ruleJSONs, "rule", nil, "JSON rule (repeatable, replaces all existing rules)")
	return cmd
}

func newPolicyDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "delete <policy-id>",
		Short:        "Delete a policy (cascades to its rules)",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := policyconnect.NewPolicyAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			_, err = client.DeletePolicy(context.Background(), connect.NewRequest(&policyv1.DeletePolicyRequest{
				PolicyId: args[0],
			}))
			if err != nil {
				return err
			}
			rc.Printer.OK("deleted")
			return nil
		},
	}
	return cmd
}

// ── REPL: policy ──────────────────────────────────────────────────────────────

// cmdPolicy routes policy REPL subcommands.
//
//	policy ls [scope-type=ws|proj|key] [scope-id=<id>] [type=floor|flexible]
//	policy get <policy-id>
//	policy create scope-type=ws|proj|key scope-id=<id> type=floor|flexible [rule='<json>'] [desc='...']
//	policy update <policy-id> [rule='<json>'] [desc='...']
//	policy delete <policy-id>
func (s *replState) cmdPolicy(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	ctx := context.Background()
	client := policyconnect.NewPolicyAdminServiceClient(s.rc.HTTPClient, s.rc.ServerURL, s.rc.ConnectOpts...)

	switch sub {
	case "ls", "list":
		req := &policyv1.ListPoliciesRequest{}
		if stStr := kvGet(args[1:], "scope-type"); stStr != "" {
			st := parseScopeType(stStr)
			req.ScopeType = &st
		}
		if sid := kvGet(args[1:], "scope-id"); sid != "" {
			req.ScopeId = &sid
		}
		if ptStr := kvGet(args[1:], "type"); ptStr != "" {
			pt := parsePolicyType(ptStr)
			req.Type = &pt
		}
		resp, err := client.ListPolicies(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		return printPolicies(s.rc.Printer, resp.Msg.GetPolicies()...)

	case "get":
		if len(args) < 2 {
			return fmt.Errorf("usage: policy get <policy-id>")
		}
		resp, err := client.GetPolicy(ctx, connect.NewRequest(&policyv1.GetPolicyRequest{PolicyId: args[1]}))
		if err != nil {
			return err
		}
		return s.rc.Printer.Proto(resp.Msg.GetPolicy())

	case "create":
		stStr := kvGet(args[1:], "scope-type")
		scopeID := kvGet(args[1:], "scope-id")
		ptStr := kvGet(args[1:], "type")
		desc := kvGet(args[1:], "desc")
		if stStr == "" {
			return fmt.Errorf("scope-type=<project|workspace|key> required")
		}
		if scopeID == "" {
			return fmt.Errorf("scope-id=<uuid> required")
		}
		if ptStr == "" {
			return fmt.Errorf("type=<floor|flexible> required")
		}
		ruleJSONs := kvGetAll(args[1:], "rule")
		rules, err := parseRuleJSONSlice(ruleJSONs)
		if err != nil {
			return err
		}
		req := &policyv1.CreatePolicyRequest{
			ScopeType: parseScopeType(stStr),
			ScopeId:   scopeID,
			Type:      parsePolicyType(ptStr),
			Rules:     rules,
		}
		if desc != "" {
			req.Description = &desc
		}
		resp, err := client.CreatePolicy(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		return s.rc.Printer.Proto(resp.Msg.GetPolicy())

	case "update":
		if len(args) < 2 {
			return fmt.Errorf("usage: policy update <policy-id> [rule='<json>'] [desc='...']")
		}
		policyID := args[1]
		desc := kvGet(args[2:], "desc")
		ruleJSONs := kvGetAll(args[2:], "rule")
		rules, err := parseRuleJSONSlice(ruleJSONs)
		if err != nil {
			return err
		}
		req := &policyv1.UpdatePolicyRequest{
			PolicyId: policyID,
			Rules:    rules,
		}
		if desc != "" {
			req.Description = &desc
		}
		resp, err := client.UpdatePolicy(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		return s.rc.Printer.Proto(resp.Msg.GetPolicy())

	case "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: policy delete <policy-id>")
		}
		_, err := client.DeletePolicy(ctx, connect.NewRequest(&policyv1.DeletePolicyRequest{PolicyId: args[1]}))
		if err != nil {
			return err
		}
		s.rc.Printer.OK("deleted")

	default:
		return fmt.Errorf("unknown rl-policy subcommand %q — try: ls, get, create, update, delete", sub)
	}
	return nil
}

// ── print helpers ─────────────────────────────────────────────────────────────

func printPolicies(p *Printer, policies ...*policyv1.Policy) error {
	if p.Format != FormatTable {
		for _, pol := range policies {
			if err := p.Proto(pol); err != nil {
				return err
			}
		}
		return nil
	}
	rows := make([]string, len(policies))
	for i, pol := range policies {
		desc := "-"
		if pol.Description != nil {
			desc = clip(pol.Description, 32)
		}
		rows[i] = fmt.Sprintf("%s\t%s\t%s\t%s\t%d\t%s\t%s",
			shortID(pol.GetPolicyId()),
			scopeTypeLabel(pol.GetScopeType()),
			shortID(pol.GetScopeId()),
			policyTypeLabel(pol.GetType()),
			len(pol.GetRules()),
			desc,
			age(pol.GetCreatedAt()),
		)
	}
	p.Table("POLICY-ID\tSCOPE-TYPE\tSCOPE-ID\tTYPE\tRULES\tDESCRIPTION\tAGE", rows)
	return nil
}

// ── parse helpers ─────────────────────────────────────────────────────────────

func parseScopeType(s string) policyv1.PolicyScopeType {
	switch strings.ToLower(s) {
	case "project", "proj":
		return policyv1.PolicyScopeType_POLICY_SCOPE_TYPE_PROJECT
	case "workspace", "ws":
		return policyv1.PolicyScopeType_POLICY_SCOPE_TYPE_WORKSPACE
	case "key":
		return policyv1.PolicyScopeType_POLICY_SCOPE_TYPE_KEY
	default:
		return policyv1.PolicyScopeType_POLICY_SCOPE_TYPE_UNSPECIFIED
	}
}

func parsePolicyType(s string) policyv1.PolicyType {
	switch strings.ToLower(s) {
	case "floor":
		return policyv1.PolicyType_POLICY_TYPE_FLOOR
	case "flexible":
		return policyv1.PolicyType_POLICY_TYPE_FLEXIBLE
	default:
		return policyv1.PolicyType_POLICY_TYPE_UNSPECIFIED
	}
}

func scopeTypeLabel(st policyv1.PolicyScopeType) string {
	switch st {
	case policyv1.PolicyScopeType_POLICY_SCOPE_TYPE_PROJECT:
		return "project"
	case policyv1.PolicyScopeType_POLICY_SCOPE_TYPE_WORKSPACE:
		return "workspace"
	case policyv1.PolicyScopeType_POLICY_SCOPE_TYPE_KEY:
		return "key"
	default:
		return "unknown"
	}
}

func policyTypeLabel(pt policyv1.PolicyType) string {
	switch pt {
	case policyv1.PolicyType_POLICY_TYPE_FLOOR:
		return "floor"
	case policyv1.PolicyType_POLICY_TYPE_FLEXIBLE:
		return "flexible"
	default:
		return "unknown"
	}
}

// parseRuleJSONSlice unmarshals a slice of JSON strings into PolicyRule protos.
func parseRuleJSONSlice(jsonStrings []string) ([]*policyv1.PolicyRule, error) {
	rules := make([]*policyv1.PolicyRule, 0, len(jsonStrings))
	for i, s := range jsonStrings {
		var r policyv1.PolicyRule
		if err := protojson.Unmarshal([]byte(s), &r); err != nil {
			return nil, fmt.Errorf("rule[%d]: invalid JSON: %w", i, err)
		}
		rules = append(rules, &r)
	}
	return rules, nil
}
