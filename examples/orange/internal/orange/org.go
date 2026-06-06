package orange

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	orgconnect "github.com/dio/transit/examples/orange/api/orange/org/admin/v1/adminv1connect"
	orgv1 "github.com/dio/transit/examples/orange/api/orange/org/admin/v1"
)

func newOrgCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "org",
		Aliases: []string{"organization"},
		Short:   "Manage orgs",
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
			return rc.Printer.Proto(resp.Msg.GetOrg())
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
		RunE: func(cmd *cobra.Command, _ []string) error {
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
			return rc.Printer.Proto(resp.Msg.GetOrg())
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
			items := make([]proto.Message, len(resp.Msg.GetOrgs()))
			for i, o := range resp.Msg.GetOrgs() {
				items[i] = o
			}
			return rc.Printer.ProtoList(items)
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
			return rc.Printer.Proto(resp.Msg.GetOrg())
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
