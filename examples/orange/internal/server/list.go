package server

import (
	"context"
	"fmt"
	"os"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	orgv1 "github.com/dio/transit/examples/orange/api/orange/org/admin/v1"
	orgconnect "github.com/dio/transit/examples/orange/api/orange/org/admin/v1/adminv1connect"
	projectv1 "github.com/dio/transit/examples/orange/api/orange/project/admin/v1"
	projectconnect "github.com/dio/transit/examples/orange/api/orange/project/admin/v1/adminv1connect"
	workspacev1 "github.com/dio/transit/examples/orange/api/orange/workspace/admin/v1"
	workspaceconnect "github.com/dio/transit/examples/orange/api/orange/workspace/admin/v1/adminv1connect"
)

// wsEntry is a denormalized workspace row carrying org and project names.
type wsEntry struct {
	name, wsID       string
	projName, projID string
	orgName, orgID   string
	createdAt        *timestamppb.Timestamp
}

// listAllWorkspaces fans out ListOrgs → ListProjects → ListWorkspaces and
// returns every workspace visible to the caller, fully paginated.
// If orgIDs is non-empty only those orgs are queried; otherwise all orgs are fetched.
func listAllWorkspaces(ctx context.Context, rc *RunCtx, orgIDs ...string) ([]wsEntry, error) {
	orgClient := orgconnect.NewOrgAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
	projClient := projectconnect.NewProjectAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
	wsClient := workspaceconnect.NewWorkspaceAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)

	orgNames := map[string]string{}

	if len(orgIDs) == 0 {
		// Fetch all orgs (paginated).
		for tok := ""; ; {
			req := &orgv1.ListOrgsRequest{}
			if tok != "" {
				req.PageToken = &tok
			}
			resp, err := orgClient.ListOrgs(ctx, connect.NewRequest(req))
			if err != nil {
				return nil, fmt.Errorf("list orgs: %w", err)
			}
			for _, o := range resp.Msg.GetOrgs() {
				orgIDs = append(orgIDs, o.GetOrgId())
				orgNames[o.GetOrgId()] = o.GetName()
			}
			tok = resp.Msg.GetNextPageToken()
			if tok == "" {
				break
			}
		}
	} else {
		// Resolve names for the supplied org IDs.
		for _, oid := range orgIDs {
			resp, err := orgClient.GetOrg(ctx, connect.NewRequest(&orgv1.GetOrgRequest{OrgId: oid}))
			if err != nil {
				return nil, fmt.Errorf("get org %s: %w", oid, err)
			}
			orgNames[oid] = resp.Msg.GetOrg().GetName()
		}
	}

	var entries []wsEntry
	for _, oid := range orgIDs {
		// Paginate projects.
		var projIDs []string
		projNames := map[string]string{}
		for tok := ""; ; {
			req := &projectv1.ListProjectsRequest{OrgId: oid}
			if tok != "" {
				req.PageToken = &tok
			}
			resp, err := projClient.ListProjects(ctx, connect.NewRequest(req))
			if err != nil {
				return nil, fmt.Errorf("list projects (org %s): %w", oid, err)
			}
			for _, p := range resp.Msg.GetProjects() {
				projIDs = append(projIDs, p.GetProjectId())
				projNames[p.GetProjectId()] = p.GetName()
			}
			tok = resp.Msg.GetNextPageToken()
			if tok == "" {
				break
			}
		}

		for _, pid := range projIDs {
			for tok := ""; ; {
				req := &workspacev1.ListWorkspacesRequest{ProjectId: pid}
				if tok != "" {
					req.PageToken = &tok
				}
				resp, err := wsClient.ListWorkspaces(ctx, connect.NewRequest(req))
				if err != nil {
					return nil, fmt.Errorf("list workspaces (proj %s): %w", pid, err)
				}
				for _, w := range resp.Msg.GetWorkspaces() {
					entries = append(entries, wsEntry{
						name:      w.GetName(),
						wsID:      w.GetWorkspaceId(),
						projName:  projNames[pid],
						projID:    pid,
						orgName:   orgNames[oid],
						orgID:     oid,
						createdAt: w.GetCreatedAt(),
					})
				}
				tok = resp.Msg.GetNextPageToken()
				if tok == "" {
					break
				}
			}
		}
	}
	return entries, nil
}

// printWSEntries renders a flat workspace table with org/project context columns.
func printWSEntries(p *Printer, entries []wsEntry) {
	rows := make([]string, len(entries))
	for i, e := range entries {
		rows[i] = fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s",
			e.name, e.wsID, e.projName, e.projID, e.orgName, e.orgID, age(e.createdAt))
	}
	p.Table("NAME\tWS-ID\tPROJECT\tPROJ-ID\tORG\tORG-ID\tAGE", rows)
}

// ── orange admin list ──────────────────────────────────────────────────────────

func newListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List resources across the org hierarchy",
	}
	cmd.AddCommand(newListWSCmd())
	return cmd
}

func newListWSCmd() *cobra.Command {
	var all bool
	var orgID string

	cmd := &cobra.Command{
		Use:          "ws",
		Aliases:      []string{"workspace"},
		Short:        "List workspaces",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if orgID == "" {
				orgID = os.Getenv("ORANGE_ORG_ID")
			}
			if !all && orgID == "" {
				return fmt.Errorf("--org-id is required unless --all is set (or set ORANGE_ORG_ID)")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			ctx := context.Background()

			var entries []wsEntry
			if all {
				entries, err = listAllWorkspaces(ctx, rc)
			} else {
				entries, err = listAllWorkspaces(ctx, rc, orgID)
			}
			if err != nil {
				return err
			}
			printWSEntries(rc.Printer, entries)
			return nil
		},
	}

	cmd.Flags().BoolVar(&all, "all", false, "list workspaces across all orgs and projects")
	cmd.Flags().StringVar(&orgID, "org-id", "", "org to list workspaces for (env: ORANGE_ORG_ID); ignored with --all")
	return cmd
}
