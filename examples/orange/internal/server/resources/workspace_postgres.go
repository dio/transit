package resources

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/teris-io/shortid"
	"google.golang.org/protobuf/types/known/timestamppb"

	workspacev1 "github.com/dio/transit/examples/orange/api/orange/workspace/admin/v1"
	adminv1connect "github.com/dio/transit/examples/orange/api/orange/workspace/admin/v1/adminv1connect"
)

const maxSlugRetries = 5

const workspaceDDL = `
CREATE TABLE IF NOT EXISTS workspaces (
  workspace_id TEXT PRIMARY KEY,
  project_id   TEXT NOT NULL,
  name         TEXT NOT NULL,
  slug         TEXT NOT NULL DEFAULT '',
  description  TEXT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (project_id, name)
);
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS slug TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS workspaces_slug_idx ON workspaces (slug) WHERE slug != ''`

// WorkspaceService implements adminv1connect.WorkspaceAdminServiceHandler over PostgreSQL.
type WorkspaceService struct {
	adminv1connect.UnimplementedWorkspaceAdminServiceHandler

	pool      *pgxpool.Pool
	logger    *slog.Logger
	egressSvc *EgressService
}

// SetEgressService wires in the EgressService so CreateWorkspace can atomically
// provision the egress artefacts.
func (s *WorkspaceService) SetEgressService(es *EgressService) {
	s.egressSvc = es
}

// NewWorkspaceService creates a WorkspaceService and ensures the workspaces table exists.
func NewWorkspaceService(pool *pgxpool.Pool, logger *slog.Logger) *WorkspaceService {
	return &WorkspaceService{pool: pool, logger: logger}
}

// EnsureSchema creates the workspaces table and adds any missing columns.
func (s *WorkspaceService) EnsureSchema(ctx context.Context) error {
	for _, stmt := range splitDDL(workspaceDDL) {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// splitDDL splits a multi-statement DDL string on semicolons, skipping
// semicolons that appear inside -- line comments.
func splitDDL(sql string) []string {
	// Strip -- comments line by line before splitting on ';'.
	var stripped strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if idx := strings.Index(line, "--"); idx >= 0 {
			line = line[:idx]
		}
		stripped.WriteString(line)
		stripped.WriteByte('\n')
	}
	var out []string
	for _, s := range strings.Split(stripped.String(), ";") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// CreateWorkspace inserts a new workspace and returns the created record.
// When an EgressService is wired in, the egress and all its cryptographic
// artefacts are provisioned atomically within the same transaction.
func (s *WorkspaceService) CreateWorkspace(
	ctx context.Context,
	req *connect.Request[workspacev1.CreateWorkspaceRequest],
) (*connect.Response[workspacev1.CreateWorkspaceResponse], error) {
	workspaceID := uuid.Must(uuid.NewV7()).String()

	const query = `
INSERT INTO workspaces (workspace_id, project_id, name, slug, description)
VALUES ($1, $2, $3, $4, $5)
RETURNING workspace_id, project_id, name, slug, description, created_at, updated_at`

	// insertWorkspace tries the INSERT with a fresh shortid slug, retrying up
	// to maxSlugRetries times when the slug index collides.
	type wsRow struct {
		wID, pID, name, slug string
		desc                 *string
		createdAt            time.Time
		updatedAt            time.Time
	}
	type queryRower interface {
		QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	}
	insertWorkspace := func(qr queryRower) (wsRow, error) {
		for range maxSlugRetries {
			slug, err := shortid.Generate()
			if err != nil {
				return wsRow{}, fmt.Errorf("generate slug: %w", err)
			}
			var r wsRow
			err = qr.QueryRow(ctx, query,
				workspaceID, req.Msg.GetProjectId(), req.Msg.GetName(), slug, req.Msg.Description,
			).Scan(&r.wID, &r.pID, &r.name, &r.slug, &r.desc, &r.createdAt, &r.updatedAt)
			if err == nil {
				return r, nil
			}
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				if pgErr.ConstraintName == "workspaces_slug_idx" {
					continue // slug collision — try a new one
				}
				return wsRow{}, connect.NewError(connect.CodeAlreadyExists, err)
			}
			return wsRow{}, connect.NewError(connect.CodeInternal, err)
		}
		return wsRow{}, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to generate unique workspace slug after %d attempts", maxSlugRetries))
	}

	if s.egressSvc != nil {
		// Transactional path: workspace + egress in one atomic operation.
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck

		r, err := insertWorkspace(tx)
		if err != nil {
			return nil, err
		}

		eg, err := s.egressSvc.ProvisionForWorkspace(ctx, tx, workspaceID)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("provision egress: %w", err))
		}

		if err := tx.Commit(ctx); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}

		return connect.NewResponse(&workspacev1.CreateWorkspaceResponse{
			Workspace: &workspacev1.Workspace{
				WorkspaceId: r.wID,
				ProjectId:   r.pID,
				Name:        r.name,
				Slug:        wsSlug(r.slug),
				Description: r.desc,
				CreatedAt:   timestamppb.New(r.createdAt),
				UpdatedAt:   timestamppb.New(r.updatedAt),
				EgressId:    eg.EgressId,
			},
		}), nil
	}

	// Non-transactional path (no egress provisioning).
	r, err := insertWorkspace(s.pool)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&workspacev1.CreateWorkspaceResponse{
		Workspace: &workspacev1.Workspace{
			WorkspaceId: r.wID,
			ProjectId:   r.pID,
			Name:        r.name,
			Slug:        wsSlug(r.slug),
			Description: r.desc,
			CreatedAt:   timestamppb.New(r.createdAt),
			UpdatedAt:   timestamppb.New(r.updatedAt),
		},
	}), nil
}

// GetWorkspace retrieves a workspace by workspace_id.
func (s *WorkspaceService) GetWorkspace(
	ctx context.Context,
	req *connect.Request[workspacev1.GetWorkspaceRequest],
) (*connect.Response[workspacev1.GetWorkspaceResponse], error) {
	const query = `
SELECT w.workspace_id, w.project_id, w.name, COALESCE(w.slug,''), w.description, w.created_at, w.updated_at,
       COALESCE(e.egress_id, '')
FROM workspaces w
LEFT JOIN egresses e ON e.workspace_id = w.workspace_id
WHERE w.workspace_id = $1`

	var (
		wID, pID, name, slug, egressID string
		desc                           *string
		createdAt                      time.Time
		updatedAt                      time.Time
	)
	err := s.pool.QueryRow(ctx, query, req.Msg.GetWorkspaceId()).
		Scan(&wID, &pID, &name, &slug, &desc, &createdAt, &updatedAt, &egressID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&workspacev1.GetWorkspaceResponse{
		Workspace: &workspacev1.Workspace{
			WorkspaceId: wID,
			ProjectId:   pID,
			Name:        name,
			Slug:        wsSlug(slug),
			Description: desc,
			CreatedAt:   timestamppb.New(createdAt),
			UpdatedAt:   timestamppb.New(updatedAt),
			EgressId:    egressID,
		},
	}), nil
}

// ListWorkspaces returns workspaces belonging to a project, with keyset pagination.
func (s *WorkspaceService) ListWorkspaces(
	ctx context.Context,
	req *connect.Request[workspacev1.ListWorkspacesRequest],
) (*connect.Response[workspacev1.ListWorkspacesResponse], error) {
	limit := int(req.Msg.GetLimit())
	if limit <= 0 {
		limit = 100
	}

	var (
		rows pgx.Rows
		err  error
	)

	pageToken := req.Msg.GetPageToken()
	if pageToken != "" {
		const query = `
SELECT w.workspace_id, w.project_id, w.name, COALESCE(w.slug,''), w.description, w.created_at, w.updated_at,
       COALESCE(e.egress_id, '')
FROM workspaces w
LEFT JOIN egresses e ON e.workspace_id = w.workspace_id
WHERE w.project_id = $1 AND w.workspace_id > $2
ORDER BY w.workspace_id ASC
LIMIT $3`
		rows, err = s.pool.Query(ctx, query, req.Msg.GetProjectId(), pageToken, limit)
	} else {
		const query = `
SELECT w.workspace_id, w.project_id, w.name, COALESCE(w.slug,''), w.description, w.created_at, w.updated_at,
       COALESCE(e.egress_id, '')
FROM workspaces w
LEFT JOIN egresses e ON e.workspace_id = w.workspace_id
WHERE w.project_id = $1
ORDER BY w.workspace_id ASC
LIMIT $2`
		rows, err = s.pool.Query(ctx, query, req.Msg.GetProjectId(), limit)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer rows.Close()

	var workspaces []*workspacev1.Workspace
	for rows.Next() {
		var (
			wID, pID, name, slug, egressID string
			desc                           *string
			createdAt                      time.Time
			updatedAt                      time.Time
		)
		if err := rows.Scan(&wID, &pID, &name, &slug, &desc, &createdAt, &updatedAt, &egressID); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		workspaces = append(workspaces, &workspacev1.Workspace{
			WorkspaceId: wID,
			ProjectId:   pID,
			Name:        name,
			Slug:        wsSlug(slug),
			Description: desc,
			CreatedAt:   timestamppb.New(createdAt),
			UpdatedAt:   timestamppb.New(updatedAt),
			EgressId:    egressID,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	var nextPageToken string
	if len(workspaces) == limit {
		nextPageToken = workspaces[len(workspaces)-1].WorkspaceId
	}

	return connect.NewResponse(&workspacev1.ListWorkspacesResponse{
		Workspaces:    workspaces,
		NextPageToken: nextPageToken,
	}), nil
}

// UpdateWorkspace updates the description and updated_at of an existing workspace.
func (s *WorkspaceService) UpdateWorkspace(
	ctx context.Context,
	req *connect.Request[workspacev1.UpdateWorkspaceRequest],
) (*connect.Response[workspacev1.UpdateWorkspaceResponse], error) {
	const query = `
WITH updated AS (
  UPDATE workspaces
  SET description = $1, updated_at = now()
  WHERE workspace_id = $2
  RETURNING workspace_id, project_id, name, slug, description, created_at, updated_at
)
SELECT u.workspace_id, u.project_id, u.name, COALESCE(u.slug,''), u.description, u.created_at, u.updated_at,
       COALESCE(e.egress_id, '')
FROM updated u
LEFT JOIN egresses e ON e.workspace_id = u.workspace_id`

	var (
		wID, pID, name, slug, egressID string
		desc                           *string
		createdAt                      time.Time
		updatedAt                      time.Time
	)
	err := s.pool.QueryRow(ctx, query,
		req.Msg.Description,
		req.Msg.GetWorkspaceId(),
	).Scan(&wID, &pID, &name, &slug, &desc, &createdAt, &updatedAt, &egressID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&workspacev1.UpdateWorkspaceResponse{
		Workspace: &workspacev1.Workspace{
			WorkspaceId: wID,
			ProjectId:   pID,
			Name:        name,
			Slug:        wsSlug(slug),
			Description: desc,
			CreatedAt:   timestamppb.New(createdAt),
			UpdatedAt:   timestamppb.New(updatedAt),
			EgressId:    egressID,
		},
	}), nil
}

// DeleteWorkspace removes a workspace by workspace_id.
func (s *WorkspaceService) DeleteWorkspace(
	ctx context.Context,
	req *connect.Request[workspacev1.DeleteWorkspaceRequest],
) (*connect.Response[workspacev1.DeleteWorkspaceResponse], error) {
	result, err := s.pool.Exec(ctx,
		`DELETE FROM workspaces WHERE workspace_id = $1`,
		req.Msg.GetWorkspaceId(),
	)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if result.RowsAffected() == 0 {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("workspace not found"))
	}

	return connect.NewResponse(&workspacev1.DeleteWorkspaceResponse{}), nil
}

func wsSlug(raw string) string {
	if raw == "" {
		return ""
	}
	return "ws-" + raw
}
