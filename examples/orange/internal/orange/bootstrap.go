package orange

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

	projectv1 "github.com/dio/transit/examples/orange/api/orange/project/admin/v1"
	secretv1 "github.com/dio/transit/examples/orange/api/orange/secret/admin/v1"
	userv1 "github.com/dio/transit/examples/orange/api/orange/user/admin/v1"
	workspacev1 "github.com/dio/transit/examples/orange/api/orange/workspace/admin/v1"
	"github.com/dio/transit/examples/orange/internal/orange/apikeys"
	"github.com/dio/transit/examples/orange/internal/resources"
	"github.com/dio/transit/examples/orange/internal/secret"
	"github.com/dio/transit/examples/orange/internal/secret/crypto"
	"github.com/dio/transit/examples/orange/internal/secret/kms"
	secretstore "github.com/dio/transit/examples/orange/internal/secret/store"
)

func newBootstrapCmd() *cobra.Command {
	var (
		localMode bool
		purge     bool
		org       string
		email     string
		include   string
		port      string
		publicURL string
	)

	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Bootstrap a fresh orange installation",
		Long: `Bootstrap creates the first org, admin user, and API key on a fresh database.

Use --include to also scaffold the full resource hierarchy:

  orange bootstrap --local --org=acme
  orange bootstrap --local --org=acme --include=proj,ws
  orange bootstrap --local --org=acme --include=proj=myproj,ws=mywspace
  orange bootstrap --local --org=acme --include=proj,ws,usr=dio

Supported --include keys (comma-separated, each accepts an optional =name):
  proj[=<name>]   create a project (default name: "default")
  ws[=<name>]     create a workspace under proj (default name: "default")
  usr[=<name>]    create a non-admin user <name>@<org>, issue an API key,
                  and add them to the workspace if one was created
                  (default name: "default", producing default@<org>)

The command prints export lines for all created resources and exits.
Re-run 'orange server --local' to start the server.`,
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
	cmd.Flags().StringVar(&include, "include", "", "comma-separated resources to create: proj[=name],ws[=name]")
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
	port      string
	publicURL string
	logger    *slog.Logger
}

// includeSpec holds the parsed --include values.
type includeSpec struct {
	proj     bool
	projName string
	ws       bool
	wsName   string
	usr      bool
	usrName  string // local part of email; full email is usrName@org
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
	// workspace requires a project
	if spec.ws && !spec.proj {
		spec.proj = true
		spec.projName = "default"
	}
	return spec, nil
}

func stringOr(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

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
	userSvc := resources.NewUserService(pool, cfg.logger.With("component", "user"))
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

	keyStore, err := apikeys.NewStore(ctx, pool)
	if err != nil {
		return fmt.Errorf("init api key store: %w", err)
	}

	// ── Parse --include ────────────────────────────────────────────────────

	spec, err := parseInclude(cfg.include)
	if err != nil {
		return err
	}

	// ── Bootstrap: org + user + API key + KEK pool ─────────────────────────

	has, err := apikeys.HasOrgs(ctx, pool)
	if err != nil {
		return err
	}
	if has {
		return fmt.Errorf("orgs already exist — use 'orange server --local' to start, or --purge to reset")
	}

	email := cfg.email
	if email == "" {
		email = "admin@" + cfg.org
	}

	now := time.Now().UTC()
	orgID := uuid.Must(uuid.NewV7()).String()
	userID := uuid.Must(uuid.NewV7()).String()

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
		userID, orgID, email, now,
	); err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	plaintext, _, err := keyStore.Issue(ctx, orgID, userID, "", []string{apikeys.ScopeAdmin}, "bootstrap admin key")
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

	// ── Optional: project ──────────────────────────────────────────────────

	var projectID string
	if spec.proj {
		resp, err := projectSvc.CreateProject(ctx, connect.NewRequest(&projectv1.CreateProjectRequest{
			OrgId: orgID,
			Name:  spec.projName,
		}))
		if err != nil {
			return fmt.Errorf("create project %q: %w", spec.projName, err)
		}
		projectID = resp.Msg.GetProject().GetProjectId()
		cfg.logger.Info("created project", "name", spec.projName, "project_id", projectID)
	}

	// ── Optional: workspace ────────────────────────────────────────────────

	var workspaceID, egressID string
	if spec.ws {
		resp, err := workspaceSvc.CreateWorkspace(ctx, connect.NewRequest(&workspacev1.CreateWorkspaceRequest{
			ProjectId: projectID,
			Name:      spec.wsName,
		}))
		if err != nil {
			return fmt.Errorf("create workspace %q: %w", spec.wsName, err)
		}
		ws := resp.Msg.GetWorkspace()
		workspaceID = ws.GetWorkspaceId()
		egressID = ws.GetEgressId()
		cfg.logger.Info("created workspace", "name", spec.wsName, "workspace_id", workspaceID, "egress_id", egressID)
	}

	// ── Optional: extra user ───────────────────────────────────────────────

	var usrAPIKey, usrEmail, usrID string
	if spec.usr {
		usrEmail = spec.usrName + "@" + cfg.org
		resp, err := userSvc.CreateUser(ctx, connect.NewRequest(&userv1.CreateUserRequest{
			OrgId: orgID,
			Email: usrEmail,
		}))
		if err != nil {
			return fmt.Errorf("create user %q: %w", usrEmail, err)
		}
		usrID = resp.Msg.GetUser().GetUserId()
		cfg.logger.Info("created user", "email", usrEmail, "user_id", usrID)

		// Issue a non-admin API key for the user.
		usrAPIKey, _, err = keyStore.Issue(ctx, orgID, usrID, "", []string{apikeys.ScopeUser}, "bootstrap user key")
		if err != nil {
			return fmt.Errorf("issue user key for %q: %w", usrEmail, err)
		}

		// Assign to workspace if one was created.
		if workspaceID != "" {
			if _, err := userSvc.AddWorkspaceMember(ctx, connect.NewRequest(&userv1.AddWorkspaceMemberRequest{
				WorkspaceId: workspaceID,
				UserId:      usrID,
			})); err != nil {
				return fmt.Errorf("add %q to workspace: %w", usrEmail, err)
			}
			cfg.logger.Info("added user to workspace", "email", usrEmail, "workspace_id", workspaceID)
		}
	}

	// ── Print summary ──────────────────────────────────────────────────────

	fmt.Fprintf(os.Stdout, "# bootstrap complete\n")
	fmt.Fprintf(os.Stdout, "# org: %s  admin: %s\n", cfg.org, email)
	fmt.Fprintf(os.Stdout, "export ORANGE_API_KEY=%s\n", plaintext)
	fmt.Fprintf(os.Stdout, "export ORANGE_ORG=%s\n", cfg.org)
	fmt.Fprintf(os.Stdout, "export ORANGE_ORG_ID=%s\n", orgID)
	if projectID != "" {
		fmt.Fprintf(os.Stdout, "export ORANGE_PROJ_ID=%s\n", projectID)
	}
	if workspaceID != "" {
		fmt.Fprintf(os.Stdout, "export ORANGE_WS_ID=%s\n", workspaceID)
	}
	if egressID != "" {
		fmt.Fprintf(os.Stdout, "export ORANGE_EGRESS_ID=%s\n", egressID)
	}
	if usrAPIKey != "" {
		fmt.Fprintf(os.Stdout, "\n# user: %s\n", usrEmail)
		fmt.Fprintf(os.Stdout, "export ORANGE_USER_ID=%s\n", usrID)
		fmt.Fprintf(os.Stdout, "export ORANGE_USER_API_KEY=%s\n", usrAPIKey)
	}
	fmt.Fprintf(os.Stdout, "\n# start the server:\n#   orange server --local\n")

	return nil
}
