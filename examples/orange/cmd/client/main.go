// orangectl is the admin CLI for the orange management plane.
//
// Usage:
//
//	orangectl [--server URL] [--api-key KEY] <resource> <verb> [flags]
//
// Examples (operations A1–A6 from orange-ops/06-admin-operations.md):
//
//	orangectl org create     --name acme
//	orangectl project create --org-id <id> --name project1
//	orangectl workspace create --project-id <id> --name workspace1
//	orangectl user create    --org-id <id> --email alice@acme.com
//	orangectl member add     --workspace-id <id> --user-id <id>
//	orangectl member remove  --workspace-id <id> --user-id <id>
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	orgv1 "github.com/dio/transit/examples/orange/api/orange/org/admin/v1"
	orgconnect "github.com/dio/transit/examples/orange/api/orange/org/admin/v1/adminv1connect"
	projectv1 "github.com/dio/transit/examples/orange/api/orange/project/admin/v1"
	projectconnect "github.com/dio/transit/examples/orange/api/orange/project/admin/v1/adminv1connect"
	userv1 "github.com/dio/transit/examples/orange/api/orange/user/admin/v1"
	userconnect "github.com/dio/transit/examples/orange/api/orange/user/admin/v1/adminv1connect"
	workspacev1 "github.com/dio/transit/examples/orange/api/orange/workspace/admin/v1"
	workspaceconnect "github.com/dio/transit/examples/orange/api/orange/workspace/admin/v1/adminv1connect"
	"github.com/dio/transit/examples/orange/internal/vtprotocodec"
)

const defaultServer = "http://localhost:8080"

func main() {
	serverURL := flag.String("server", envOr("ORANGE_SERVER", defaultServer), "management plane server URL")
	apiKey := flag.String("api-key", envOr("ORANGE_API_KEY", ""), "admin API key (env: ORANGE_API_KEY)")
	flag.Usage = usage
	flag.Parse()

	args := flag.Args()
	if len(args) < 2 {
		usage()
		os.Exit(1)
	}

	if *apiKey == "" {
		fmt.Fprintln(os.Stderr, "error: --api-key is required (or set ORANGE_API_KEY)")
		os.Exit(1)
	}

	resource, verb := args[0], args[1]
	rest := args[2:]

	httpClient := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &bearerTransport{key: *apiKey, base: http.DefaultTransport},
	}
	opts := []connect.ClientOption{connect.WithCodec(vtprotocodec.Codec{})}
	ctx := context.Background()

	if err := dispatch(ctx, *serverURL, httpClient, opts, resource+" "+verb, rest); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// bearerTransport injects Authorization: Bearer on every request.
type bearerTransport struct {
	key  string
	base http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.key)
	return t.base.RoundTrip(clone)
}

func dispatch(ctx context.Context, serverURL string, httpClient *http.Client, opts []connect.ClientOption, cmd string, args []string) error {
	switch cmd {

	// ── A1: Create Org ────────────────────────────────────────────────────────
	case "org create":
		fs := flag.NewFlagSet("org create", flag.ExitOnError)
		name := fs.String("name", "", "unique slug e.g. acme (required)")
		description := fs.String("description", "", "optional free-text annotation")
		_ = fs.Parse(args)
		requireFlag(*name, "--name", fs)
		req := &orgv1.CreateOrgRequest{Name: *name}
		if *description != "" {
			req.Description = description
		}
		client := orgconnect.NewOrgAdminServiceClient(httpClient, serverURL, opts...)
		resp, err := client.CreateOrg(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		return print(resp.Msg.Org)

	case "org get":
		fs := flag.NewFlagSet("org get", flag.ExitOnError)
		orgID := fs.String("org-id", "", "org UUID7 (required)")
		_ = fs.Parse(args)
		requireFlag(*orgID, "--org-id", fs)
		client := orgconnect.NewOrgAdminServiceClient(httpClient, serverURL, opts...)
		resp, err := client.GetOrg(ctx, connect.NewRequest(&orgv1.GetOrgRequest{OrgId: *orgID}))
		if err != nil {
			return err
		}
		return print(resp.Msg.Org)

	case "org list":
		fs := flag.NewFlagSet("org list", flag.ExitOnError)
		limit := fs.Int("limit", 50, "max results")
		_ = fs.Parse(args)
		lim := int32(*limit)
		client := orgconnect.NewOrgAdminServiceClient(httpClient, serverURL, opts...)
		resp, err := client.ListOrgs(ctx, connect.NewRequest(&orgv1.ListOrgsRequest{Limit: &lim}))
		if err != nil {
			return err
		}
		return print(resp.Msg)

	// ── A1: Create Project ────────────────────────────────────────────────────
	case "project create":
		fs := flag.NewFlagSet("project create", flag.ExitOnError)
		orgID := fs.String("org-id", "", "org UUID7 (required)")
		name := fs.String("name", "", "slug e.g. project1 (required)")
		description := fs.String("description", "", "optional description")
		_ = fs.Parse(args)
		requireFlag(*orgID, "--org-id", fs)
		requireFlag(*name, "--name", fs)
		req := &projectv1.CreateProjectRequest{OrgId: *orgID, Name: *name}
		if *description != "" {
			req.Description = description
		}
		client := projectconnect.NewProjectAdminServiceClient(httpClient, serverURL, opts...)
		resp, err := client.CreateProject(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		return print(resp.Msg.Project)

	case "project get":
		fs := flag.NewFlagSet("project get", flag.ExitOnError)
		projectID := fs.String("project-id", "", "project UUID7 (required)")
		_ = fs.Parse(args)
		requireFlag(*projectID, "--project-id", fs)
		client := projectconnect.NewProjectAdminServiceClient(httpClient, serverURL, opts...)
		resp, err := client.GetProject(ctx, connect.NewRequest(&projectv1.GetProjectRequest{ProjectId: *projectID}))
		if err != nil {
			return err
		}
		return print(resp.Msg.Project)

	case "project list":
		fs := flag.NewFlagSet("project list", flag.ExitOnError)
		orgID := fs.String("org-id", "", "org UUID7 (required)")
		limit := fs.Int("limit", 50, "max results")
		_ = fs.Parse(args)
		requireFlag(*orgID, "--org-id", fs)
		lim := int32(*limit)
		client := projectconnect.NewProjectAdminServiceClient(httpClient, serverURL, opts...)
		resp, err := client.ListProjects(ctx, connect.NewRequest(&projectv1.ListProjectsRequest{
			OrgId: *orgID, Limit: &lim,
		}))
		if err != nil {
			return err
		}
		return print(resp.Msg)

	// ── A2: Create Workspace ──────────────────────────────────────────────────
	case "workspace create":
		fs := flag.NewFlagSet("workspace create", flag.ExitOnError)
		projectID := fs.String("project-id", "", "project UUID7 (required)")
		name := fs.String("name", "", "slug e.g. workspace1 (required)")
		description := fs.String("description", "", "optional description")
		_ = fs.Parse(args)
		requireFlag(*projectID, "--project-id", fs)
		requireFlag(*name, "--name", fs)
		req := &workspacev1.CreateWorkspaceRequest{ProjectId: *projectID, Name: *name}
		if *description != "" {
			req.Description = description
		}
		client := workspaceconnect.NewWorkspaceAdminServiceClient(httpClient, serverURL, opts...)
		resp, err := client.CreateWorkspace(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		return print(resp.Msg.Workspace)

	case "workspace get":
		fs := flag.NewFlagSet("workspace get", flag.ExitOnError)
		workspaceID := fs.String("workspace-id", "", "workspace UUID7 (required)")
		_ = fs.Parse(args)
		requireFlag(*workspaceID, "--workspace-id", fs)
		client := workspaceconnect.NewWorkspaceAdminServiceClient(httpClient, serverURL, opts...)
		resp, err := client.GetWorkspace(ctx, connect.NewRequest(&workspacev1.GetWorkspaceRequest{WorkspaceId: *workspaceID}))
		if err != nil {
			return err
		}
		return print(resp.Msg.Workspace)

	case "workspace list":
		fs := flag.NewFlagSet("workspace list", flag.ExitOnError)
		projectID := fs.String("project-id", "", "project UUID7 (required)")
		limit := fs.Int("limit", 50, "max results")
		_ = fs.Parse(args)
		requireFlag(*projectID, "--project-id", fs)
		lim := int32(*limit)
		client := workspaceconnect.NewWorkspaceAdminServiceClient(httpClient, serverURL, opts...)
		resp, err := client.ListWorkspaces(ctx, connect.NewRequest(&workspacev1.ListWorkspacesRequest{
			ProjectId: *projectID, Limit: &lim,
		}))
		if err != nil {
			return err
		}
		return print(resp.Msg)

	// ── User management ───────────────────────────────────────────────────────
	case "user create":
		fs := flag.NewFlagSet("user create", flag.ExitOnError)
		orgID := fs.String("org-id", "", "org UUID7 (required)")
		email := fs.String("email", "", "email address (required)")
		description := fs.String("description", "", "optional description")
		_ = fs.Parse(args)
		requireFlag(*orgID, "--org-id", fs)
		requireFlag(*email, "--email", fs)
		req := &userv1.CreateUserRequest{OrgId: *orgID, Email: *email}
		if *description != "" {
			req.Description = description
		}
		client := userconnect.NewUserAdminServiceClient(httpClient, serverURL, opts...)
		resp, err := client.CreateUser(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		return print(resp.Msg.User)

	case "user get":
		fs := flag.NewFlagSet("user get", flag.ExitOnError)
		userID := fs.String("user-id", "", "user UUID7 (required)")
		_ = fs.Parse(args)
		requireFlag(*userID, "--user-id", fs)
		client := userconnect.NewUserAdminServiceClient(httpClient, serverURL, opts...)
		resp, err := client.GetUser(ctx, connect.NewRequest(&userv1.GetUserRequest{UserId: *userID}))
		if err != nil {
			return err
		}
		return print(resp.Msg.User)

	case "user list":
		fs := flag.NewFlagSet("user list", flag.ExitOnError)
		orgID := fs.String("org-id", "", "org UUID7 (required)")
		limit := fs.Int("limit", 50, "max results")
		_ = fs.Parse(args)
		requireFlag(*orgID, "--org-id", fs)
		lim := int32(*limit)
		client := userconnect.NewUserAdminServiceClient(httpClient, serverURL, opts...)
		resp, err := client.ListUsers(ctx, connect.NewRequest(&userv1.ListUsersRequest{
			OrgId: *orgID, Limit: &lim,
		}))
		if err != nil {
			return err
		}
		return print(resp.Msg)

	// ── A5: Add member / A6: Remove member ───────────────────────────────────
	case "member add":
		fs := flag.NewFlagSet("member add", flag.ExitOnError)
		workspaceID := fs.String("workspace-id", "", "workspace UUID7 (required)")
		userID := fs.String("user-id", "", "user UUID7 (required)")
		_ = fs.Parse(args)
		requireFlag(*workspaceID, "--workspace-id", fs)
		requireFlag(*userID, "--user-id", fs)
		client := userconnect.NewUserAdminServiceClient(httpClient, serverURL, opts...)
		resp, err := client.AddWorkspaceMember(ctx, connect.NewRequest(&userv1.AddWorkspaceMemberRequest{
			WorkspaceId: *workspaceID, UserId: *userID,
		}))
		if err != nil {
			return err
		}
		return print(resp.Msg.Member)

	case "member remove":
		fs := flag.NewFlagSet("member remove", flag.ExitOnError)
		workspaceID := fs.String("workspace-id", "", "workspace UUID7 (required)")
		userID := fs.String("user-id", "", "user UUID7 (required)")
		_ = fs.Parse(args)
		requireFlag(*workspaceID, "--workspace-id", fs)
		requireFlag(*userID, "--user-id", fs)
		client := userconnect.NewUserAdminServiceClient(httpClient, serverURL, opts...)
		_, err := client.RemoveWorkspaceMember(ctx, connect.NewRequest(&userv1.RemoveWorkspaceMemberRequest{
			WorkspaceId: *workspaceID, UserId: *userID,
		}))
		if err != nil {
			return err
		}
		fmt.Println("ok")
		return nil

	case "member list":
		fs := flag.NewFlagSet("member list", flag.ExitOnError)
		workspaceID := fs.String("workspace-id", "", "workspace UUID7 (required)")
		limit := fs.Int("limit", 50, "max results")
		_ = fs.Parse(args)
		requireFlag(*workspaceID, "--workspace-id", fs)
		lim := int32(*limit)
		client := userconnect.NewUserAdminServiceClient(httpClient, serverURL, opts...)
		resp, err := client.ListWorkspaceMembers(ctx, connect.NewRequest(&userv1.ListWorkspaceMembersRequest{
			WorkspaceId: *workspaceID, Limit: &lim,
		}))
		if err != nil {
			return err
		}
		return print(resp.Msg)

	default:
		parts := splitCmd(cmd)
		fmt.Fprintf(os.Stderr, "unknown command: %s %s\n\n", parts[0], parts[1])
		usage()
		os.Exit(1)
	}
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func print(msg proto.Message) error {
	b, err := protojson.MarshalOptions{Multiline: true, EmitUnpopulated: false}.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}
	fmt.Println(string(b))
	return nil
}

func requireFlag(val, name string, fs *flag.FlagSet) {
	if val == "" {
		fmt.Fprintf(os.Stderr, "error: %s is required\n\n", name)
		fs.Usage()
		os.Exit(1)
	}
}

func splitCmd(cmd string) [2]string {
	for i, c := range cmd {
		if c == ' ' {
			return [2]string{cmd[:i], cmd[i+1:]}
		}
	}
	return [2]string{cmd, ""}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func usage() {
	fmt.Fprintf(os.Stderr, `orangectl — orange management plane CLI

Usage:
  orangectl [--server URL] [--api-key KEY] <resource> <verb> [flags]

  --server URL   server base URL (default: %s, env: ORANGE_SERVER)
  --api-key KEY  admin API key, required (env: ORANGE_API_KEY)

Operations (orange-ops/06-admin-operations.md):

  Orgs:
    orangectl org create   --name <slug> [--description <text>]
    orangectl org get      --org-id <id>
    orangectl org list

  Projects (A1):
    orangectl project create  --org-id <id> --name <slug>
    orangectl project get     --project-id <id>
    orangectl project list    --org-id <id>

  Workspaces (A2):
    orangectl workspace create  --project-id <id> --name <slug>
    orangectl workspace get     --workspace-id <id>
    orangectl workspace list    --project-id <id>

  Users:
    orangectl user create  --org-id <id> --email <addr>
    orangectl user get     --user-id <id>
    orangectl user list    --org-id <id>

  Workspace members (A5/A6):
    orangectl member add     --workspace-id <id> --user-id <id>
    orangectl member remove  --workspace-id <id> --user-id <id>
    orangectl member list    --workspace-id <id>

`, defaultServer)
}
