package orange

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
	"github.com/dio/transit/examples/orange/internal/orange/apikeys"
)

func newAPIKeyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "apikey",
		Aliases: []string{"key"},
		Short:   "Manage API keys",
	}
	cmd.AddCommand(newAPIKeyIssueCmd())
	cmd.AddCommand(newAPIKeyListCmd())
	cmd.AddCommand(newAPIKeyRevokeCmd())
	return cmd
}

func newAPIKeyIssueCmd() *cobra.Command {
	var orgID, userID, scopeFlag, description string
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
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			rec, plaintext, err := issueAPIKey(rc, orgID, userID, parseScopes(scopeFlag), description)
			if err != nil {
				return err
			}
			return printAPIKeys(rc.Printer, plaintext, rec)
		},
	}
	cmd.Flags().StringVar(&orgID, "org-id", "", "org ID (env: ORANGE_ORG_ID)")
	cmd.Flags().StringVar(&userID, "user-id", "", "user ID to associate with the key")
	cmd.Flags().StringVar(&scopeFlag, "scope", "read,write", "comma-separated scopes: read, write, admin, proxy, user, token:issue, egress-bundle:download")
	cmd.Flags().StringVar(&description, "description", "", "key description")
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
func issueAPIKey(rc *RunCtx, orgID, userID string, scopes []string, description string) (*apikeyv1.ApiKey, string, error) {
	client := apikeyconnect.NewAPIKeyAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
	req := &apikeyv1.IssueKeyRequest{
		OrgId:  orgID,
		UserId: userID,
		Scopes: scopes,
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

// parseScopes splits a comma-separated scope string.
// Falls back to DefaultUserScopes when empty.
func parseScopes(s string) []string {
	if s == "" {
		return apikeys.DefaultUserScopes
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return apikeys.DefaultUserScopes
	}
	return out
}

// printAPIKeys renders key records. plaintext is non-empty only on issue.
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
			rows[i] = fmt.Sprintf("%s\t%s\t%s\t%s",
				keyCol, strings.Join(k.GetScopes(), ","), k.GetUserId(), age(k.GetCreatedAt()))
		}
		header := "KEY-PREFIX\tSCOPES\tUSER-ID\tAGE"
		if plaintext != "" {
			header = "KEY (shown once)\tSCOPES\tUSER-ID\tAGE"
		}
		p.Table(header, rows)
		return nil
	}
}
