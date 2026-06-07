package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	egressadminv1 "github.com/dio/transit/examples/orange/api/orange/egress/admin/v1"
	projectv1 "github.com/dio/transit/examples/orange/api/orange/project/admin/v1"
	secretv1 "github.com/dio/transit/examples/orange/api/orange/secret/admin/v1"
	userv1 "github.com/dio/transit/examples/orange/api/orange/user/admin/v1"
	workspacev1 "github.com/dio/transit/examples/orange/api/orange/workspace/admin/v1"
	"github.com/dio/transit/examples/orange/internal/server/apikeys"
	"github.com/dio/transit/examples/orange/internal/server/resources"
	"github.com/dio/transit/examples/orange/internal/server/secret"
	"github.com/dio/transit/examples/orange/internal/server/secret/crypto"
	"github.com/dio/transit/examples/orange/internal/server/secret/kms"
	secretstore "github.com/dio/transit/examples/orange/internal/server/secret/store"
)

func newBootstrapCmd() *cobra.Command {
	var (
		localMode bool
		purge     bool
		org       string
		email     string
		include   string
		assign    string
		entries   string
		actions   string
		port      string
		publicURL string
	)

	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Bootstrap a fresh orange installation",
		Long: `Bootstrap creates the first org, admin user, and API key on a fresh database.

Use --include to scaffold resources with explicit keys:

  orange bootstrap --local --org=orange.io
  orange bootstrap --local --org=orange.io --include=proj,ws
  orange bootstrap --local --org=orange.io --include=proj=myproj,ws=mywspace
  orange bootstrap --local --org=orange.io --include=proj=proj1,ws=ws1,usr=dio --purge
  orange bootstrap --local --org=orange.io --entries=dio@proj1/ws1,kai@proj1/ws1,adi@proj2/ws2 --purge

Or use --entries for a compact notation (comma-separated tokens):

  orange bootstrap --local --org=orange.io \
    --entries=dio@proj1/ws1,kai@proj1/ws1,adi@proj2/ws2,proj3 --purge

  Token grammar:
    proj          → create project
    proj/ws       → create project + workspace
    usr@          → create user, no workspace assignment
    usr@proj/ws   → create user + project + workspace, assign user to ws

--include and --entries can be combined. --assign adds extra assignments on top.

Use --actions to run post-bootstrap operations on created resources:

  orange bootstrap --local --org=orange.io \
    --entries=dio@proj1/ws1,kai@proj1/ws1 \
    --actions=download:egress-bundle@ws1,download:egress-bundle@ws2

  Action grammar (comma-separated):
    download:egress-bundle@<wsName>  → write <egressID>.tar.gz in current directory`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if org == "" {
				return fmt.Errorf("--org is required")
			}
			if purge && !localMode {
				return fmt.Errorf("--purge is only available with --local")
			}
			logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
			return runBootstrap(cmd.Context(), bootstrapCfg{
				local:     localMode,
				purge:     purge,
				org:       org,
				email:     email,
				include:   include,
				assign:    assign,
				entries:   entries,
				actions:   actions,
				port:      port,
				publicURL: publicURL,
				logger:    logger,
			})
		},
	}

	cmd.Flags().BoolVar(&localMode, "local", false, "use embedded Postgres and auto-generate KEK in ~/.orange/")
	cmd.Flags().BoolVar(&purge, "purge", false, "purge ~/.orange/data and KEK before bootstrapping (requires --local)")
	cmd.Flags().StringVar(&org, "org", envOr("ORANGE_BOOTSTRAP_ORG", ""), "org name to bootstrap (required)")
	cmd.Flags().StringVar(&email, "email", envOr("ORANGE_BOOTSTRAP_EMAIL", ""), "admin email (default: admin@<org>)")
	cmd.Flags().StringVar(&include, "include", "", "comma-separated resources: proj[=name],ws[=name],usr[=name]")
	cmd.Flags().StringVar(&assign, "assign", "", "extra user:workspace assignments, e.g. dio:ws1,kai:ws1")
	cmd.Flags().StringVar(&entries, "entries", "", "compact resource spec, e.g. dio@proj1/ws1,kai@,proj2/ws2,proj3")
	cmd.Flags().StringVar(&actions, "actions", "", "post-bootstrap actions, e.g. download:egress-bundle@ws1,download:egress-bundle@ws2")
	cmd.Flags().StringVar(&port, "port", envOr("PORT", "8080"), "server port used as fallback for egress server_url")
	cmd.Flags().StringVar(&publicURL, "public-url", envOr("ORANGE_PUBLIC_URL", ""), "public URL written into egress bundles (env: ORANGE_PUBLIC_URL; default: http://localhost:<port>)")

	return cmd
}

type bootstrapCfg struct {
	local     bool
	purge     bool
	org       string
	email     string
	include   string
	assign    string
	entries   string
	actions   string
	port      string
	publicURL string
	logger    *slog.Logger
}

// ── Resource spec ──────────────────────────────────────────────────────────────

// resourceSpec is the deduplicated, ordered set of resources to create.
type resourceSpec struct {
	projOrder  []string          // project names, first-seen order
	wsOrder    []string          // workspace names, first-seen order
	usrOrder   []string          // user local names, first-seen order
	wsProj     map[string]string // wsName → projName
	autoAssign []assignPair      // user→ws memberships derived from flag co-occurrence
}

func newResourceSpec() resourceSpec {
	return resourceSpec{wsProj: map[string]string{}}
}

// assignPair is a single user→workspace membership to create.
type assignPair struct {
	usrName string
	wsName  string
}

// mergeSpecs merges b into a, deduplicating by name (first seen wins for proj/ws/usr).
func mergeSpecs(a, b resourceSpec) resourceSpec {
	out := newResourceSpec()
	seenProj := map[string]bool{}
	seenWs := map[string]bool{}
	seenUsr := map[string]bool{}

	addProj := func(name string) {
		if !seenProj[name] {
			seenProj[name] = true
			out.projOrder = append(out.projOrder, name)
		}
	}
	addWs := func(name, projName string) {
		if !seenWs[name] {
			seenWs[name] = true
			out.wsOrder = append(out.wsOrder, name)
			out.wsProj[name] = projName
		}
	}
	addUsr := func(name string) {
		if !seenUsr[name] {
			seenUsr[name] = true
			out.usrOrder = append(out.usrOrder, name)
		}
	}

	for _, p := range a.projOrder {
		addProj(p)
	}
	for _, w := range a.wsOrder {
		addWs(w, a.wsProj[w])
	}
	for _, u := range a.usrOrder {
		addUsr(u)
	}
	for _, p := range b.projOrder {
		addProj(p)
	}
	for _, w := range b.wsOrder {
		addWs(w, b.wsProj[w])
	}
	for _, u := range b.usrOrder {
		addUsr(u)
	}
	out.autoAssign = append(a.autoAssign, b.autoAssign...)
	return out
}

// ── --include parser ───────────────────────────────────────────────────────────

// includeSpec holds the parsed --include values (single flag, comma-separated keys).
type includeSpec struct {
	proj     bool
	projName string
	ws       bool
	wsName   string
	usr      bool
	usrName  string
}

func parseInclude(s string) (includeSpec, error) {
	var spec includeSpec
	if s == "" {
		return spec, nil
	}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, val, _ := strings.Cut(part, "=")
		switch key {
		case "proj":
			spec.proj = true
			spec.projName = stringOr(val, "default")
		case "ws":
			spec.ws = true
			spec.wsName = stringOr(val, "default")
		case "usr":
			spec.usr = true
			spec.usrName = stringOr(val, "default")
		default:
			return spec, fmt.Errorf("unknown --include key %q (valid: proj, ws, usr)", key)
		}
	}
	if spec.ws && !spec.proj {
		spec.proj = true
		spec.projName = "default"
	}
	return spec, nil
}

func includeToResourceSpec(inc includeSpec) resourceSpec {
	rs := newResourceSpec()
	if inc.proj {
		rs.projOrder = append(rs.projOrder, inc.projName)
	}
	if inc.ws {
		rs.wsOrder = append(rs.wsOrder, inc.wsName)
		rs.wsProj[inc.wsName] = inc.projName
	}
	if inc.usr {
		rs.usrOrder = append(rs.usrOrder, inc.usrName)
		if inc.ws {
			// user co-occurs with a workspace → auto-assign
			rs.autoAssign = append(rs.autoAssign, assignPair{usrName: inc.usrName, wsName: inc.wsName})
		}
	}
	return rs
}

// ── --entries parser ───────────────────────────────────────────────────────────

// parseEntries parses the --entries flag into a resourceSpec.
//
// Token grammar (comma-separated):
//
//	proj          → create project
//	proj/ws       → create project + workspace
//	usr@          → create user, no assignment
//	usr@proj/ws   → create user + project + workspace, assign user to ws
func parseEntries(s string) (resourceSpec, error) {
	rs := newResourceSpec()
	if s == "" {
		return rs, nil
	}
	seenProj := map[string]bool{}
	seenWs := map[string]bool{}
	seenUsr := map[string]bool{}

	addProj := func(name string) {
		if !seenProj[name] {
			seenProj[name] = true
			rs.projOrder = append(rs.projOrder, name)
		}
	}
	addWs := func(name, projName string) {
		if !seenWs[name] {
			seenWs[name] = true
			rs.wsOrder = append(rs.wsOrder, name)
			rs.wsProj[name] = projName
		}
	}
	addUsr := func(name string) {
		if !seenUsr[name] {
			seenUsr[name] = true
			rs.usrOrder = append(rs.usrOrder, name)
		}
	}

	for _, token := range strings.Split(s, ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		if atIdx := strings.Index(token, "@"); atIdx >= 0 {
			// usr@ or usr@proj/ws
			usrName := token[:atIdx]
			rest := token[atIdx+1:]
			if usrName == "" {
				return rs, fmt.Errorf("--entries token %q: user name cannot be empty before @", token)
			}
			addUsr(usrName)
			if rest != "" {
				projName, wsName, ok := strings.Cut(rest, "/")
				if !ok || projName == "" || wsName == "" {
					return rs, fmt.Errorf("--entries token %q: workspace reference must be proj/ws", token)
				}
				addProj(projName)
				addWs(wsName, projName)
				rs.autoAssign = append(rs.autoAssign, assignPair{usrName: usrName, wsName: wsName})
			}
		} else if projName, wsName, ok := strings.Cut(token, "/"); ok {
			// proj/ws
			if projName == "" || wsName == "" {
				return rs, fmt.Errorf("--entries token %q: both project and workspace names are required", token)
			}
			addProj(projName)
			addWs(wsName, projName)
		} else {
			// proj only
			addProj(token)
		}
	}
	return rs, nil
}

// ── --assign parser ────────────────────────────────────────────────────────────

func parseAssign(s string) ([]assignPair, error) {
	if s == "" {
		return nil, nil
	}
	var pairs []assignPair
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		u, w, ok := strings.Cut(part, ":")
		if !ok || u == "" || w == "" {
			return nil, fmt.Errorf("--assign %q: expected user:workspace format", part)
		}
		pairs = append(pairs, assignPair{usrName: u, wsName: w})
	}
	return pairs, nil
}

// ── --actions parser ───────────────────────────────────────────────────────────

// bootstrapAction is one post-bootstrap operation.
// Format: verb:resource@target  (e.g. download:egress-bundle@ws1).
type bootstrapAction struct {
	verb     string // e.g. "download"
	resource string // e.g. "egress-bundle"
	target   string // workspace name as given in --entries or --include
}

func parseActions(s string) ([]bootstrapAction, error) {
	if s == "" {
		return nil, nil
	}
	var out []bootstrapAction
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		verb, rest, ok := strings.Cut(part, ":")
		if !ok {
			return nil, fmt.Errorf("--actions token %q: expected verb:resource@target", part)
		}
		resource, target, ok := strings.Cut(rest, "@")
		if !ok {
			return nil, fmt.Errorf("--actions token %q: expected resource@target after %q:", part, verb)
		}
		if verb == "" || resource == "" || target == "" {
			return nil, fmt.Errorf("--actions token %q: verb, resource, and target must all be non-empty", part)
		}
		out = append(out, bootstrapAction{verb: verb, resource: resource, target: target})
	}
	return out, nil
}

// ── Helpers ────────────────────────────────────────────────────────────────────

func stringOr(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

// envLabel uppercases a resource name for use as an env var suffix.
func envLabel(s string) string {
	return strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(s))
}

// ── runBootstrap ───────────────────────────────────────────────────────────────

func runBootstrap(parent context.Context, cfg bootstrapCfg) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	if cfg.purge {
		if err := purgeLocalData(cfg.logger); err != nil {
			return err
		}
	}

	masterKEKURI := os.Getenv("MASTER_KEK_URI")
	if cfg.local {
		uri, err := resolveLocalKEK(cfg.logger)
		if err != nil {
			return fmt.Errorf("local KEK: %w", err)
		}
		masterKEKURI = uri
	}
	if masterKEKURI == "" {
		return fmt.Errorf("MASTER_KEK_URI is required (or use --local)")
	}

	storeDSN, cleanup, err := startEmbeddedOrExternal(ctx, cfg.local, cfg.logger)
	if err != nil {
		return fmt.Errorf("store backend: %w", err)
	}
	defer cleanup()

	pool, err := pgxpool.New(ctx, storeDSN)
	if err != nil {
		return fmt.Errorf("pgx pool: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("pgx ping: %w", err)
	}

	// ── Secret service ─────────────────────────────────────────────────────

	st, err := secretstore.NewPGSecretStore(ctx, pool)
	if err != nil {
		return fmt.Errorf("init secret store: %w", err)
	}
	provider, err := kms.Load(ctx, masterKEKURI)
	if err != nil {
		return fmt.Errorf("init KMS: %w", err)
	}
	secretSvc, err := secret.New(ctx, secret.Config{
		Provider:  provider,
		Encryptor: crypto.New(),
		Store:     st,
		Logger:    cfg.logger.With("component", "secret"),
	})
	if err != nil {
		return fmt.Errorf("init secret service: %w", err)
	}

	// ── Resource services (schemas only; no HTTP server) ───────────────────

	orgSvc, err := resources.NewOrgService(pool, cfg.logger.With("component", "org"))
	if err != nil {
		return fmt.Errorf("init org service: %w", err)
	}
	_ = orgSvc // used via raw SQL in bootstrap; kept here to ensure schema

	projectSvc, err := resources.NewProjectService(pool, cfg.logger.With("component", "project"))
	if err != nil {
		return fmt.Errorf("init project service: %w", err)
	}
	workspaceSvc := resources.NewWorkspaceService(pool, cfg.logger.With("component", "workspace"))
	if err := workspaceSvc.EnsureSchema(ctx); err != nil {
		return fmt.Errorf("init workspace schema: %w", err)
	}

	keyStore, err := apikeys.NewStore(ctx, pool)
	if err != nil {
		return fmt.Errorf("init api key store: %w", err)
	}

	userSvc := resources.NewUserService(pool, cfg.logger.With("component", "user"), keyStore)
	if err := userSvc.EnsureSchema(ctx); err != nil {
		return fmt.Errorf("init user schema: %w", err)
	}
	serverURL := cfg.publicURL
	if serverURL == "" {
		serverURL = "http://localhost:" + cfg.port
	}
	egressSvc, err := resources.NewEgressService(pool, cfg.logger.With("component", "egress"), serverURL, secretSvc)
	if err != nil {
		return fmt.Errorf("init egress service: %w", err)
	}
	workspaceSvc.SetEgressService(egressSvc)

	// ── Parse flags into a merged resource spec ────────────────────────────

	inclSpec, err := parseInclude(cfg.include)
	if err != nil {
		return err
	}
	spec := includeToResourceSpec(inclSpec)

	if cfg.entries != "" {
		entriesSpec, err := parseEntries(cfg.entries)
		if err != nil {
			return err
		}
		spec = mergeSpecs(spec, entriesSpec)
	}

	// --assign is additive on top of auto-assign derived from flag co-occurrence
	extraAssign, err := parseAssign(cfg.assign)
	if err != nil {
		return err
	}
	allAssign := append(spec.autoAssign, extraAssign...)

	// Parse --actions early so bad syntax is caught before any DB writes.
	acts, err := parseActions(cfg.actions)
	if err != nil {
		return err
	}

	// ── Bootstrap: org + admin user + API key + KEK pool ──────────────────

	has, err := apikeys.HasOrgs(ctx, pool)
	if err != nil {
		return err
	}
	if has {
		return fmt.Errorf("orgs already exist — use 'orange server --local' to start, or --purge to reset")
	}

	adminEmail := cfg.email
	if adminEmail == "" {
		adminEmail = "admin@" + cfg.org
	}

	now := time.Now().UTC()
	orgID := uuid.Must(uuid.NewV7()).String()
	adminID := uuid.Must(uuid.NewV7()).String()

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err = tx.Exec(ctx,
		`INSERT INTO orgs (org_id, name, created_at, updated_at) VALUES ($1, $2, $3, $3)`,
		orgID, cfg.org, now,
	); err != nil {
		return fmt.Errorf("create org: %w", err)
	}
	if _, err = tx.Exec(ctx,
		`INSERT INTO users (user_id, org_id, email, created_at, updated_at) VALUES ($1, $2, $3, $4, $4)`,
		adminID, orgID, adminEmail, now,
	); err != nil {
		return fmt.Errorf("create admin user: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	plaintext, _, err := keyStore.Issue(ctx, orgID, adminID, "", []string{apikeys.ScopeOrgAdmin}, "bootstrap admin key")
	if err != nil {
		return fmt.Errorf("issue bootstrap key: %w", err)
	}

	const kekPoolSize = 3
	for i := range kekPoolSize {
		resp, err := secretSvc.CreateServiceKEK(ctx, connect.NewRequest(&secretv1.CreateServiceKEKRequest{}))
		if err != nil {
			return fmt.Errorf("seed kek pool [%d]: %w", i, err)
		}
		cfg.logger.Info("seeded service KEK pool member", "kek_id", resp.Msg.GetKekId())
	}

	// ── Projects ───────────────────────────────────────────────────────────

	projIDs := map[string]string{} // projName → projectID
	for _, pName := range spec.projOrder {
		resp, err := projectSvc.CreateProject(ctx, connect.NewRequest(&projectv1.CreateProjectRequest{
			OrgId: orgID,
			Name:  pName,
		}))
		if err != nil {
			return fmt.Errorf("create project %q: %w", pName, err)
		}
		projIDs[pName] = resp.Msg.GetProject().GetProjectId()
		cfg.logger.Info("created project", "name", pName, "project_id", projIDs[pName])
	}

	// ── Workspaces ─────────────────────────────────────────────────────────

	type wsInfo struct{ id, egressID string }
	wsInfos := map[string]wsInfo{} // wsName → info
	for _, wName := range spec.wsOrder {
		pName := spec.wsProj[wName]
		resp, err := workspaceSvc.CreateWorkspace(ctx, connect.NewRequest(&workspacev1.CreateWorkspaceRequest{
			ProjectId: projIDs[pName],
			Name:      wName,
		}))
		if err != nil {
			return fmt.Errorf("create workspace %q: %w", wName, err)
		}
		ws := resp.Msg.GetWorkspace()
		wsInfos[wName] = wsInfo{id: ws.GetWorkspaceId(), egressID: ws.GetEgressId()}
		cfg.logger.Info("created workspace", "name", wName, "workspace_id", ws.GetWorkspaceId(), "egress_id", ws.GetEgressId())
	}

	// ── Users ──────────────────────────────────────────────────────────────

	type usrInfo struct{ id, apiKey, email string }
	usrInfos := map[string]usrInfo{} // usrName → info
	for _, uName := range spec.usrOrder {
		uEmail := uName + "@" + cfg.org
		resp, err := userSvc.CreateUser(ctx, connect.NewRequest(&userv1.CreateUserRequest{
			OrgId: orgID,
			Email: uEmail,
		}))
		if err != nil {
			return fmt.Errorf("create user %q: %w", uEmail, err)
		}
		uid := resp.Msg.GetUser().GetUserId()
		apiKey, _, err := keyStore.Issue(ctx, orgID, uid, "", []string{apikeys.ScopeUserRead}, "bootstrap user key")
		if err != nil {
			return fmt.Errorf("issue user key for %q: %w", uEmail, err)
		}
		usrInfos[uName] = usrInfo{id: uid, apiKey: apiKey, email: uEmail}
		cfg.logger.Info("created user", "email", uEmail, "user_id", uid)
	}

	// ── Assignments ────────────────────────────────────────────────────────

	for _, pair := range allAssign {
		ui, ok := usrInfos[pair.usrName]
		if !ok {
			return fmt.Errorf("--assign: unknown user %q (not created in this bootstrap run)", pair.usrName)
		}
		wi, ok := wsInfos[pair.wsName]
		if !ok {
			return fmt.Errorf("--assign: unknown workspace %q (not created in this bootstrap run)", pair.wsName)
		}
		if _, err := userSvc.AddWorkspaceMember(ctx, connect.NewRequest(&userv1.AddWorkspaceMemberRequest{
			WorkspaceId: wi.id,
			UserId:      ui.id,
		})); err != nil {
			return fmt.Errorf("assign %q to workspace %q: %w", pair.usrName, pair.wsName, err)
		}
		// AddWorkspaceMember atomically supersedes the user's existing key(s) with
		// workspace-scoped permissions (secret:read, secret:write, token:issue).
		cfg.logger.Info("assigned user to workspace and updated key scopes",
			"user", pair.usrName, "workspace", pair.wsName)
	}

	// ── Execute post-bootstrap actions ────────────────────────────────────────

	// bundlePaths collects wsName→filePath for egress-bundle downloads so they
	// appear in the env-var summary below.
	bundlePaths := map[string]string{}
	for _, a := range acts {
		switch a.verb + ":" + a.resource {
		case "download:egress-bundle":
			wi, ok := wsInfos[a.target]
			if !ok {
				return fmt.Errorf("action download:egress-bundle@%s: unknown workspace %q — must be listed in --entries or --include", a.target, a.target)
			}
			resp, err := egressSvc.GetEgressBundle(ctx, connect.NewRequest(&egressadminv1.GetEgressBundleRequest{EgressId: wi.egressID}))
			if err != nil {
				return fmt.Errorf("download egress bundle for workspace %q: %w", a.target, err)
			}
			outPath := wi.egressID + ".tar.gz"
			if err := writeBundleTarGz(outPath, bundleFiles(resp.Msg.GetBundle())); err != nil {
				return fmt.Errorf("write egress bundle for workspace %q: %w", a.target, err)
			}
			bundlePaths[a.target] = outPath
			cfg.logger.Info("downloaded egress bundle", "workspace", a.target, "egress_id", wi.egressID, "path", outPath)
		default:
			return fmt.Errorf("unknown action %q — supported: download:egress-bundle", a.verb+":"+a.resource)
		}
	}

	// ── Print summary ──────────────────────────────────────────────────────

	fmt.Fprintf(os.Stdout, "# bootstrap complete\n")
	fmt.Fprintf(os.Stdout, "# org: %s  admin: %s\n", cfg.org, adminEmail)
	fmt.Fprintf(os.Stdout, "export ORANGE_API_KEY=%s\n", plaintext)
	fmt.Fprintf(os.Stdout, "export ORANGE_ORG=%s\n", cfg.org)
	fmt.Fprintf(os.Stdout, "export ORANGE_ORG_ID=%s\n", orgID)

	for _, pName := range spec.projOrder {
		fmt.Fprintf(os.Stdout, "\n# project %s\n", pName)
		fmt.Fprintf(os.Stdout, "export ORANGE_PROJ_ID_%s=%s\n", envLabel(pName), projIDs[pName])
	}
	for _, wName := range spec.wsOrder {
		wi := wsInfos[wName]
		fmt.Fprintf(os.Stdout, "\n# workspace %s  (proj: %s)\n", wName, spec.wsProj[wName])
		fmt.Fprintf(os.Stdout, "export ORANGE_WS_ID_%s=%s\n", envLabel(wName), wi.id)
		fmt.Fprintf(os.Stdout, "export ORANGE_EGRESS_ID_%s=%s\n", envLabel(wName), wi.egressID)
		if p, ok := bundlePaths[wName]; ok {
			fmt.Fprintf(os.Stdout, "export ORANGE_EGRESS_BUNDLE_%s=%s\n", envLabel(wName), p)
		}
	}
	for _, uName := range spec.usrOrder {
		ui := usrInfos[uName]
		fmt.Fprintf(os.Stdout, "\n# user %s\n", ui.email)
		fmt.Fprintf(os.Stdout, "export ORANGE_USER_ID_%s=%s\n", envLabel(uName), ui.id)
		fmt.Fprintf(os.Stdout, "export ORANGE_USER_API_KEY_%s=%s\n", envLabel(uName), ui.apiKey)
	}

	fmt.Fprintf(os.Stdout, "\n# start the server:\n#   orange server --local\n")
	return nil
}
