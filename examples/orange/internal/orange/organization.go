package orange

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	orgv1 "github.com/dio/transit/examples/orange/api/orange/org/admin/v1"
	orgconnect "github.com/dio/transit/examples/orange/api/orange/org/admin/v1/adminv1connect"
)

func newOrgCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "organization",
		Aliases: []string{"org"},
		Short:   "Manage organizations",
	}
	cmd.AddCommand(newOrgCreateCmd())
	cmd.AddCommand(newOrgGetCmd())
	cmd.AddCommand(newOrgListCmd())
	cmd.AddCommand(newOrgUpdateCmd())
	cmd.AddCommand(newOrgDeleteCmd())
	return cmd
}

func newOrgCreateCmd() *cobra.Command {
	var name, description string
	cmd := &cobra.Command{
		Use:          "create",
		Short:        "Create an org",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := orgconnect.NewOrgAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			req := &orgv1.CreateOrgRequest{Name: name}
			if cmd.Flags().Changed("description") {
				req.Description = &description
			}
			resp, err := client.CreateOrg(context.Background(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			return printOrgs(rc.Printer, resp.Msg.GetOrg())
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "org name")
	cmd.Flags().StringVar(&description, "description", "", "org description")
	return cmd
}

func newOrgGetCmd() *cobra.Command {
	var orgID string
	cmd := &cobra.Command{
		Use:          "get",
		Short:        "Get an org",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if orgID == "" {
				return fmt.Errorf("--org-id is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := orgconnect.NewOrgAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.GetOrg(context.Background(), connect.NewRequest(&orgv1.GetOrgRequest{OrgId: orgID}))
			if err != nil {
				return err
			}
			return printOrgs(rc.Printer, resp.Msg.GetOrg())
		},
	}
	cmd.Flags().StringVar(&orgID, "org-id", "", "org ID")
	return cmd
}

func newOrgListCmd() *cobra.Command {
	var limit int32
	var pageToken string
	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List orgs",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := orgconnect.NewOrgAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			req := &orgv1.ListOrgsRequest{}
			if cmd.Flags().Changed("limit") {
				req.Limit = &limit
			}
			if cmd.Flags().Changed("page-token") {
				req.PageToken = &pageToken
			}
			resp, err := client.ListOrgs(context.Background(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			return printOrgs(rc.Printer, resp.Msg.GetOrgs()...)
		},
	}
	cmd.Flags().Int32Var(&limit, "limit", 50, "max results")
	cmd.Flags().StringVar(&pageToken, "page-token", "", "pagination token")
	return cmd
}

func newOrgUpdateCmd() *cobra.Command {
	var orgID, description string
	cmd := &cobra.Command{
		Use:          "update",
		Short:        "Update an org",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if orgID == "" {
				return fmt.Errorf("--org-id is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := orgconnect.NewOrgAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			req := &orgv1.UpdateOrgRequest{OrgId: orgID}
			if cmd.Flags().Changed("description") {
				req.Description = &description
			}
			resp, err := client.UpdateOrg(context.Background(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			return printOrgs(rc.Printer, resp.Msg.GetOrg())
		},
	}
	cmd.Flags().StringVar(&orgID, "org-id", "", "org ID")
	cmd.Flags().StringVar(&description, "description", "", "new description")
	return cmd
}

func newOrgDeleteCmd() *cobra.Command {
	var orgID string
	cmd := &cobra.Command{
		Use:          "delete",
		Short:        "Delete an org",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if orgID == "" {
				return fmt.Errorf("--org-id is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := orgconnect.NewOrgAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			_, err = client.DeleteOrg(context.Background(), connect.NewRequest(&orgv1.DeleteOrgRequest{OrgId: orgID}))
			if err != nil {
				return err
			}
			rc.Printer.OK("deleted")
			return nil
		},
	}
	cmd.Flags().StringVar(&orgID, "org-id", "", "org ID")
	return cmd
}

func printOrgs(p *Printer, orgs ...*orgv1.Org) error {
	switch p.Format {
	case FormatJSON, FormatYAML:
		msgs := make([]proto.Message, len(orgs))
		for i, o := range orgs {
			msgs[i] = o
		}
		if len(msgs) == 1 {
			return p.Proto(msgs[0])
		}
		return p.ProtoList(msgs)
	default:
		rows := make([]string, len(orgs))
		for i, o := range orgs {
			rows[i] = fmt.Sprintf("%s\t%s\t%s\t%s",
				o.GetName(), o.GetOrgId(), clip(o.Description, 40), age(o.GetCreatedAt()))
		}
		p.Table("NAME\tID\tDESCRIPTION\tAGE", rows)
		return nil
	}
}
