package orange

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	workspaceconnect "github.com/dio/transit/examples/orange/api/orange/workspace/admin/v1/adminv1connect"
	workspacev1 "github.com/dio/transit/examples/orange/api/orange/workspace/admin/v1"
)

func newWorkspaceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "workspace",
		Aliases: []string{"ws"},
		Short:   "Manage workspaces",
	}
	cmd.AddCommand(newWorkspaceCreateCmd())
	cmd.AddCommand(newWorkspaceGetCmd())
	cmd.AddCommand(newWorkspaceListCmd())
	cmd.AddCommand(newWorkspaceUpdateCmd())
	cmd.AddCommand(newWorkspaceDeleteCmd())
	return cmd
}

func newWorkspaceCreateCmd() *cobra.Command {
	var projectID, name, description string
	cmd := &cobra.Command{
		Use:          "create",
		Short:        "Create a workspace",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if projectID == "" {
				return fmt.Errorf("--project-id is required")
			}
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := workspaceconnect.NewWorkspaceAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			req := &workspacev1.CreateWorkspaceRequest{ProjectId: projectID, Name: name}
			if cmd.Flags().Changed("description") {
				req.Description = &description
			}
			resp, err := client.CreateWorkspace(context.Background(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			return rc.Printer.Proto(resp.Msg.GetWorkspace())
		},
	}
	cmd.Flags().StringVar(&projectID, "project-id", "", "project ID")
	cmd.Flags().StringVar(&name, "name", "", "workspace name")
	cmd.Flags().StringVar(&description, "description", "", "workspace description")
	return cmd
}

func newWorkspaceGetCmd() *cobra.Command {
	var workspaceID string
	cmd := &cobra.Command{
		Use:          "get",
		Short:        "Get a workspace",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if workspaceID == "" {
				return fmt.Errorf("--workspace-id is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := workspaceconnect.NewWorkspaceAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.GetWorkspace(context.Background(), connect.NewRequest(&workspacev1.GetWorkspaceRequest{WorkspaceId: workspaceID}))
			if err != nil {
				return err
			}
			return rc.Printer.Proto(resp.Msg.GetWorkspace())
		},
	}
	cmd.Flags().StringVar(&workspaceID, "workspace-id", "", "workspace ID")
	return cmd
}

func newWorkspaceListCmd() *cobra.Command {
	var projectID, pageToken string
	var limit int32
	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List workspaces",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if projectID == "" {
				return fmt.Errorf("--project-id is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := workspaceconnect.NewWorkspaceAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			req := &workspacev1.ListWorkspacesRequest{ProjectId: projectID}
			if cmd.Flags().Changed("limit") {
				req.Limit = &limit
			}
			if cmd.Flags().Changed("page-token") {
				req.PageToken = &pageToken
			}
			resp, err := client.ListWorkspaces(context.Background(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			items := make([]proto.Message, len(resp.Msg.GetWorkspaces()))
			for i, w := range resp.Msg.GetWorkspaces() {
				items[i] = w
			}
			return rc.Printer.ProtoList(items)
		},
	}
	cmd.Flags().StringVar(&projectID, "project-id", "", "project ID")
	cmd.Flags().Int32Var(&limit, "limit", 50, "max results")
	cmd.Flags().StringVar(&pageToken, "page-token", "", "pagination token")
	return cmd
}

func newWorkspaceUpdateCmd() *cobra.Command {
	var workspaceID, description string
	cmd := &cobra.Command{
		Use:          "update",
		Short:        "Update a workspace",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if workspaceID == "" {
				return fmt.Errorf("--workspace-id is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := workspaceconnect.NewWorkspaceAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			req := &workspacev1.UpdateWorkspaceRequest{WorkspaceId: workspaceID}
			if cmd.Flags().Changed("description") {
				req.Description = &description
			}
			resp, err := client.UpdateWorkspace(context.Background(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			return rc.Printer.Proto(resp.Msg.GetWorkspace())
		},
	}
	cmd.Flags().StringVar(&workspaceID, "workspace-id", "", "workspace ID")
	cmd.Flags().StringVar(&description, "description", "", "new description")
	return cmd
}

func newWorkspaceDeleteCmd() *cobra.Command {
	var workspaceID string
	cmd := &cobra.Command{
		Use:          "delete",
		Short:        "Delete a workspace",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if workspaceID == "" {
				return fmt.Errorf("--workspace-id is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := workspaceconnect.NewWorkspaceAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			_, err = client.DeleteWorkspace(context.Background(), connect.NewRequest(&workspacev1.DeleteWorkspaceRequest{WorkspaceId: workspaceID}))
			if err != nil {
				return err
			}
			rc.Printer.OK("deleted")
			return nil
		},
	}
	cmd.Flags().StringVar(&workspaceID, "workspace-id", "", "workspace ID")
	return cmd
}
