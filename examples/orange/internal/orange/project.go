package orange

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	projectconnect "github.com/dio/transit/examples/orange/api/orange/project/admin/v1/adminv1connect"
	projectv1 "github.com/dio/transit/examples/orange/api/orange/project/admin/v1"
)

func newProjectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "project",
		Aliases: []string{"proj"},
		Short:   "Manage projects",
	}
	cmd.AddCommand(newProjectCreateCmd())
	cmd.AddCommand(newProjectGetCmd())
	cmd.AddCommand(newProjectListCmd())
	cmd.AddCommand(newProjectUpdateCmd())
	cmd.AddCommand(newProjectDeleteCmd())
	return cmd
}

func newProjectCreateCmd() *cobra.Command {
	var orgID, name, description string
	cmd := &cobra.Command{
		Use:          "create",
		Short:        "Create a project",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if orgID == "" {
				return fmt.Errorf("--org-id is required")
			}
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := projectconnect.NewProjectAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			req := &projectv1.CreateProjectRequest{OrgId: orgID, Name: name}
			if cmd.Flags().Changed("description") {
				req.Description = &description
			}
			resp, err := client.CreateProject(context.Background(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			return rc.Printer.Proto(resp.Msg.GetProject())
		},
	}
	cmd.Flags().StringVar(&orgID, "org-id", "", "org ID")
	cmd.Flags().StringVar(&name, "name", "", "project name")
	cmd.Flags().StringVar(&description, "description", "", "project description")
	return cmd
}

func newProjectGetCmd() *cobra.Command {
	var projectID string
	cmd := &cobra.Command{
		Use:          "get",
		Short:        "Get a project",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if projectID == "" {
				return fmt.Errorf("--project-id is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := projectconnect.NewProjectAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.GetProject(context.Background(), connect.NewRequest(&projectv1.GetProjectRequest{ProjectId: projectID}))
			if err != nil {
				return err
			}
			return rc.Printer.Proto(resp.Msg.GetProject())
		},
	}
	cmd.Flags().StringVar(&projectID, "project-id", "", "project ID")
	return cmd
}

func newProjectListCmd() *cobra.Command {
	var orgID, pageToken string
	var limit int32
	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List projects",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if orgID == "" {
				return fmt.Errorf("--org-id is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := projectconnect.NewProjectAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			req := &projectv1.ListProjectsRequest{OrgId: orgID}
			if cmd.Flags().Changed("limit") {
				req.Limit = &limit
			}
			if cmd.Flags().Changed("page-token") {
				req.PageToken = &pageToken
			}
			resp, err := client.ListProjects(context.Background(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			items := make([]proto.Message, len(resp.Msg.GetProjects()))
			for i, p := range resp.Msg.GetProjects() {
				items[i] = p
			}
			return rc.Printer.ProtoList(items)
		},
	}
	cmd.Flags().StringVar(&orgID, "org-id", "", "org ID")
	cmd.Flags().Int32Var(&limit, "limit", 50, "max results")
	cmd.Flags().StringVar(&pageToken, "page-token", "", "pagination token")
	return cmd
}

func newProjectUpdateCmd() *cobra.Command {
	var projectID, description string
	cmd := &cobra.Command{
		Use:          "update",
		Short:        "Update a project",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if projectID == "" {
				return fmt.Errorf("--project-id is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := projectconnect.NewProjectAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			req := &projectv1.UpdateProjectRequest{ProjectId: projectID}
			if cmd.Flags().Changed("description") {
				req.Description = &description
			}
			resp, err := client.UpdateProject(context.Background(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			return rc.Printer.Proto(resp.Msg.GetProject())
		},
	}
	cmd.Flags().StringVar(&projectID, "project-id", "", "project ID")
	cmd.Flags().StringVar(&description, "description", "", "new description")
	return cmd
}

func newProjectDeleteCmd() *cobra.Command {
	var projectID string
	cmd := &cobra.Command{
		Use:          "delete",
		Short:        "Delete a project",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if projectID == "" {
				return fmt.Errorf("--project-id is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := projectconnect.NewProjectAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			_, err = client.DeleteProject(context.Background(), connect.NewRequest(&projectv1.DeleteProjectRequest{ProjectId: projectID}))
			if err != nil {
				return err
			}
			rc.Printer.OK("deleted")
			return nil
		},
	}
	cmd.Flags().StringVar(&projectID, "project-id", "", "project ID")
	return cmd
}
