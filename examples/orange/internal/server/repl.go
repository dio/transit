package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"connectrpc.com/connect"
	"github.com/chzyer/readline"
	"github.com/spf13/cobra"

	apikeyv1 "github.com/dio/transit/examples/orange/api/orange/apikey/admin/v1"
	apikeyconnect "github.com/dio/transit/examples/orange/api/orange/apikey/admin/v1/adminv1connect"
	egressv1 "github.com/dio/transit/examples/orange/api/orange/egress/admin/v1"
	egressconnect "github.com/dio/transit/examples/orange/api/orange/egress/admin/v1/adminv1connect"
	orgv1 "github.com/dio/transit/examples/orange/api/orange/org/admin/v1"
	orgconnect "github.com/dio/transit/examples/orange/api/orange/org/admin/v1/adminv1connect"
	profilev1 "github.com/dio/transit/examples/orange/api/orange/profile/admin/v1"
	profileconnect "github.com/dio/transit/examples/orange/api/orange/profile/admin/v1/adminv1connect"
	projectv1 "github.com/dio/transit/examples/orange/api/orange/project/admin/v1"
	projectconnect "github.com/dio/transit/examples/orange/api/orange/project/admin/v1/adminv1connect"
	secretv1 "github.com/dio/transit/examples/orange/api/orange/secret/admin/v1"
	secretconnect "github.com/dio/transit/examples/orange/api/orange/secret/admin/v1/adminv1connect"
	userv1 "github.com/dio/transit/examples/orange/api/orange/user/admin/v1"
	userconnect "github.com/dio/transit/examples/orange/api/orange/user/admin/v1/adminv1connect"
	workspacev1 "github.com/dio/transit/examples/orange/api/orange/workspace/admin/v1"
	workspaceconnect "github.com/dio/transit/examples/orange/api/orange/workspace/admin/v1/adminv1connect"
	"github.com/dio/transit/examples/orange/internal/server/secret"
)

func newReplCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "repl [key=value ...]",
		Short: "Start an interactive admin REPL",
		Long: `Interactive admin shell with persistent context. Type 'help' for commands.

Seed the context before entering the REPL by passing key=value positional args:

  orange admin repl ws=<id>
  orange admin repl org=<id> proj=<id>
  orange admin repl ws=<id> org=<id>

Valid keys: org, proj, ws  (values are IDs, looked up via the API on start).`,
		SilenceUsage: true,
		Args:         cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			return runREPL(rc, args)
		},
	}
}

// replState tracks the active org/project/workspace context across commands.
type replState struct {
	rc       *RunCtx
	orgID    string
	orgName  string
	projID   string
	projName string
	wsID     string
	wsName   string
}

func (s *replState) prompt() string {
	var parts []string
	if s.orgName != "" {
		parts = append(parts, s.orgName)
	} else if s.orgID != "" {
		parts = append(parts, shortID(s.orgID))
	}
	if s.projName != "" {
		parts = append(parts, s.projName)
	} else if s.projID != "" {
		parts = append(parts, shortID(s.projID))
	}
	if s.wsName != "" {
		parts = append(parts, s.wsName)
	} else if s.wsID != "" {
		parts = append(parts, shortID(s.wsID))
	}
	if len(parts) == 0 {
		return "orange> "
	}
	return fmt.Sprintf("orange [%s]> ", strings.Join(parts, " / "))
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func runREPL(rc *RunCtx, seedArgs []string) error {
	histFile := os.ExpandEnv("$HOME/.orange/repl_history")
	_ = os.MkdirAll(os.ExpandEnv("$HOME/.orange"), 0o700)

	s := &replState{rc: rc}

	// Apply seed context from positional key=value args (e.g. ws=<id>).
	// Order matters: resolve org first, then proj, then ws so that breadcrumbs
	// propagate correctly. Sort by kind before applying.
	order := map[string]int{"org": 0, "organization": 0, "proj": 1, "project": 1, "ws": 2, "workspace": 2}
	type seedPair struct{ key, val string }
	seeds := make([]seedPair, 0, len(seedArgs))
	for _, arg := range seedArgs {
		k, v, ok := strings.Cut(arg, "=")
		if !ok || v == "" {
			fmt.Fprintf(os.Stderr, "warning: ignoring seed arg %q — expected key=value\n", arg)
			continue
		}
		if _, known := order[k]; !known {
			fmt.Fprintf(os.Stderr, "warning: unknown seed key %q — valid keys: org, proj, ws\n", k)
			continue
		}
		seeds = append(seeds, seedPair{k, v})
	}
	// Stable sort by kind order.
	for i := 1; i < len(seeds); i++ {
		for j := i; j > 0 && order[seeds[j].key] < order[seeds[j-1].key]; j-- {
			seeds[j], seeds[j-1] = seeds[j-1], seeds[j]
		}
	}
	for _, sp := range seeds {
		if err := s.cmdUse(sp.key, sp.val); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not seed %s=%s: %v\n", sp.key, sp.val, err)
		}
	}

	rl, err := readline.NewEx(&readline.Config{
		Prompt:          s.prompt(),
		HistoryFile:     histFile,
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
	})
	if err != nil {
		return err
	}
	defer rl.Close()

	fmt.Println("orange admin REPL  •  type 'help' for commands, 'exit' or Ctrl+D to quit")

	for {
		rl.SetPrompt(s.prompt())
		line, err := rl.Readline()
		if errors.Is(err, readline.ErrInterrupt) {
			if line == "" {
				break
			}
			continue
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if dispatchErr := s.dispatch(line); dispatchErr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", dispatchErr)
		}
	}
	return nil
}

// tokenize splits a REPL input line on whitespace but keeps single-quoted
// strings together as one token (stripping the surrounding quotes).
// This lets users write: rl-scope set entry='{"models":["*"],"rpm":100}'
func tokenize(line string) []string {
	var tokens []string
	var cur strings.Builder
	inSingle := false
	for _, ch := range line {
		switch {
		case ch == '\'' && !inSingle:
			inSingle = true
		case ch == '\'' && inSingle:
			inSingle = false
		case (ch == ' ' || ch == '\t') && !inSingle:
			if cur.Len() > 0 {
				tokens = append(tokens, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(ch)
		}
	}
	if cur.Len() > 0 {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

// dispatch splits the line and routes to the appropriate handler.
func (s *replState) dispatch(line string) error {
	toks := tokenize(line)
	if len(toks) == 0 {
		return nil
	}

	switch toks[0] {
	case "exit", "quit":
		os.Exit(0)

	case "help", "?":
		printReplHelp()

	case "context", "ctx":
		s.printContext()

	case "use":
		if len(toks) < 3 {
			return fmt.Errorf("usage: use <org|proj|ws> <id>")
		}
		return s.cmdUse(toks[1], toks[2])

	case "unset":
		if len(toks) < 2 {
			return fmt.Errorf("usage: unset <org|proj|ws>")
		}
		return s.cmdUnset(toks[1])

	case "org", "organization":
		return s.cmdOrg(toks[1:])

	case "proj", "project":
		return s.cmdProj(toks[1:])

	case "ws", "workspace":
		return s.cmdWS(toks[1:])

	case "member", "mbr":
		return s.cmdMember(toks[1:])

	case "user", "usr":
		return s.cmdUser(toks[1:])

	case "egress":
		return s.cmdEgress(toks[1:])

	case "apikey", "key":
		return s.cmdAPIKey(toks[1:])

	case "secret", "sec":
		return s.cmdSecret(toks[1:])

	case "config", "cfg":
		return s.cmdConfig(toks[1:])

	case "rl-tier", "rlt":
		return s.cmdRLTier(toks[1:])

	case "rl-scope", "rls":
		return s.cmdRLScope(toks[1:])

	case "rl-policy", "rlp":
		return s.cmdPolicy(toks[1:])

	case "keyentry", "ke":
		return s.cmdKeyEntry(toks[1:])

	case "keyentry-token", "ket":
		return s.cmdKeyEntryToken(toks[1:])

	case "keyentry-secret", "kes":
		return s.cmdKeyEntrySecret(toks[1:])

	case "keyentry-routing", "ker":
		return s.cmdKeyEntryRouting(toks[1:])

	case "profile", "prof":
		return s.cmdProfile(toks[1:])

	case "su":
		return s.cmdSu(toks[1:])

	default:
		return fmt.Errorf("unknown command %q — type 'help' for a list", toks[0])
	}
	return nil
}

// ── navigation ────────────────────────────────────────────────────────────────

func (s *replState) cmdUse(resource, id string) error {
	ctx := context.Background()
	switch resource {
	case "org", "organization":
		client := orgconnect.NewOrgAdminServiceClient(s.rc.HTTPClient, s.rc.ServerURL, s.rc.ConnectOpts...)
		resp, err := client.GetOrg(ctx, connect.NewRequest(&orgv1.GetOrgRequest{OrgId: id}))
		if err != nil {
			return err
		}
		o := resp.Msg.GetOrg()
		s.orgID, s.orgName = o.GetOrgId(), o.GetName()
		s.projID, s.projName = "", ""
		s.wsID, s.wsName = "", ""
		fmt.Printf("org: %s (%s)\n", s.orgName, s.orgID)

	case "proj", "project":
		projClient := projectconnect.NewProjectAdminServiceClient(s.rc.HTTPClient, s.rc.ServerURL, s.rc.ConnectOpts...)
		resp, err := projClient.GetProject(ctx, connect.NewRequest(&projectv1.GetProjectRequest{ProjectId: id}))
		if err != nil {
			return err
		}
		p := resp.Msg.GetProject()
		s.projID, s.projName = p.GetProjectId(), p.GetName()
		s.wsID, s.wsName = "", ""
		// Backfill org breadcrumb when jumping directly to a project.
		if s.orgID == "" || s.orgName == "" {
			s.orgID = p.GetOrgId()
			orgClient := orgconnect.NewOrgAdminServiceClient(s.rc.HTTPClient, s.rc.ServerURL, s.rc.ConnectOpts...)
			if or2, err2 := orgClient.GetOrg(ctx, connect.NewRequest(&orgv1.GetOrgRequest{OrgId: s.orgID})); err2 == nil {
				s.orgName = or2.Msg.GetOrg().GetName()
			}
		}
		fmt.Printf("project: %s (%s)\n", s.projName, s.projID)

	case "ws", "workspace":
		wsClient := workspaceconnect.NewWorkspaceAdminServiceClient(s.rc.HTTPClient, s.rc.ServerURL, s.rc.ConnectOpts...)
		resp, err := wsClient.GetWorkspace(ctx, connect.NewRequest(&workspacev1.GetWorkspaceRequest{WorkspaceId: id}))
		if err != nil {
			return err
		}
		w := resp.Msg.GetWorkspace()
		s.wsID, s.wsName = w.GetWorkspaceId(), w.GetName()
		// Backfill proj breadcrumb when jumping directly to a workspace.
		if s.projID == "" || s.projName == "" {
			s.projID = w.GetProjectId()
			projClient := projectconnect.NewProjectAdminServiceClient(s.rc.HTTPClient, s.rc.ServerURL, s.rc.ConnectOpts...)
			if pr2, err2 := projClient.GetProject(ctx, connect.NewRequest(&projectv1.GetProjectRequest{ProjectId: s.projID})); err2 == nil {
				s.projName = pr2.Msg.GetProject().GetName()
				// Backfill org breadcrumb too.
				if s.orgID == "" || s.orgName == "" {
					s.orgID = pr2.Msg.GetProject().GetOrgId()
					orgClient := orgconnect.NewOrgAdminServiceClient(s.rc.HTTPClient, s.rc.ServerURL, s.rc.ConnectOpts...)
					if or2, err3 := orgClient.GetOrg(ctx, connect.NewRequest(&orgv1.GetOrgRequest{OrgId: s.orgID})); err3 == nil {
						s.orgName = or2.Msg.GetOrg().GetName()
					}
				}
			}
		}
		fmt.Printf("workspace: %s (%s)\n", s.wsName, s.wsID)

	default:
		return fmt.Errorf("unknown resource %q — try: org, proj, ws", resource)
	}
	return nil
}

func (s *replState) cmdUnset(resource string) error {
	switch resource {
	case "org", "organization":
		s.orgID, s.orgName = "", ""
		s.projID, s.projName = "", ""
		s.wsID, s.wsName = "", ""
	case "proj", "project":
		s.projID, s.projName = "", ""
		s.wsID, s.wsName = "", ""
	case "ws", "workspace":
		s.wsID, s.wsName = "", ""
	default:
		return fmt.Errorf("unknown resource %q — try: org, proj, ws", resource)
	}
	return nil
}

func (s *replState) printContext() {
	if s.orgID == "" && s.projID == "" && s.wsID == "" {
		fmt.Println("no context set — use 'use org <id>' to start")
		return
	}
	if s.orgID != "" {
		fmt.Printf("org:     %s (%s)\n", s.orgName, s.orgID)
	}
	if s.projID != "" {
		fmt.Printf("project: %s (%s)\n", s.projName, s.projID)
	}
	if s.wsID != "" {
		fmt.Printf("workspace: %s (%s)\n", s.wsName, s.wsID)
	}
}

// ── org ───────────────────────────────────────────────────────────────────────

func (s *replState) cmdOrg(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	ctx := context.Background()
	client := orgconnect.NewOrgAdminServiceClient(s.rc.HTTPClient, s.rc.ServerURL, s.rc.ConnectOpts...)

	switch sub {
	case "ls", "list":
		resp, err := client.ListOrgs(ctx, connect.NewRequest(&orgv1.ListOrgsRequest{}))
		if err != nil {
			return err
		}
		return printOrgs(s.rc.Printer, resp.Msg.GetOrgs()...)

	case "get":
		id := s.orgID
		if len(args) > 1 {
			id = args[1]
		}
		if id == "" {
			return fmt.Errorf("no org in context — provide an id or run 'use org <id>'")
		}
		resp, err := client.GetOrg(ctx, connect.NewRequest(&orgv1.GetOrgRequest{OrgId: id}))
		if err != nil {
			return err
		}
		return printOrgs(s.rc.Printer, resp.Msg.GetOrg())

	case "create":
		if len(args) < 2 {
			return fmt.Errorf("usage: org create <name>")
		}
		resp, err := client.CreateOrg(ctx, connect.NewRequest(&orgv1.CreateOrgRequest{Name: args[1]}))
		if err != nil {
			return err
		}
		return printOrgs(s.rc.Printer, resp.Msg.GetOrg())

	case "update":
		id := s.orgID
		if len(args) > 1 && !strings.Contains(args[1], "=") {
			id = args[1]
		}
		if id == "" {
			return fmt.Errorf("no org in context — provide an id or run 'use org <id>'")
		}
		req := &orgv1.UpdateOrgRequest{OrgId: id}
		if desc := kvGet(args[1:], "description"); desc != "" {
			req.Description = &desc
		}
		resp, err := client.UpdateOrg(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		return printOrgs(s.rc.Printer, resp.Msg.GetOrg())

	case "delete":
		id := s.orgID
		if len(args) > 1 {
			id = args[1]
		}
		if id == "" {
			return fmt.Errorf("no org in context — provide an id or run 'use org <id>'")
		}
		_, err := client.DeleteOrg(ctx, connect.NewRequest(&orgv1.DeleteOrgRequest{OrgId: id}))
		if err != nil {
			return err
		}
		if id == s.orgID {
			s.orgID, s.orgName = "", ""
			s.projID, s.projName = "", ""
			s.wsID, s.wsName = "", ""
		}
		s.rc.Printer.OK("deleted")

	default:
		return fmt.Errorf("unknown org subcommand %q — try: ls, get, create, update, delete", sub)
	}
	return nil
}

// ── project ───────────────────────────────────────────────────────────────────

func (s *replState) cmdProj(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	ctx := context.Background()
	client := projectconnect.NewProjectAdminServiceClient(s.rc.HTTPClient, s.rc.ServerURL, s.rc.ConnectOpts...)

	switch sub {
	case "ls", "list":
		if s.orgID == "" {
			return fmt.Errorf("no org in context — run 'use org <id>' first")
		}
		resp, err := client.ListProjects(ctx, connect.NewRequest(&projectv1.ListProjectsRequest{OrgId: s.orgID}))
		if err != nil {
			return err
		}
		return printProjects(s.rc.Printer, resp.Msg.GetProjects()...)

	case "get":
		id := s.projID
		if len(args) > 1 {
			id = args[1]
		}
		if id == "" {
			return fmt.Errorf("no project in context — provide an id or run 'use proj <id>'")
		}
		resp, err := client.GetProject(ctx, connect.NewRequest(&projectv1.GetProjectRequest{ProjectId: id}))
		if err != nil {
			return err
		}
		return printProjects(s.rc.Printer, resp.Msg.GetProject())

	case "create":
		if len(args) < 2 {
			return fmt.Errorf("usage: proj create <name>")
		}
		if s.orgID == "" {
			return fmt.Errorf("no org in context — run 'use org <id>' first")
		}
		resp, err := client.CreateProject(ctx, connect.NewRequest(&projectv1.CreateProjectRequest{OrgId: s.orgID, Name: args[1]}))
		if err != nil {
			return err
		}
		return printProjects(s.rc.Printer, resp.Msg.GetProject())

	case "update":
		id := s.projID
		if len(args) > 1 && !strings.Contains(args[1], "=") {
			id = args[1]
		}
		if id == "" {
			return fmt.Errorf("no project in context — provide an id or run 'use proj <id>'")
		}
		req := &projectv1.UpdateProjectRequest{ProjectId: id}
		if desc := kvGet(args[1:], "description"); desc != "" {
			req.Description = &desc
		}
		resp, err := client.UpdateProject(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		return printProjects(s.rc.Printer, resp.Msg.GetProject())

	case "delete":
		id := s.projID
		if len(args) > 1 {
			id = args[1]
		}
		if id == "" {
			return fmt.Errorf("no project in context — provide an id or run 'use proj <id>'")
		}
		_, err := client.DeleteProject(ctx, connect.NewRequest(&projectv1.DeleteProjectRequest{ProjectId: id}))
		if err != nil {
			return err
		}
		if id == s.projID {
			s.projID, s.projName = "", ""
			s.wsID, s.wsName = "", ""
		}
		s.rc.Printer.OK("deleted")

	default:
		return fmt.Errorf("unknown proj subcommand %q — try: ls, get, create, update, delete", sub)
	}
	return nil
}

// ── workspace ─────────────────────────────────────────────────────────────────

func (s *replState) cmdWS(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	ctx := context.Background()
	client := workspaceconnect.NewWorkspaceAdminServiceClient(s.rc.HTTPClient, s.rc.ServerURL, s.rc.ConnectOpts...)

	switch sub {
	case "ls", "list":
		// "ws ls *" lists all workspaces across every org and project.
		if len(args) > 1 && args[1] == "*" {
			entries, err := listAllWorkspaces(ctx, s.rc)
			if err != nil {
				return err
			}
			printWSEntries(s.rc.Printer, entries)
			return nil
		}
		if s.projID == "" {
			return fmt.Errorf("no project in context — run 'use proj <id>' first (or 'ws ls *' to list all)")
		}
		resp, err := client.ListWorkspaces(ctx, connect.NewRequest(&workspacev1.ListWorkspacesRequest{ProjectId: s.projID}))
		if err != nil {
			return err
		}
		return printWorkspaces(s.rc.Printer, resp.Msg.GetWorkspaces()...)

	case "get":
		id := s.wsID
		if len(args) > 1 {
			id = args[1]
		}
		if id == "" {
			return fmt.Errorf("no workspace in context — provide an id or run 'use ws <id>'")
		}
		resp, err := client.GetWorkspace(ctx, connect.NewRequest(&workspacev1.GetWorkspaceRequest{WorkspaceId: id}))
		if err != nil {
			return err
		}
		return printWorkspaces(s.rc.Printer, resp.Msg.GetWorkspace())

	case "create":
		if len(args) < 2 {
			return fmt.Errorf("usage: ws create <name>")
		}
		if s.projID == "" {
			return fmt.Errorf("no project in context — run 'use proj <id>' first")
		}
		resp, err := client.CreateWorkspace(ctx, connect.NewRequest(&workspacev1.CreateWorkspaceRequest{
			ProjectId: s.projID,
			Name:      args[1],
		}))
		if err != nil {
			return err
		}
		return printWorkspaces(s.rc.Printer, resp.Msg.GetWorkspace())

	case "update":
		id := s.wsID
		if len(args) > 1 && !strings.Contains(args[1], "=") {
			id = args[1]
		}
		if id == "" {
			return fmt.Errorf("no workspace in context — provide an id or run 'use ws <id>'")
		}
		req := &workspacev1.UpdateWorkspaceRequest{WorkspaceId: id}
		if desc := kvGet(args[1:], "description"); desc != "" {
			req.Description = &desc
		}
		resp, err := client.UpdateWorkspace(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		return printWorkspaces(s.rc.Printer, resp.Msg.GetWorkspace())

	case "delete":
		id := s.wsID
		if len(args) > 1 {
			id = args[1]
		}
		if id == "" {
			return fmt.Errorf("no workspace in context — provide an id or run 'use ws <id>'")
		}
		_, err := client.DeleteWorkspace(ctx, connect.NewRequest(&workspacev1.DeleteWorkspaceRequest{WorkspaceId: id}))
		if err != nil {
			return err
		}
		if id == s.wsID {
			s.wsID, s.wsName = "", ""
		}
		s.rc.Printer.OK("deleted")

	default:
		return fmt.Errorf("unknown ws subcommand %q — try: ls, get, create, update, delete", sub)
	}
	return nil
}

// ── member ────────────────────────────────────────────────────────────────────

func (s *replState) cmdMember(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	if s.wsID == "" {
		return fmt.Errorf("no workspace in context — run 'use ws <id>' first")
	}
	ctx := context.Background()
	client := userconnect.NewUserAdminServiceClient(s.rc.HTTPClient, s.rc.ServerURL, s.rc.ConnectOpts...)

	switch sub {
	case "ls", "list":
		resp, err := client.ListWorkspaceMembers(ctx, connect.NewRequest(&userv1.ListWorkspaceMembersRequest{WorkspaceId: s.wsID}))
		if err != nil {
			return err
		}
		return s.printMembersWithEmail(ctx, client, resp.Msg.GetMembers()...)

	case "add":
		if len(args) < 2 {
			return fmt.Errorf("usage: member add <user-id>")
		}
		resp, err := client.AddWorkspaceMember(ctx, connect.NewRequest(&userv1.AddWorkspaceMemberRequest{
			WorkspaceId: s.wsID,
			UserId:      args[1],
		}))
		if err != nil {
			return err
		}
		return printMembers(s.rc.Printer, resp.Msg.GetMember())

	case "rm", "remove":
		if len(args) < 2 {
			return fmt.Errorf("usage: member rm <user-id>")
		}
		_, err := client.RemoveWorkspaceMember(ctx, connect.NewRequest(&userv1.RemoveWorkspaceMemberRequest{
			WorkspaceId: s.wsID,
			UserId:      args[1],
		}))
		if err != nil {
			return err
		}
		s.rc.Printer.OK("removed")

	default:
		return fmt.Errorf("unknown member subcommand %q — try: ls, add, rm", sub)
	}
	return nil
}

// printMembersWithEmail enriches each WorkspaceMember with an email by calling
// GetUser per member, then prints EMAIL | USER-ID | AGE. WORKSPACE-ID is
// omitted because this is always called from within a workspace context.
// On a GetUser failure the email column shows "<unknown>" so the rest renders.
func (s *replState) printMembersWithEmail(ctx context.Context, client userconnect.UserAdminServiceClient, members ...*userv1.WorkspaceMember) error {
	tableRows := make([]string, len(members))
	for i, m := range members {
		email := "<unknown>"
		if ur, err := client.GetUser(ctx, connect.NewRequest(&userv1.GetUserRequest{UserId: m.GetUserId()})); err == nil {
			email = ur.Msg.GetUser().GetEmail()
		}
		tableRows[i] = fmt.Sprintf("%s\t%s\t%s", email, m.GetUserId(), age(m.GetJoinedAt()))
	}
	s.rc.Printer.Table("EMAIL\tUSER-ID\tAGE", tableRows)
	return nil
}

// ── user ──────────────────────────────────────────────────────────────────────

func (s *replState) cmdUser(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	ctx := context.Background()
	client := userconnect.NewUserAdminServiceClient(s.rc.HTTPClient, s.rc.ServerURL, s.rc.ConnectOpts...)

	switch sub {
	case "ls", "list":
		if s.orgID == "" {
			return fmt.Errorf("no org in context — run 'use org <id>' first")
		}
		resp, err := client.ListUsers(ctx, connect.NewRequest(&userv1.ListUsersRequest{OrgId: s.orgID}))
		if err != nil {
			return err
		}
		return printUsers(s.rc.Printer, resp.Msg.GetUsers()...)

	case "get":
		if len(args) < 2 {
			return fmt.Errorf("usage: user get <user-id>")
		}
		userID := args[1]
		resp, err := client.GetUser(ctx, connect.NewRequest(&userv1.GetUserRequest{UserId: userID}))
		if err != nil {
			return err
		}
		u := resp.Msg.GetUser()
		if err := printUsers(s.rc.Printer, u); err != nil {
			return err
		}
		// Fetch active API keys for this user and display their scopes.
		keyClient := apikeyconnect.NewAPIKeyAdminServiceClient(s.rc.HTTPClient, s.rc.ServerURL, s.rc.ConnectOpts...)
		keyResp, err := keyClient.ListKeys(ctx, connect.NewRequest(&apikeyv1.ListKeysRequest{
			OrgId:  u.GetOrgId(),
			UserId: userID,
		}))
		if err != nil {
			// Non-fatal: show user info we already printed and warn about keys.
			fmt.Fprintf(os.Stderr, "warning: could not fetch keys: %v\n", err)
			return nil
		}
		return printAPIKeys(s.rc.Printer, "", keyResp.Msg.GetKeys()...)

	case "create":
		if len(args) < 2 {
			return fmt.Errorf("usage: user create <email>")
		}
		if s.orgID == "" {
			return fmt.Errorf("no org in context — run 'use org <id>' first")
		}
		resp, err := client.CreateUser(ctx, connect.NewRequest(&userv1.CreateUserRequest{
			OrgId: s.orgID,
			Email: args[1],
		}))
		if err != nil {
			return err
		}
		return printUsers(s.rc.Printer, resp.Msg.GetUser())

	case "update":
		if len(args) < 2 {
			return fmt.Errorf("usage: user update <user-id> [description=<text>]")
		}
		userID := args[1]
		req := &userv1.UpdateUserRequest{UserId: userID}
		if desc := kvGet(args[2:], "description"); desc != "" {
			req.Description = &desc
		}
		resp, err := client.UpdateUser(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		return printUsers(s.rc.Printer, resp.Msg.GetUser())

	case "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: user delete <user-id>")
		}
		_, err := client.DeleteUser(ctx, connect.NewRequest(&userv1.DeleteUserRequest{UserId: args[1]}))
		if err != nil {
			return err
		}
		s.rc.Printer.OK("deleted")

	default:
		return fmt.Errorf("unknown user subcommand %q — try: ls, get, create, update, delete", sub)
	}
	return nil
}

// ── egress ────────────────────────────────────────────────────────────────────

func (s *replState) cmdEgress(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	if s.wsID == "" {
		return fmt.Errorf("no workspace in context — run 'use ws <id>' first")
	}
	ctx := context.Background()
	client := egressconnect.NewEgressAdminServiceClient(s.rc.HTTPClient, s.rc.ServerURL, s.rc.ConnectOpts...)

	switch sub {
	case "get":
		resp, err := client.GetEgressByWorkspace(ctx, connect.NewRequest(&egressv1.GetEgressByWorkspaceRequest{WorkspaceId: s.wsID}))
		if err != nil {
			return err
		}
		return printEgresses(s.rc.Printer, resp.Msg.GetEgress())

	case "bundle":
		outDir := "."
		if len(args) > 1 {
			outDir = args[1]
		}
		wsResp, err := client.GetEgressByWorkspace(ctx, connect.NewRequest(&egressv1.GetEgressByWorkspaceRequest{WorkspaceId: s.wsID}))
		if err != nil {
			return err
		}
		egressID := wsResp.Msg.GetEgress().GetEgressId()
		bundleResp, err := client.GetEgressBundle(ctx, connect.NewRequest(&egressv1.GetEgressBundleRequest{EgressId: egressID}))
		if err != nil {
			return err
		}
		outPath := outDir
		if outPath == "." {
			outPath = egressID + ".tar.gz"
		}
		return writeBundle(bundleResp.Msg.GetBundle(), outPath, s.rc.Printer)

	case "status":
		resp, err := client.GetEgressByWorkspace(ctx, connect.NewRequest(&egressv1.GetEgressByWorkspaceRequest{WorkspaceId: s.wsID}))
		if err != nil {
			return err
		}
		e := resp.Msg.GetEgress()
		fmt.Printf("egress %s: admin=%s online=%s\n",
			e.GetEgressId(),
			egressAdminStatusString(e.GetAdminStatus()),
			e.GetOnlineStatus().String())

	default:
		return fmt.Errorf("unknown egress subcommand %q — try: get, bundle, status", sub)
	}
	return nil
}

// ── secret ────────────────────────────────────────────────────────────────────

// expandRealm resolves a realm argument against the current REPL context.
// A fully-qualified realm ("ws/<uuid>/purpose", "proj/<uuid>/purpose",
// "org/<uuid>/purpose") is returned unchanged. A bare purpose string
// ("api-keys", "providers") is expanded to the innermost active scope:
// ws → proj → org. Returns an error when no context is set.
func (s *replState) expandRealm(raw string) (string, error) {
	if strings.HasPrefix(raw, "ws/") || strings.HasPrefix(raw, "proj/") || strings.HasPrefix(raw, "org/") {
		return raw, nil
	}
	switch {
	case s.wsID != "":
		return "ws/" + s.wsID + "/" + raw, nil
	case s.projID != "":
		return "proj/" + s.projID + "/" + raw, nil
	case s.orgID != "":
		return "org/" + s.orgID + "/" + raw, nil
	default:
		return "", fmt.Errorf("no context set — use 'ws use <id>' or provide a full realm (ws/<id>/purpose)")
	}
}

// cmdSecret routes secret subcommands.
//
// Realm inference: when the REPL prompt shows a workspace/project/org context,
// a bare purpose string is automatically expanded to the innermost scope:
//
//	secret set api-keys anthropic        → ws/<ws-id>/api-keys  (in ws context)
//	secret set proj/<id>/api-keys foo    → used as-is (explicit)
//
// Usage in REPL:
//
//	secret ls [realm-prefix]             defaults to current scope when omitted
//	secret set <realm> <name>            prompts for material with hidden input
//	secret get <realm> <name>
//	secret versions <realm> <name>
//	secret enable  <realm> <name> <version-id>
//	secret disable <realm> <name> <version-id>
//	secret retire  <realm> <name> <version-id>
func (s *replState) cmdSecret(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	ctx := context.Background()
	client := secretconnect.NewSecretAdminServiceClient(s.rc.HTTPClient, s.rc.ServerURL, s.rc.ConnectOpts...)

	switch sub {
	case "ls", "list":
		realm := ""
		if len(args) > 1 {
			realm = args[1]
		}
		// Default to listing the current scope when no prefix is given.
		if realm == "" {
			switch {
			case s.wsID != "":
				realm = "ws/" + s.wsID + "/"
			case s.projID != "":
				realm = "proj/" + s.projID + "/"
			case s.orgID != "":
				realm = "org/" + s.orgID + "/"
			}
		}
		resp, err := client.ListSecrets(ctx, connect.NewRequest(&secretv1.ListSecretsRequest{Realm: realm}))
		if err != nil {
			return err
		}
		return printSecretSummaries(s.rc.Printer, resp.Msg.GetSecrets()...)

	case "set":
		if len(args) < 3 {
			return fmt.Errorf("usage: secret set <realm> <name>")
		}
		realm, err := s.expandRealm(args[1])
		if err != nil {
			return err
		}
		if _, _, _, err := secret.ParseRealm(realm); err != nil {
			return err
		}
		name := args[2]
		material, err := s.readHiddenInput("Value (hidden): ")
		if err != nil {
			return err
		}
		if len(material) == 0 {
			return fmt.Errorf("material is empty; nothing stored")
		}
		resp, err := client.CreateVersion(ctx, connect.NewRequest(&secretv1.CreateVersionRequest{
			Realm:    realm,
			SecretId: name,
			Material: material,
			Enable:   true,
		}))
		if err != nil {
			return err
		}
		return printSecretVersions(s.rc.Printer, resp.Msg.GetVersion())

	case "get":
		if len(args) < 3 {
			return fmt.Errorf("usage: secret get <realm> <name>")
		}
		realm, err := s.expandRealm(args[1])
		if err != nil {
			return err
		}
		resp, err := client.ResolveVersion(ctx, connect.NewRequest(&secretv1.ResolveVersionRequest{
			Realm:    realm,
			SecretId: args[2],
		}))
		if err != nil {
			return err
		}
		// In REPL always print just the material — it's an interactive session.
		fmt.Println(string(resp.Msg.GetVersion().GetMaterial()))
		return nil

	case "versions":
		if len(args) < 3 {
			return fmt.Errorf("usage: secret versions <realm> <name>")
		}
		realm, err := s.expandRealm(args[1])
		if err != nil {
			return err
		}
		resp, err := client.ListVersions(ctx, connect.NewRequest(&secretv1.ListVersionsRequest{
			Realm:    realm,
			SecretId: args[2],
		}))
		if err != nil {
			return err
		}
		return printSecretVersions(s.rc.Printer, resp.Msg.GetVersions()...)

	case "enable":
		if len(args) < 4 {
			return fmt.Errorf("usage: secret enable <realm> <name> <version-id>")
		}
		realm, err := s.expandRealm(args[1])
		if err != nil {
			return err
		}
		resp, err := client.EnableVersion(ctx, connect.NewRequest(&secretv1.EnableVersionRequest{
			Realm:     realm,
			SecretId:  args[2],
			VersionId: args[3],
		}))
		if err != nil {
			return err
		}
		return printSecretVersions(s.rc.Printer, resp.Msg.GetVersion())

	case "disable":
		if len(args) < 4 {
			return fmt.Errorf("usage: secret disable <realm> <name> <version-id>")
		}
		realm, err := s.expandRealm(args[1])
		if err != nil {
			return err
		}
		resp, err := client.DisableVersion(ctx, connect.NewRequest(&secretv1.DisableVersionRequest{
			Realm:     realm,
			SecretId:  args[2],
			VersionId: args[3],
		}))
		if err != nil {
			return err
		}
		return printSecretVersions(s.rc.Printer, resp.Msg.GetVersion())

	case "retire":
		if len(args) < 4 {
			return fmt.Errorf("usage: secret retire <realm> <name> <version-id>")
		}
		realm, err := s.expandRealm(args[1])
		if err != nil {
			return err
		}
		resp, err := client.RetireVersion(ctx, connect.NewRequest(&secretv1.RetireVersionRequest{
			Realm:     realm,
			SecretId:  args[2],
			VersionId: args[3],
		}))
		if err != nil {
			return err
		}
		return printSecretVersions(s.rc.Printer, resp.Msg.GetVersion())

	case "kek":
		sub2 := ""
		if len(args) > 1 {
			sub2 = args[1]
		}
		realm := kvGet(args[2:], "realm")
		switch sub2 {
		case "create":
			resp, err := client.CreateServiceKEK(ctx, connect.NewRequest(&secretv1.CreateServiceKEKRequest{Realm: realm}))
			if err != nil {
				return err
			}
			switch s.rc.Printer.Format {
			case FormatJSON, FormatYAML:
				return s.rc.Printer.Proto(resp.Msg)
			default:
				s.rc.Printer.Table("KEK-ID\tVERSION", []string{
					fmt.Sprintf("%s\t%d", resp.Msg.GetKekId(), resp.Msg.GetKekVersion()),
				})
			}
		case "rotate":
			resp, err := client.RotateServiceKEK(ctx, connect.NewRequest(&secretv1.RotateServiceKEKRequest{Realm: realm}))
			if err != nil {
				return err
			}
			switch s.rc.Printer.Format {
			case FormatJSON, FormatYAML:
				return s.rc.Printer.Proto(resp.Msg)
			default:
				rows := make([]string, len(resp.Msg.GetRotated()))
				for i, r := range resp.Msg.GetRotated() {
					rows[i] = fmt.Sprintf("%s\t%d\t%d\t%d",
						r.GetKekId(), r.GetOldVersion(), r.GetNewVersion(), r.GetMasterKekVersion())
				}
				s.rc.Printer.Table("KEK-ID\tOLD-VER\tNEW-VER\tMASTER-VER", rows)
			}
		default:
			return fmt.Errorf("unknown secret kek subcommand %q — try: create [realm=<realm>], rotate [realm=<realm>]", sub2)
		}

	default:
		return fmt.Errorf("unknown secret subcommand %q — try: ls, set, get, versions, enable, disable, retire, kek", sub)
	}
	return nil
}

// readHiddenInput prompts the user and reads a line with terminal echo disabled
// so that sensitive material (API keys, passwords) is not visible when typed or pasted.
func (s *replState) readHiddenInput(prompt string) ([]byte, error) {
	rl, err := readline.NewEx(&readline.Config{
		Prompt:          prompt,
		InterruptPrompt: "^C",
		EOFPrompt:       "",
	})
	if err != nil {
		return nil, err
	}
	defer rl.Close()
	return rl.ReadPassword(prompt)
}

// ── profile ───────────────────────────────────────────────────────────────────

func (s *replState) cmdProfile(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	if s.wsID == "" {
		return fmt.Errorf("no workspace in context — run 'use ws <id>' first")
	}
	ctx := context.Background()
	client := profileconnect.NewProfileAdminServiceClient(s.rc.HTTPClient, s.rc.ServerURL, s.rc.ConnectOpts...)

	switch sub {
	case "ls", "list":
		userID := ""
		if len(args) > 1 {
			userID = args[1]
		}
		req := &profilev1.ListProfilesRequest{WorkspaceId: s.wsID}
		if userID != "" {
			req.UserId = &userID
		}
		resp, err := client.ListProfiles(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		return printProfiles(s.rc.Printer, s.wsName, resp.Msg.GetProfiles()...)

	case "get":
		if len(args) < 2 {
			return fmt.Errorf("usage: profile get <profile-id>")
		}
		resp, err := client.GetProfile(ctx, connect.NewRequest(&profilev1.GetProfileRequest{ProfileId: args[1]}))
		if err != nil {
			return err
		}
		return printProfileDetail(s.rc.Printer, resp.Msg.GetProfile())

	case "add", "create":
		if len(args) < 3 {
			return fmt.Errorf("usage: profile add <user-id> <name> [desc=<text>]")
		}
		userID, name := args[1], args[2]
		req := &profilev1.CreateProfileRequest{
			WorkspaceId: s.wsID,
			UserId:      userID,
			Name:        name,
		}
		if desc := kvGet(args[3:], "desc"); desc != "" {
			req.Description = &desc
		}
		resp, err := client.CreateProfile(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		p := resp.Msg.GetProfile()
		if err := printProfileDetail(s.rc.Printer, p); err != nil {
			return err
		}
		path := derivedProfilePath(s.wsName, p.GetUserId(), p.GetName())
		fmt.Printf("  MCP path (add to config): %s\n", path)
		return nil

	case "rm", "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: profile rm <profile-id>")
		}
		_, err := client.DeleteProfile(ctx, connect.NewRequest(&profilev1.DeleteProfileRequest{ProfileId: args[1]}))
		if err != nil {
			return err
		}
		s.rc.Printer.OK("deleted")

	default:
		return fmt.Errorf("unknown profile subcommand %q — try: ls [user-id], get <id>, add <user-id> <name>, rm <id>", sub)
	}
	return nil
}

// profileSlug converts a string to a URL-safe slug (lowercase, non-alphanum runs → "-").
func profileSlug(s string) string {
	var b strings.Builder
	prev := rune('-')
	for _, ch := range strings.ToLower(s) {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') {
			b.WriteRune(ch)
			prev = ch
		} else if prev != '-' {
			b.WriteByte('-')
			prev = '-'
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// derivedProfilePath produces the opaque MCP URL token for a profile:
// slug(wsName)--slug(user)--slug(profileName).
// For user, only the local part of an email is used ("alice@…" → "alice").
func derivedProfilePath(wsName, userID, profileName string) string {
	user := userID
	if at := strings.IndexByte(user, '@'); at >= 0 {
		user = user[:at]
	}
	return profileSlug(wsName) + "--" + profileSlug(user) + "--" + profileSlug(profileName)
}

func printProfiles(p *Printer, wsName string, profiles ...*profilev1.Profile) error {
	switch p.Format {
	case FormatJSON, FormatYAML:
		for _, pr := range profiles {
			if err := p.Proto(pr); err != nil {
				return err
			}
		}
		return nil
	default:
		rows := make([]string, len(profiles))
		for i, pr := range profiles {
			path := derivedProfilePath(wsName, pr.GetUserId(), pr.GetName())
			rows[i] = fmt.Sprintf("%s\t%s\t%s\t%s\t%d tool(s)",
				pr.GetProfileId(), pr.GetUserId(), pr.GetName(), path, len(pr.GetTools()))
		}
		p.Table("PROFILE-ID\tUSER-ID\tNAME\tMCP-PATH\tTOOLS", rows)
		return nil
	}
}

func printProfileDetail(p *Printer, pr *profilev1.Profile) error {
	switch p.Format {
	case FormatJSON, FormatYAML:
		return p.Proto(pr)
	default:
		fmt.Printf("profile:  %s\n", pr.GetProfileId())
		fmt.Printf("ws:       %s\n", pr.GetWorkspaceId())
		fmt.Printf("user:     %s\n", pr.GetUserId())
		fmt.Printf("name:     %s\n", pr.GetName())
		if desc := pr.GetDescription(); desc != "" {
			fmt.Printf("desc:     %s\n", desc)
		}
		if len(pr.GetTools()) > 0 {
			fmt.Println("tools:")
			for _, tf := range pr.GetTools() {
				opt := ""
				if tf.GetOptional() {
					opt = " (optional)"
				}
				inc := "*"
				if len(tf.GetInclude()) > 0 {
					inc = strings.Join(tf.GetInclude(), ", ")
				}
				fmt.Printf("  %-20s  [%s]%s\n", tf.GetServer(), inc, opt)
			}
		}
		if len(pr.GetAuthOverrides()) > 0 {
			fmt.Println("auth overrides:")
			for _, ao := range pr.GetAuthOverrides() {
				fmt.Printf("  %-20s  %s  %s\n", ao.GetServer(), ao.GetAuthType(), ao.GetSecretRef())
			}
		}
		fmt.Printf("created:  %s\n", age(pr.GetCreatedAt()))
		return nil
	}
}

// ── help ──────────────────────────────────────────────────────────────────────

// ── apikey ────────────────────────────────────────────────────────────────────

func (s *replState) cmdAPIKey(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	ctx := context.Background()
	client := apikeyconnect.NewAPIKeyAdminServiceClient(s.rc.HTTPClient, s.rc.ServerURL, s.rc.ConnectOpts...)

	switch sub {
	case "ls", "list":
		if s.orgID == "" {
			return fmt.Errorf("no org in context — run 'use org <id>' first")
		}
		userID := ""
		if len(args) > 1 {
			userID = args[1]
		}
		resp, err := client.ListKeys(ctx, connect.NewRequest(&apikeyv1.ListKeysRequest{
			OrgId:  s.orgID,
			UserId: userID,
		}))
		if err != nil {
			return err
		}
		return printAPIKeys(s.rc.Printer, "", resp.Msg.GetKeys()...)

	case "get":
		if len(args) < 2 {
			return fmt.Errorf("usage: apikey get <key-id>")
		}
		resp, err := client.GetKey(ctx, connect.NewRequest(&apikeyv1.GetKeyRequest{KeyId: args[1]}))
		if err != nil {
			return err
		}
		return printAPIKeyDetail(s.rc.Printer, resp.Msg.GetKey())

	case "scope":
		if len(args) < 2 {
			return fmt.Errorf("usage: apikey scope <key-id>")
		}
		return s.interactiveScopeEditor(ctx, client, args[1])

	case "issue":
		if s.orgID == "" {
			return fmt.Errorf("no org in context — run 'use org <id>' first")
		}
		userID := kvGet(args[1:], "user")
		wsID := kvGet(args[1:], "ws")
		if wsID == "" {
			wsID = s.wsID
		}
		scopeFlag := kvGet(args[1:], "scope")
		tmpl := kvGet(args[1:], "template")
		desc := kvGet(args[1:], "desc")
		scopeList := parseScopes(scopeFlag)
		if tmpl != "" {
			extra, err := templateScopes(tmpl, wsID, userID)
			if err != nil {
				return err
			}
			scopeList = mergeScopes(scopeList, extra)
		}
		rec, plaintext, err := issueAPIKey(s.rc, s.orgID, userID, wsID, scopeList, desc)
		if err != nil {
			return err
		}
		return printAPIKeys(s.rc.Printer, plaintext, rec)

	case "revoke":
		if len(args) < 2 {
			return fmt.Errorf("usage: apikey revoke <key-id>")
		}
		_, err := client.RevokeKey(ctx, connect.NewRequest(&apikeyv1.RevokeKeyRequest{KeyId: args[1]}))
		if err != nil {
			return err
		}
		s.rc.Printer.OK("revoked")

	default:
		return fmt.Errorf("unknown apikey subcommand %q — try: ls [user-id], get <key-id>, scope <key-id>, issue, revoke <key-id>", sub)
	}
	return nil
}

// interactiveScopeEditor presents a checkbox-style scope selection loop for a key.
func (s *replState) interactiveScopeEditor(ctx context.Context, client apikeyconnect.APIKeyAdminServiceClient, keyID string) error {
	resp, err := client.GetKey(ctx, connect.NewRequest(&apikeyv1.GetKeyRequest{KeyId: keyID}))
	if err != nil {
		return err
	}
	key := resp.Msg.GetKey()

	// Build the candidate scope list from known base scopes + workspace scopes (current
	// context or those already on the key).
	candidates := buildScopeCandidates(key.GetScopes(), s.wsID)

	// selected tracks which candidates are currently active.
	selected := make([]bool, len(candidates))
	have := make(map[string]bool, len(key.GetScopes()))
	for _, sc := range key.GetScopes() {
		have[sc] = true
	}
	for i, c := range candidates {
		selected[i] = have[c]
	}

	printScopeCheckboxes(key.GetKeyPrefix(), candidates, selected)
	fmt.Println("Toggle by number(s) (space-separated), 'ws-member' to add template, 'done' to apply, 'q' to cancel:")

	rl, err := readline.NewEx(&readline.Config{
		Prompt:          "scope> ",
		InterruptPrompt: "^C",
		EOFPrompt:       "done",
	})
	if err != nil {
		return err
	}
	defer rl.Close()

	for {
		line, err := rl.Readline()
		if err != nil {
			if errors.Is(err, readline.ErrInterrupt) || errors.Is(err, io.EOF) {
				fmt.Println("cancelled")
				return nil
			}
			return err
		}
		input := strings.TrimSpace(line)
		switch input {
		case "q", "quit", "cancel":
			fmt.Println("cancelled")
			return nil

		case "done", "":
			// Collect the selected scopes and call UpdateKeyScopes with whatever was toggled on.
			add := collectSelected(candidates, selected, key.GetScopes())
			if len(add) == 0 {
				fmt.Println("no new scopes to add")
				return nil
			}
			updResp, err := client.UpdateKeyScopes(ctx, connect.NewRequest(&apikeyv1.UpdateKeyScopesRequest{
				KeyId:     keyID,
				AddScopes: add,
			}))
			if err != nil {
				return err
			}
			fmt.Println("Updated scopes:")
			return printAPIKeyDetail(s.rc.Printer, updResp.Msg.GetKey())

		case "ws-member":
			if s.wsID == "" {
				fmt.Fprintln(os.Stderr, "no workspace in context — run 'use ws <id>' first")
				continue
			}
			// Add workspace member scopes to candidates + select them.
			wsCandidates := buildScopeCandidates(key.GetScopes(), s.wsID)
			if len(wsCandidates) > len(candidates) {
				for i := len(candidates); i < len(wsCandidates); i++ {
					candidates = append(candidates, wsCandidates[i])
					selected = append(selected, false)
				}
			}
			// Select all workspace-scoped candidates for s.wsID.
			for i, c := range candidates {
				if strings.HasSuffix(c, "["+s.wsID+"]") {
					selected[i] = true
				}
			}
			printScopeCheckboxes(key.GetKeyPrefix(), candidates, selected)
			fmt.Println("Toggle by number(s), 'done' to apply, 'q' to cancel:")

		default:
			// Parse space-separated numbers.
			toggled := false
			for _, tok := range strings.Fields(input) {
				var n int
				if _, err := fmt.Sscanf(tok, "%d", &n); err != nil || n < 1 || n > len(candidates) {
					fmt.Fprintf(os.Stderr, "invalid number %q (valid: 1-%d)\n", tok, len(candidates))
					continue
				}
				selected[n-1] = !selected[n-1]
				toggled = true
			}
			if toggled {
				printScopeCheckboxes(key.GetKeyPrefix(), candidates, selected)
				fmt.Println("Toggle by number(s), 'done' to apply, 'q' to cancel:")
			}
		}
	}
}

// buildScopeCandidates returns a deduplicated, ordered list of scope options for the editor.
// It includes all known base scopes, the key's existing scopes, and workspace scopes for wsID.
func buildScopeCandidates(existing []string, wsID string) []string {
	base := []string{
		"user:read",
		"org:admin",
		"egress-bundle:download",
	}
	if wsID != "" {
		base = append(base,
			"secret:read["+wsID+"]",
			"secret:write["+wsID+"]",
			"token:issue["+wsID+"]",
		)
	}
	// Append any existing scopes not already in base.
	have := make(map[string]bool, len(base))
	for _, s := range base {
		have[s] = true
	}
	for _, s := range existing {
		if !have[s] {
			base = append(base, s)
			have[s] = true
		}
	}
	return base
}

func printScopeCheckboxes(keyPrefix string, candidates []string, selected []bool) {
	fmt.Printf("\nKey: %s…\n", keyPrefix)
	for i, c := range candidates {
		check := " "
		if selected[i] {
			check = "✓"
		}
		fmt.Printf("  [%2d] %s  %s\n", i+1, check, c)
	}
}

// collectSelected returns scopes that are selected but not already on the key.
func collectSelected(candidates []string, selected []bool, existing []string) []string {
	have := make(map[string]bool, len(existing))
	for _, s := range existing {
		have[s] = true
	}
	var add []string
	for i, c := range candidates {
		if selected[i] && !have[c] {
			add = append(add, c)
		}
	}
	return add
}

// cmdSu switches to the user REPL using a provided or prompted API key.
// Ctrl-D / exit in the user REPL returns to the admin REPL.
func (s *replState) cmdSu(args []string) error {
	var apiKey string
	if len(args) > 0 {
		apiKey = os.ExpandEnv(args[0])
	} else {
		raw, err := s.readHiddenInput("API key: ")
		if err != nil {
			return err
		}
		apiKey = strings.TrimSpace(string(raw))
	}
	if apiKey == "" {
		return fmt.Errorf("API key is required")
	}
	userRC := makeAdminRunCtx(s.rc.ServerURL, apiKey)
	var seeds []string
	if s.wsID != "" {
		// Pass id:name so the user REPL can set ws context without an admin API call.
		seed := "ws=" + s.wsID
		if s.wsName != "" {
			seed += ":" + s.wsName
		}
		seeds = append(seeds, seed)
	}
	fmt.Fprintln(os.Stderr, "# entering user REPL — Ctrl-D to return to admin")
	_ = runUserREPL(userRC, seeds)
	fmt.Fprintln(os.Stderr, "# back in admin REPL")
	return nil
}

func printReplHelp() {
	fmt.Print(`
Start with context (seed args):
  orange admin repl ws=<id>
  orange admin repl org=<id> proj=<id>

Navigation:
  use org <id>          set org context (clears proj/ws)
  use proj <id>         set project context (clears ws)
  use ws <id>           set workspace context
  unset <org|proj|ws>   clear context level
  context / ctx         show current context

Org:
  org ls                           list orgs
  org get [id]                     get org (defaults to current)
  org create <name>                create org
  org update [id] [description=…]  update description (defaults to current)
  org delete [id]                  delete org (defaults to current)

Project  (requires org context):
  proj ls                           list projects in current org
  proj get [id]                     get project
  proj create <name>                create project
  proj update [id] [description=…]  update description (defaults to current)
  proj delete [id]                  delete project

Workspace  (requires proj context):
  ws ls                             list workspaces in current project
  ws ls *                           list ALL workspaces across every org and project
  ws get [id]                       get workspace
  ws create <name>                  create workspace
  ws update [id] [description=…]    update description (defaults to current)
  ws delete [id]                    delete workspace

Member  (requires ws context):
  member ls             list workspace members (shows email)
  member add <user-id>  add member
  member rm <user-id>   remove member

User  (requires org context for ls/create):
  user ls                              list users in current org
  user get <id>                        get user and their active key scopes
  user create <email>                  create user in current org
  user update <id> [description=…]     update description
  user delete <id>                     delete user

Egress  (requires ws context):
  egress get            get egress for current workspace
  egress bundle [dir]   download egress bundle (default: .)
  egress status         show online/offline status

API Key  (requires org context for ls/issue; alias: key):
  apikey ls [user-id]                           list active keys (optionally filter by user)
  apikey get <key-id>                           show full key details (scopes, workspace, description)
  apikey scope <key-id>                         interactive scope editor (checkbox-style)
  apikey issue [user=<id>] [ws=<id>] [scope=<csv>] [template=ws-member] [desc=<text>]
                                                issue a new API key (plaintext shown once)
  apikey revoke <key-id>                        revoke an API key

Secret  (realm inferred from context; bare purpose expanded to ws/<id>/purpose when in ws):
  secret ls [realm-prefix]             list secrets; defaults to current scope when omitted
  secret set <purpose> <name>          create + enable in current scope (prompts, hidden input)
  secret set <realm> <name>            same but with explicit realm (ws/proj/org/<uuid>/<purpose>)
  secret get <realm> <name>            print active material to stdout
  secret versions <realm> <name>       list all versions
  secret enable  <realm> <name> <vid>  enable a specific version
  secret disable <realm> <name> <vid>  disable a specific version
  secret retire  <realm> <name> <vid>  retire a specific version (permanent)
  secret kek create [realm=<realm>]    provision a service KEK (pool member if realm omitted)
  secret kek rotate [realm=<realm>]    rotate service KEK(s) under the current master KEK

Config snapshots  (alias: cfg):
  config ls [<ws-id>]                  list published snapshots (defaults to ws context)
  config publish <file-path> [ws=<id>] [by=<who>]
                                       compile + publish a YAML config snapshot

Rate-limit tiers  (requires ws context; alias: rlt):
  rl-tier ls [<ws-id>]                 list tiers in workspace
  rl-tier get <name> [<ws-id>]         get tier detail (proto JSON)
  rl-tier create <name> [rpm=N rph=N rpd=N usd_per_day=N on_exceed=reject|throttle|log_only ...]
  rl-tier update <name> [rpm=N ...]    full replacement of all limit fields
  rl-tier delete <name> [<ws-id>]      delete tier (fails if referenced by a scope)

Rate-limit scopes  (requires ws context; alias: rls):
  rl-scope ls [<ws-id>]                list scopes in workspace
  rl-scope get [user=<id>]             get workspace or user scope
  rl-scope set [user=<id>] entry='<json>' [entry='<json>' ...]
                                       replace scope entries (single-quoted JSON, no spaces)
  rl-scope delete [user=<id>]          delete scope

  Entry JSON fields: models (array), rpm, rph, rpd, usd_per_day, tier_name, on_exceed
  Example: entry='{"models":["*"],"rpm":60,"on_exceed":"throttle"}'

Rate-limit policy  (floor/flexible; alias: rlp):
  rl-policy ls [scope-type=ws|proj|key] [scope-id=<id>] [type=floor|flexible]
  rl-policy get <policy-id>
  rl-policy create scope-type=ws scope-id=<id> type=floor [rule='<json>'] [desc='...']
  rl-policy update <policy-id> [rule='<json>'] [desc='...']
  rl-policy delete <policy-id>

  Rule JSON fields: models (array), rpm, rph, rpd, usd_per_day, on_exceed
  Example: rule='{"models":["claude-3-opus"],"rpm":10,"on_exceed":"reject"}'

Key entries / token slots  (alias: ke):
  keyentry ls [<ws-id>]                    list token slots in workspace
  keyentry get <key-entry-id>              get a token slot (proto JSON)
  keyentry create <name> user=<id> [ws=<id>] [desc=<text>]
                                           create a token slot for a user
  keyentry update <key-entry-id> [desc=<text>]
                                           update description
  keyentry delete <key-entry-id>           delete the slot

PASETO tokens  (alias: ket):
  keyentry-token ls <key-entry-id>               list tokens issued from a slot
  keyentry-token get <token-id>                  get token record
  keyentry-token issue <key-entry-id> [ttl=<s>]  issue an anonymous PASETO token (shown once)
  keyentry-token revoke <token-id>               revoke a token

Key secrets  (alias: kes):
  keyentry-secret ls <key-entry-id>                      list key secrets for a slot
  keyentry-secret get <key-secret-id>                    get a key secret record
  keyentry-secret create <key-entry-id> target=<upstream> [desc=<text>]
                                                  create (prompts for value, hidden)
  keyentry-secret rotate <key-secret-id>                 rotate (prompts for new value, hidden)
  keyentry-secret delete <key-secret-id>                 delete

Routing overrides  (alias: ker):
  keyentry-routing targets [<ws-id>]               list LLM providers available as routing targets
  keyentry-routing models <provider> [<ws-id>]     list backend model names for a provider
  keyentry-routing validate file=<path> [ws=<id>]  parse & preview rules locally (no server write)
  keyentry-routing get <key-entry-id>              show routing overrides
  keyentry-routing set <key-entry-id> file=<path>  replace from YAML (or .json)
  keyentry-routing set <key-entry-id> json='{"rules":[{"model":"...","target":"..."}]}'
  keyentry-routing delete <key-entry-id>           clear all overrides (reverts to workspace default)

  Rule fields: model (client-facing model ID), target (llm.providers key — run 'targets' to list valid values)
               backend_model (optional): overrides the model name sent to the upstream
  Advanced:    chain: [{target:A}, {target:B}]  ordered fallback (by provider or by backend_model)
               split: [{weight:80,target:A}, {weight:20,target:A,backend_model:B}]  A/B by provider or model

Profile  (requires ws context; alias: prof):
  profile ls [user-id]                         list profiles in current workspace
  profile get <profile-id>                     show profile details (tools, auth overrides)
  profile add <user-id> <name> [desc=<text>]   create profile (prints derived MCP path)
  profile rm <profile-id>                      delete profile

  MCP path is auto-derived as: slug(ws)--slug(user)--slug(name)
  Add it as 'path:' under the profile key in orange.yaml so the proxy can route /mcp/<path>.

Other:
  su [api-key]          switch to user REPL (prompts for API key if omitted); Ctrl-D to return
  help / ?              show this help
  exit / quit / Ctrl+D  exit REPL

`)
}
