package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"

	configadminv1 "github.com/dio/transit/examples/orange/api/orange/config/admin/v1"
	projectv1 "github.com/dio/transit/examples/orange/api/orange/project/admin/v1"
	secretv1 "github.com/dio/transit/examples/orange/api/orange/secret/admin/v1"
	userv1 "github.com/dio/transit/examples/orange/api/orange/user/admin/v1"
	workspacev1 "github.com/dio/transit/examples/orange/api/orange/workspace/admin/v1"
	"github.com/dio/transit/examples/orange/internal/server/apikeys"
	"github.com/dio/transit/examples/orange/internal/server/resources"
	"github.com/dio/transit/examples/orange/internal/server/secret"
	"github.com/dio/transit/examples/orange/internal/vtprotocodec"
)

// localBootstrapSvc groups the already-initialised services needed for an
// in-process bootstrap driven by orange.yaml.
type localBootstrapSvc struct {
	pool         *pgxpool.Pool
	keyStore     *apikeys.Store
	projectSvc   *resources.ProjectService
	workspaceSvc *resources.WorkspaceService
	userSvc      *resources.UserService
	secretSvc    *secret.Service
	configSvc    *resources.ConfigService
}

// localBootstrapResult holds the IDs and credentials created by bootstrapFromYAML.
type localBootstrapResult struct {
	orgID      string
	orgName    string
	projID     string
	projName   string
	adminKey   string
	adminEmail string
	userKey    string
	userEmail  string
}

// yamlKeysOnly is a minimal parse target — we only need the key names from the
// top-level "keys:" map to derive workspace names.
type yamlKeysOnly struct {
	Keys map[string]any `yaml:"keys"`
}

// bootstrapFromYAML reads configPath, extracts workspace names from the keys:
// map, then creates org / project / workspaces / user and assigns memberships
// in-process using the already-started services.
func bootstrapFromYAML(ctx context.Context, cfg serverCfg, svc localBootstrapSvc) (localBootstrapResult, error) {
	var res localBootstrapResult

	data, err := os.ReadFile(cfg.configPath)
	if err != nil {
		return res, fmt.Errorf("read bootstrap config %q: %w", cfg.configPath, err)
	}
	var yk yamlKeysOnly
	if err := yaml.Unmarshal(data, &yk); err != nil {
		return res, fmt.Errorf("parse bootstrap config %q: %w", cfg.configPath, err)
	}

	// Key format is workspace/user/slug (e.g. "demo/dio/sk-default").
	// Derive unique workspace names from the first path component.
	wsNameSet := make(map[string]bool)
	for keyName := range yk.Keys {
		if ws, _, ok := strings.Cut(keyName, "/"); ok && ws != "" {
			wsNameSet[ws] = true
		} else if keyName != "" {
			// bare key name — treat the whole thing as workspace
			wsNameSet[keyName] = true
		}
	}
	var wsNames []string
	for ws := range wsNameSet {
		wsNames = append(wsNames, ws)
	}
	sort.Strings(wsNames)

	has, err := apikeys.HasOrgs(ctx, svc.pool)
	if err != nil {
		return res, err
	}
	if has {
		return res, fmt.Errorf("org already exists — use --no-purge to attach to existing data")
	}

	const kekPoolSize = 3
	for i := range kekPoolSize {
		resp, err := svc.secretSvc.CreateServiceKEK(ctx, connect.NewRequest(&secretv1.CreateServiceKEKRequest{}))
		if err != nil {
			return res, fmt.Errorf("seed kek pool [%d]: %w", i, err)
		}
		cfg.logger.Info("seeded service KEK pool member", "kek_id", resp.Msg.GetKekId())
	}

	now := time.Now().UTC()
	orgID := uuid.Must(uuid.NewV7()).String()
	adminID := uuid.Must(uuid.NewV7()).String()
	adminEmail := "admin@" + cfg.org

	tx, err := svc.pool.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err = tx.Exec(ctx,
		`INSERT INTO orgs (org_id, name, created_at, updated_at) VALUES ($1, $2, $3, $3)`,
		orgID, cfg.org, now,
	); err != nil {
		return res, fmt.Errorf("create org: %w", err)
	}
	if _, err = tx.Exec(ctx,
		`INSERT INTO users (user_id, org_id, email, created_at, updated_at) VALUES ($1, $2, $3, $4, $4)`,
		adminID, orgID, adminEmail, now,
	); err != nil {
		return res, fmt.Errorf("create admin user: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return res, err
	}

	adminKey, _, err := svc.keyStore.Issue(ctx, orgID, adminID, "", []string{apikeys.ScopeOrgAdmin}, "local-dev admin key")
	if err != nil {
		return res, fmt.Errorf("issue admin key: %w", err)
	}

	projResp, err := svc.projectSvc.CreateProject(ctx, connect.NewRequest(&projectv1.CreateProjectRequest{
		OrgId: orgID,
		Name:  cfg.project,
	}))
	if err != nil {
		return res, fmt.Errorf("create project %q: %w", cfg.project, err)
	}
	projID := projResp.Msg.GetProject().GetProjectId()
	cfg.logger.Info("created project", "name", cfg.project, "project_id", projID)

	wsIDs := make(map[string]string, len(wsNames))
	for _, wsName := range wsNames {
		wsResp, err := svc.workspaceSvc.CreateWorkspace(ctx, connect.NewRequest(&workspacev1.CreateWorkspaceRequest{
			ProjectId: projID,
			Name:      wsName,
		}))
		if err != nil {
			return res, fmt.Errorf("create workspace %q: %w", wsName, err)
		}
		ws := wsResp.Msg.GetWorkspace()
		wsIDs[wsName] = ws.GetWorkspaceId()
		cfg.logger.Info("created workspace", "name", wsName, "workspace_id", ws.GetWorkspaceId(), "egress_id", ws.GetEgressId())
	}

	userEmail := cfg.user + "@" + cfg.org
	uResp, err := svc.userSvc.CreateUser(ctx, connect.NewRequest(&userv1.CreateUserRequest{
		OrgId: orgID,
		Email: userEmail,
	}))
	if err != nil {
		return res, fmt.Errorf("create user %q: %w", userEmail, err)
	}
	uid := uResp.Msg.GetUser().GetUserId()
	cfg.logger.Info("created user", "email", userEmail, "user_id", uid)

	userKey, _, err := svc.keyStore.Issue(ctx, orgID, uid, "", apikeys.DefaultUserScopes, cfg.user+" bootstrap key")
	if err != nil {
		return res, fmt.Errorf("issue user key for %q: %w", userEmail, err)
	}

	for _, wsName := range wsNames {
		if _, err := svc.userSvc.AddWorkspaceMember(ctx, connect.NewRequest(&userv1.AddWorkspaceMemberRequest{
			WorkspaceId: wsIDs[wsName],
			UserId:      uid,
		})); err != nil {
			return res, fmt.Errorf("assign %q to workspace %q: %w", cfg.user, wsName, err)
		}
		cfg.logger.Info("assigned user to workspace", "user", cfg.user, "workspace", wsName)
	}

	// Auto-publish the local orange.yaml as the initial config snapshot for
	// every workspace so that "egress serve --local --bundle" can fetch config
	// immediately without a manual "config publish" step in the REPL.
	if svc.configSvc != nil {
		for _, wsName := range wsNames {
			wsID := wsIDs[wsName]
			_, err := svc.configSvc.PublishSnapshot(ctx, connect.NewRequest(&configadminv1.PublishSnapshotRequest{
				WorkspaceId: wsID,
				YamlConfig:  string(data),
				PublishedBy: adminEmail,
			}))
			if err != nil {
				cfg.logger.Warn("auto-publish snapshot failed", "workspace", wsName, "err", err)
			} else {
				cfg.logger.Info("auto-published config snapshot", "workspace", wsName, "workspace_id", wsID)
			}
		}
	}

	res = localBootstrapResult{
		orgID:      orgID,
		orgName:    cfg.org,
		projID:     projID,
		projName:   cfg.project,
		adminKey:   adminKey,
		adminEmail: adminEmail,
		userKey:    userKey,
		userEmail:  userEmail,
	}
	return res, nil
}

// makeAdminRunCtx builds a RunCtx suitable for the embedded admin REPL.
func makeAdminRunCtx(serverURL, apiKey string) *RunCtx {
	httpClient := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &bearerTransport{key: apiKey, base: http.DefaultTransport},
	}
	return &RunCtx{
		Printer:     &Printer{Format: FormatTable, Out: os.Stdout},
		ConnectOpts: []connect.ClientOption{connect.WithCodec(vtprotocodec.Codec{})},
		HTTPClient:  httpClient,
		ServerURL:   serverURL,
		APIKey:      apiKey,
	}
}

// waitForServer polls /healthz until the server responds 200 or ctx is done.
func waitForServer(ctx context.Context, serverURL string) error {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		resp, err := client.Get(serverURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("server at %s did not become ready within 10s", serverURL)
}
