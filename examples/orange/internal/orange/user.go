package orange

import (
	"context"
	"fmt"
	"os"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	userv1 "github.com/dio/transit/examples/orange/api/orange/user/admin/v1"
	userconnect "github.com/dio/transit/examples/orange/api/orange/user/admin/v1/adminv1connect"
)

// ── orange user ───────────────────────────────────────────────────────────────

func newUserCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "user",
		Aliases: []string{"usr"},
		Short:   "Manage users",
	}
	cmd.AddCommand(newUserCreateCmd())
	cmd.AddCommand(newUserGetCmd())
	cmd.AddCommand(newUserListCmd())
	cmd.AddCommand(newUserUpdateCmd())
	cmd.AddCommand(newUserDeleteCmd())
	return cmd
}

func newUserCreateCmd() *cobra.Command {
	var orgID, email, description, scopeFlag string
	cmd := &cobra.Command{
		Use:          "create",
		Short:        "Create a user and issue an API key",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if orgID == "" {
				orgID = os.Getenv("ORANGE_ORG_ID")
			}
			if orgID == "" {
				return fmt.Errorf("--org-id is required (or set ORANGE_ORG_ID)")
			}
			if email == "" {
				return fmt.Errorf("--email is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := userconnect.NewUserAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			req := &userv1.CreateUserRequest{OrgId: orgID, Email: email}
			if cmd.Flags().Changed("description") {
				req.Description = &description
			}
			resp, err := client.CreateUser(context.Background(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			user := resp.Msg.GetUser()
			if err := printUsers(rc.Printer, user); err != nil {
				return err
			}
			keyProto, plaintext, err := issueAPIKey(rc, orgID, user.GetUserId(), parseScopes(scopeFlag), "auto-issued for "+email)
			if err != nil {
				return fmt.Errorf("user created but key issuance failed: %w", err)
			}
			return printAPIKeys(rc.Printer, plaintext, keyProto)
		},
	}
	cmd.Flags().StringVar(&orgID, "org-id", "", "org ID (env: ORANGE_ORG_ID)")
	cmd.Flags().StringVar(&email, "email", "", "user email address")
	cmd.Flags().StringVar(&description, "description", "", "user description")
	cmd.Flags().StringVar(&scopeFlag, "scope", "read,write", "comma-separated scopes: read, write, admin, proxy, user, token:issue, egress-bundle:download")
	return cmd
}

func newUserGetCmd() *cobra.Command {
	var userID string
	cmd := &cobra.Command{
		Use:          "get",
		Short:        "Get a user",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if userID == "" {
				userID = os.Getenv("ORANGE_USER_ID")
			}
			if userID == "" {
				return fmt.Errorf("--user-id is required (or set ORANGE_USER_ID)")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := userconnect.NewUserAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.GetUser(context.Background(), connect.NewRequest(&userv1.GetUserRequest{UserId: userID}))
			if err != nil {
				return err
			}
			return printUsers(rc.Printer, resp.Msg.GetUser())
		},
	}
	cmd.Flags().StringVar(&userID, "user-id", "", "user ID (env: ORANGE_USER_ID)")
	return cmd
}

func newUserListCmd() *cobra.Command {
	var orgID, pageToken string
	var limit int32
	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List users in an org",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
			client := userconnect.NewUserAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			req := &userv1.ListUsersRequest{OrgId: orgID}
			if cmd.Flags().Changed("limit") {
				req.Limit = &limit
			}
			if cmd.Flags().Changed("page-token") {
				req.PageToken = &pageToken
			}
			resp, err := client.ListUsers(context.Background(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			return printUsers(rc.Printer, resp.Msg.GetUsers()...)
		},
	}
	cmd.Flags().StringVar(&orgID, "org-id", "", "org ID (env: ORANGE_ORG_ID)")
	cmd.Flags().Int32Var(&limit, "limit", 50, "max results")
	cmd.Flags().StringVar(&pageToken, "page-token", "", "pagination token")
	return cmd
}

func newUserUpdateCmd() *cobra.Command {
	var userID, description string
	cmd := &cobra.Command{
		Use:          "update",
		Short:        "Update a user",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if userID == "" {
				userID = os.Getenv("ORANGE_USER_ID")
			}
			if userID == "" {
				return fmt.Errorf("--user-id is required (or set ORANGE_USER_ID)")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := userconnect.NewUserAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			req := &userv1.UpdateUserRequest{UserId: userID}
			if cmd.Flags().Changed("description") {
				req.Description = &description
			}
			resp, err := client.UpdateUser(context.Background(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			return printUsers(rc.Printer, resp.Msg.GetUser())
		},
	}
	cmd.Flags().StringVar(&userID, "user-id", "", "user ID (env: ORANGE_USER_ID)")
	cmd.Flags().StringVar(&description, "description", "", "new description")
	return cmd
}

func newUserDeleteCmd() *cobra.Command {
	var userID string
	cmd := &cobra.Command{
		Use:          "delete",
		Short:        "Delete a user",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if userID == "" {
				userID = os.Getenv("ORANGE_USER_ID")
			}
			if userID == "" {
				return fmt.Errorf("--user-id is required (or set ORANGE_USER_ID)")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := userconnect.NewUserAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			_, err = client.DeleteUser(context.Background(), connect.NewRequest(&userv1.DeleteUserRequest{UserId: userID}))
			if err != nil {
				return err
			}
			rc.Printer.OK("deleted")
			return nil
		},
	}
	cmd.Flags().StringVar(&userID, "user-id", "", "user ID (env: ORANGE_USER_ID)")
	return cmd
}

// ── orange member ─────────────────────────────────────────────────────────────

func newMemberCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "member",
		Aliases: []string{"mbr"},
		Short:   "Manage workspace members",
	}
	cmd.AddCommand(newMemberAddCmd())
	cmd.AddCommand(newMemberRemoveCmd())
	cmd.AddCommand(newMemberListCmd())
	return cmd
}

func newMemberAddCmd() *cobra.Command {
	var workspaceID, userID string
	cmd := &cobra.Command{
		Use:          "add",
		Short:        "Add a user to a workspace",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if workspaceID == "" {
				workspaceID = os.Getenv("ORANGE_WS_ID")
			}
			if workspaceID == "" {
				return fmt.Errorf("--workspace-id is required (or set ORANGE_WS_ID)")
			}
			if userID == "" {
				userID = os.Getenv("ORANGE_USER_ID")
			}
			if userID == "" {
				return fmt.Errorf("--user-id is required (or set ORANGE_USER_ID)")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := userconnect.NewUserAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.AddWorkspaceMember(context.Background(), connect.NewRequest(&userv1.AddWorkspaceMemberRequest{
				WorkspaceId: workspaceID,
				UserId:      userID,
			}))
			if err != nil {
				return err
			}
			return printMembers(rc.Printer, resp.Msg.GetMember())
		},
	}
	cmd.Flags().StringVar(&workspaceID, "workspace-id", "", "workspace ID (env: ORANGE_WS_ID)")
	cmd.Flags().StringVar(&userID, "user-id", "", "user ID (env: ORANGE_USER_ID)")
	return cmd
}

func newMemberRemoveCmd() *cobra.Command {
	var workspaceID, userID string
	cmd := &cobra.Command{
		Use:          "remove",
		Short:        "Remove a user from a workspace",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if workspaceID == "" {
				workspaceID = os.Getenv("ORANGE_WS_ID")
			}
			if workspaceID == "" {
				return fmt.Errorf("--workspace-id is required (or set ORANGE_WS_ID)")
			}
			if userID == "" {
				userID = os.Getenv("ORANGE_USER_ID")
			}
			if userID == "" {
				return fmt.Errorf("--user-id is required (or set ORANGE_USER_ID)")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := userconnect.NewUserAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			_, err = client.RemoveWorkspaceMember(context.Background(), connect.NewRequest(&userv1.RemoveWorkspaceMemberRequest{
				WorkspaceId: workspaceID,
				UserId:      userID,
			}))
			if err != nil {
				return err
			}
			rc.Printer.OK("removed")
			return nil
		},
	}
	cmd.Flags().StringVar(&workspaceID, "workspace-id", "", "workspace ID (env: ORANGE_WS_ID)")
	cmd.Flags().StringVar(&userID, "user-id", "", "user ID (env: ORANGE_USER_ID)")
	return cmd
}

func newMemberListCmd() *cobra.Command {
	var workspaceID, pageToken string
	var limit int32
	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List workspace members",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if workspaceID == "" {
				workspaceID = os.Getenv("ORANGE_WS_ID")
			}
			if workspaceID == "" {
				return fmt.Errorf("--workspace-id is required (or set ORANGE_WS_ID)")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := userconnect.NewUserAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			req := &userv1.ListWorkspaceMembersRequest{WorkspaceId: workspaceID}
			if cmd.Flags().Changed("limit") {
				req.Limit = &limit
			}
			if cmd.Flags().Changed("page-token") {
				req.PageToken = &pageToken
			}
			resp, err := client.ListWorkspaceMembers(context.Background(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			return printMembers(rc.Printer, resp.Msg.GetMembers()...)
		},
	}
	cmd.Flags().StringVar(&workspaceID, "workspace-id", "", "workspace ID (env: ORANGE_WS_ID)")
	cmd.Flags().Int32Var(&limit, "limit", 50, "max results")
	cmd.Flags().StringVar(&pageToken, "page-token", "", "pagination token")
	return cmd
}

// ── table helpers ─────────────────────────────────────────────────────────────

func printUsers(p *Printer, users ...*userv1.User) error {
	switch p.Format {
	case FormatJSON, FormatYAML:
		msgs := make([]proto.Message, len(users))
		for i, u := range users {
			msgs[i] = u
		}
		if len(msgs) == 1 {
			return p.Proto(msgs[0])
		}
		return p.ProtoList(msgs)
	default:
		rows := make([]string, len(users))
		for i, u := range users {
			rows[i] = fmt.Sprintf("%s\t%s\t%s\t%s",
				u.GetEmail(), u.GetUserId(), u.GetOrgId(), age(u.GetCreatedAt()))
		}
		p.Table("EMAIL\tUSER-ID\tORG-ID\tAGE", rows)
		return nil
	}
}

func printMembers(p *Printer, members ...*userv1.WorkspaceMember) error {
	switch p.Format {
	case FormatJSON, FormatYAML:
		msgs := make([]proto.Message, len(members))
		for i, m := range members {
			msgs[i] = m
		}
		if len(msgs) == 1 {
			return p.Proto(msgs[0])
		}
		return p.ProtoList(msgs)
	default:
		rows := make([]string, len(members))
		for i, m := range members {
			rows[i] = fmt.Sprintf("%s\t%s\t%s",
				m.GetUserId(), m.GetWorkspaceId(), age(m.GetJoinedAt()))
		}
		p.Table("USER-ID\tWORKSPACE-ID\tAGE", rows)
		return nil
	}
}
