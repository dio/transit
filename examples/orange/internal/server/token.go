package server

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	keyentryv1 "github.com/dio/transit/examples/orange/api/orange/keyentry/admin/v1"
	keyentryconnect "github.com/dio/transit/examples/orange/api/orange/keyentry/admin/v1/adminv1connect"
)

func newTokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage tokens",
	}
	cmd.AddCommand(newTokenCreateCmd())
	return cmd
}

func newTokenCreateCmd() *cobra.Command {
	var name, description string
	var ttl int64
	cmd := &cobra.Command{
		Use:          "create",
		Short:        "Issue a token (workspace and user derived from the API key)",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}

			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}

			req := &keyentryv1.IssueNamedTokenRequest{
				Name:       name,
				TtlSeconds: ttl,
			}
			if description != "" {
				req.Description = &description
			}

			client := keyentryconnect.NewKeyEntryAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.IssueNamedToken(context.Background(), connect.NewRequest(req))
			if err != nil {
				return err
			}

			return printToken(rc.Printer, resp.Msg.GetToken(), resp.Msg.GetMetadata())
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "token slot name (e.g. default, batch)")
	cmd.Flags().StringVar(&description, "description", "", "optional description")
	cmd.Flags().Int64Var(&ttl, "ttl", 0, "token TTL in seconds (0 = no expiry)")
	return cmd
}

func printToken(p *Printer, plaintext string, tok *keyentryv1.PASETOToken) error {
	switch p.Format {
	case FormatJSON, FormatYAML:
		return p.Proto(tok)
	default:
		expires := "never"
		if exp := tok.GetExp(); exp != nil && exp.AsTime().Year() < 9999 {
			expires = ttlString(exp.AsTime())
		}
		header := "TOKEN (shown once)\tSLOT\tEXPIRES"
		row := fmt.Sprintf("%s\t%s\t%s", plaintext, tok.GetKeyEntryId(), expires)
		p.Table(header, []string{row})
		return nil
	}
}

func ttlString(t time.Time) string {
	d := time.Until(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
