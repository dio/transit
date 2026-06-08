package server

import (
	"context"
	"fmt"
	"os"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	apikeyv1 "github.com/dio/transit/examples/orange/api/orange/apikey/admin/v1"
	apikeyconnect "github.com/dio/transit/examples/orange/api/orange/apikey/admin/v1/adminv1connect"
	"github.com/dio/transit/examples/orange/internal/server/apikeys"
	"github.com/dio/transit/examples/orange/internal/server/scopes"
)

func newAPIKeyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "apikey",
		Aliases: []string{"key"},
		Short:   "Manage API keys",
	}
	cmd.AddCommand(newAPIKeyIssueCmd())
	cmd.AddCommand(newAPIKeyGetCmd())
	cmd.AddCommand(newAPIKeyListCmd())
	cmd.AddCommand(newAPIKeyUpdateScopeCmd())
	cmd.AddCommand(newAPIKeyRevokeCmd())
	return cmd
}

func newAPIKeyIssueCmd() *cobra.Command {
	var orgID, userID, workspaceID, scopeFlag, tmpl, description string
	cmd := &cobra.Command{
		Use:          "issue",
		Short:        "Issue a new API key",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if orgID == "" {
				orgID = os.Getenv("ORANGE_ORG_ID")
			}
			if orgID == "" {
				return fmt.Errorf("--org-id is required (or set ORANGE_ORG_ID)")
			}
			if workspaceID == "" {
				workspaceID = os.Getenv("ORANGE_WS_ID")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}

			scopeList := parseScopes(scopeFlag)
			if tmpl != "" {
				extra, err := templateScopes(tmpl, workspaceID, userID)
				if err != nil {
					return err
				}
				scopeList = mergeScopes(scopeList, extra)
			}

			rec, plaintext, err := issueAPIKey(rc, orgID, userID, workspaceID, scopeList, description)
			if err != nil {
				return err
			}
			return printAPIKeys(rc.Printer, plaintext, rec)
		},
	}
	cmd.Flags().StringVar(&orgID, "org-id", "", "org ID (env: ORANGE_ORG_ID)")
	cmd.Flags().StringVar(&userID, "user-id", "", "user ID to associate with the key")
	cmd.Flags().StringVar(&workspaceID, "workspace-id", "", "workspace ID to scope the key (env: ORANGE_WS_ID)")
	cmd.Flags().StringVar(&scopeFlag, "scope", "", "comma-separated scopes (default: user:read)")
	cmd.Flags().StringVar(&tmpl, "template", "", `scope template shortcut: "ws-member" (requires --workspace-id)`)
	cmd.Flags().StringVar(&description, "description", "", "key description")
	return cmd
}

func newAPIKeyGetCmd() *cobra.Command {
	var keyID string
	cmd := &cobra.Command{
		Use:          "get",
		Short:        "Get API key details",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			if keyID == "" && len(args) > 0 {
				keyID = args[0]
			}
			if keyID == "" {
				return fmt.Errorf("--key-id is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := apikeyconnect.NewAPIKeyAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.GetKey(context.Background(), connect.NewRequest(&apikeyv1.GetKeyRequest{KeyId: keyID}))
			if err != nil {
				return err
			}
			return printAPIKeyDetail(rc.Printer, resp.Msg.GetKey())
		},
	}
	cmd.Flags().StringVar(&keyID, "key-id", "", "key ID to retrieve")
	return cmd
}

func newAPIKeyListCmd() *cobra.Command {
	var orgID, userID string
	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List active API keys",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if orgID == "" {
				orgID = os.Getenv("ORANGE_ORG_ID")
			}
			if orgID == "" {
				return fmt.Errorf("--org-id is required (or set ORANGE_ORG_ID)")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := apikeyconnect.NewAPIKeyAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.ListKeys(context.Background(), connect.NewRequest(&apikeyv1.ListKeysRequest{
				OrgId:  orgID,
				UserId: userID,
			}))
			if err != nil {
				return err
			}
			return printAPIKeys(rc.Printer, "", resp.Msg.GetKeys()...)
		},
	}
	cmd.Flags().StringVar(&orgID, "org-id", "", "org ID (env: ORANGE_ORG_ID)")
	cmd.Flags().StringVar(&userID, "user-id", "", "filter by user ID")
	return cmd
}

func newAPIKeyUpdateScopeCmd() *cobra.Command {
	var keyID, scopeFlag, tmpl, workspaceID string
	cmd := &cobra.Command{
		Use:   "update-scope",
		Short: "Merge scopes into an existing API key (additive — existing scopes are preserved)",
		Long: `Merge additional scopes into an existing API key without dropping existing ones.

The key's plaintext token is unchanged; only its scope list is updated.

Examples:
  # Add a single scope
  orange admin apikey update-scope --key-id=<id> --scope=secret:read

  # Use the ws-member template to add all workspace member scopes at once
  orange admin apikey update-scope --key-id=<id> --template=ws-member --workspace-id=<ws-id>

  # Combine explicit scopes and a template
  orange admin apikey update-scope --key-id=<id> --scope=user:read --template=ws-member --workspace-id=<ws-id>`,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if keyID == "" {
				return fmt.Errorf("--key-id is required")
			}
			if splitScopes(scopeFlag) == nil && tmpl == "" {
				return fmt.Errorf("no scopes to add: provide --scope or --template")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := apikeyconnect.NewAPIKeyAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.UpdateKeyScopes(context.Background(), connect.NewRequest(&apikeyv1.UpdateKeyScopesRequest{
				KeyId:       keyID,
				AddScopes:   splitScopes(scopeFlag),
				Template:    tmpl,
				WorkspaceId: workspaceID,
			}))
			if err != nil {
				return err
			}
			return printAPIKeys(rc.Printer, "", resp.Msg.GetKey())
		},
	}
	cmd.Flags().StringVar(&keyID, "key-id", "", "key ID to update")
	cmd.Flags().StringVar(&scopeFlag, "scope", "", "comma-separated scopes to add")
	cmd.Flags().StringVar(&tmpl, "template", "", `scope template: "ws-member" (requires --workspace-id)`)
	cmd.Flags().StringVar(&workspaceID, "workspace-id", "", "workspace ID (required for --template=ws-member)")
	return cmd
}

func newAPIKeyRevokeCmd() *cobra.Command {
	var keyID string
	cmd := &cobra.Command{
		Use:          "revoke",
		Short:        "Revoke an API key",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if keyID == "" {
				return fmt.Errorf("--key-id is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := apikeyconnect.NewAPIKeyAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			_, err = client.RevokeKey(context.Background(), connect.NewRequest(&apikeyv1.RevokeKeyRequest{KeyId: keyID}))
			if err != nil {
				return err
			}
			rc.Printer.OK("revoked")
			return nil
		},
	}
	cmd.Flags().StringVar(&keyID, "key-id", "", "key ID to revoke")
	return cmd
}

// issueAPIKey calls IssueKey via Connect and returns the record and plaintext.
func issueAPIKey(rc *RunCtx, orgID, userID, workspaceID string, scopeList []string, description string) (*apikeyv1.ApiKey, string, error) {
	client := apikeyconnect.NewAPIKeyAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
	req := &apikeyv1.IssueKeyRequest{
		OrgId:       orgID,
		UserId:      userID,
		WorkspaceId: workspaceID,
		Scopes:      scopeList,
	}
	if description != "" {
		req.Description = &description
	}
	resp, err := client.IssueKey(context.Background(), connect.NewRequest(req))
	if err != nil {
		return nil, "", err
	}
	return resp.Msg.GetKey(), resp.Msg.GetPlaintext(), nil
}

// templateScopes returns the scope list for a named template.
func templateScopes(tmpl, workspaceID, userID string) ([]string, error) {
	switch tmpl {
	case "ws-member":
		if workspaceID == "" {
			return nil, fmt.Errorf("--workspace-id is required for --template=ws-member")
		}
		return scopes.WorkspaceMemberScopes(workspaceID, userID), nil
	default:
		return nil, fmt.Errorf("unknown template %q — supported: ws-member", tmpl)
	}
}

// mergeScopes returns a deduplicated union of a and b.
func mergeScopes(a, b []string) []string {
	have := make(map[string]bool, len(a))
	out := make([]string, len(a))
	copy(out, a)
	for _, s := range a {
		have[s] = true
	}
	for _, s := range b {
		if !have[s] {
			out = append(out, s)
			have[s] = true
		}
	}
	return out
}

// parseScopes splits a comma-separated scope string.
// Falls back to DefaultUserScopes when empty.
func parseScopes(s string) []string {
	if s == "" {
		return apikeys.DefaultUserScopes
	}
	parts := splitScopes(s)
	if len(parts) == 0 {
		return apikeys.DefaultUserScopes
	}
	return parts
}

// splitScopes splits a comma-separated scope string without any default fallback.
func splitScopes(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// printAPIKeys renders key records in list/issue format.
// plaintext is non-empty only on issue (shown in the first row).
func printAPIKeys(p *Printer, plaintext string, keys ...*apikeyv1.ApiKey) error {
	switch p.Format {
	case FormatJSON, FormatYAML:
		msgs := make([]proto.Message, len(keys))
		for i, k := range keys {
			msgs[i] = k
		}
		if len(msgs) == 1 {
			return p.Proto(msgs[0])
		}
		return p.ProtoList(msgs)
	default:
		rows := make([]string, len(keys))
		for i, k := range keys {
			keyCol := k.GetKeyPrefix() + "…"
			if i == 0 && plaintext != "" {
				keyCol = plaintext
			}
			desc := "-"
			if k.Description != nil && *k.Description != "" {
				desc = *k.Description
			}
			uid, wsid := k.GetUserId(), k.GetWorkspaceId()
			rows[i] = fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s",
				keyCol,
				strings.Join(k.GetScopes(), " "),
				clip(&uid, 20),
				clip(&wsid, 20),
				clip(&desc, 24),
				age(k.GetCreatedAt()),
			)
		}
		header := "KEY-PREFIX\tSCOPES\tUSER-ID\tWS-ID\tDESCRIPTION\tAGE"
		if plaintext != "" {
			header = "KEY (shown once)\tSCOPES\tUSER-ID\tWS-ID\tDESCRIPTION\tAGE"
		}
		p.Table(header, rows)
		return nil
	}
}

// printAPIKeyDetail renders a single key with full details.
func printAPIKeyDetail(p *Printer, k *apikeyv1.ApiKey) error {
	switch p.Format {
	case FormatJSON, FormatYAML:
		return p.Proto(k)
	default:
		desc := "-"
		if k.Description != nil && *k.Description != "" {
			desc = *k.Description
		}
		rows := []string{
			fmt.Sprintf("KEY-ID\t%s", k.GetKeyId()),
			fmt.Sprintf("PREFIX\t%s…", k.GetKeyPrefix()),
			fmt.Sprintf("ORG-ID\t%s", k.GetOrgId()),
			fmt.Sprintf("USER-ID\t%s", orDash(k.GetUserId())),
			fmt.Sprintf("WS-ID\t%s", orDash(k.GetWorkspaceId())),
			fmt.Sprintf("DESCRIPTION\t%s", desc),
			fmt.Sprintf("CREATED\t%s ago", age(k.GetCreatedAt())),
		}
		for i, sc := range k.GetScopes() {
			label := "\t"
			if i == 0 {
				label = "SCOPES\t"
			}
			rows = append(rows, label+sc)
		}
		p.Table("FIELD\tVALUE", rows)
		return nil
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
