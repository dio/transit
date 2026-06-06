package resources

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	workspacev1 "github.com/dio/transit/examples/orange/api/orange/workspace/admin/v1"
	adminv1connect "github.com/dio/transit/examples/orange/api/orange/workspace/admin/v1/adminv1connect"
)

const workspaceDDL = `
CREATE TABLE IF NOT EXISTS workspaces (
  workspace_id TEXT PRIMARY KEY,
  project_id   TEXT NOT NULL,
  name         TEXT NOT NULL,
  description  TEXT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (project_id, name)
)`

// WorkspaceService implements adminv1connect.WorkspaceAdminServiceHandler over PostgreSQL.
type WorkspaceService struct {
	adminv1connect.UnimplementedWorkspaceAdminServiceHandler

	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewWorkspaceService creates a WorkspaceService and ensures the workspaces table exists.
func NewWorkspaceService(pool *pgxpool.Pool, logger *slog.Logger) *WorkspaceService {
	return &WorkspaceService{pool: pool, logger: logger}
}

// EnsureSchema creates the workspaces table if it does not exist.
func (s *WorkspaceService) EnsureSchema(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, workspaceDDL)
	return err
}

// CreateWorkspace inserts a new workspace and returns the created record.
func (s *WorkspaceService) CreateWorkspace(
	ctx context.Context,
	req *connect.Request[workspacev1.CreateWorkspaceRequest],
) (*connect.Response[workspacev1.CreateWorkspaceResponse], error) {
	workspaceID := uuid.Must(uuid.NewV7()).String()

	const query = `
INSERT INTO workspaces (workspace_id, project_id, name, description)
VALUES ($1, $2, $3, $4)
RETURNING workspace_id, project_id, name, description, created_at, updated_at`

	var (
		wID, pID, name string
		desc            *string
		createdAt       time.Time
		updatedAt       time.Time
	)
	err := s.pool.QueryRow(ctx, query,
		workspaceID,
		req.Msg.GetProjectId(),
		req.Msg.GetName(),
		req.Msg.Description,
	).Scan(&wID, &pID, &name, &desc, &createdAt, &updatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, connect.NewError(connect.CodeAlreadyExists, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&workspacev1.CreateWorkspaceResponse{
		Workspace: &workspacev1.Workspace{
			WorkspaceId: wID,
			ProjectId:   pID,
			Name:        name,
			Description: desc,
			CreatedAt:   timestamppb.New(createdAt),
			UpdatedAt:   timestamppb.New(updatedAt),
		},
	}), nil
}

// GetWorkspace retrieves a workspace by workspace_id.
func (s *WorkspaceService) GetWorkspace(
	ctx context.Context,
	req *connect.Request[workspacev1.GetWorkspaceRequest],
) (*connect.Response[workspacev1.GetWorkspaceResponse], error) {
	const query = `
SELECT workspace_id, project_id, name, description, created_at, updated_at
FROM workspaces
WHERE workspace_id = $1`

	var (
		wID, pID, name string
		desc            *string
		createdAt       time.Time
		updatedAt       time.Time
	)
	err := s.pool.QueryRow(ctx, query, req.Msg.GetWorkspaceId()).
		Scan(&wID, &pID, &name, &desc, &createdAt, &updatedAt)
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
			Description: desc,
			CreatedAt:   timestamppb.New(createdAt),
			UpdatedAt:   timestamppb.New(updatedAt),
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
SELECT workspace_id, project_id, name, description, created_at, updated_at
FROM workspaces
WHERE project_id = $1 AND workspace_id > $2
ORDER BY workspace_id ASC
LIMIT $3`
		rows, err = s.pool.Query(ctx, query, req.Msg.GetProjectId(), pageToken, limit)
	} else {
		const query = `
SELECT workspace_id, project_id, name, description, created_at, updated_at
FROM workspaces
WHERE project_id = $1
ORDER BY workspace_id ASC
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
			wID, pID, name string
			desc            *string
			createdAt       time.Time
			updatedAt       time.Time
		)
		if err := rows.Scan(&wID, &pID, &name, &desc, &createdAt, &updatedAt); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		workspaces = append(workspaces, &workspacev1.Workspace{
			WorkspaceId: wID,
			ProjectId:   pID,
			Name:        name,
			Description: desc,
			CreatedAt:   timestamppb.New(createdAt),
			UpdatedAt:   timestamppb.New(updatedAt),
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
UPDATE workspaces
SET description = $1, updated_at = now()
WHERE workspace_id = $2
RETURNING workspace_id, project_id, name, description, created_at, updated_at`

	var (
		wID, pID, name string
		desc            *string
		createdAt       time.Time
		updatedAt       time.Time
	)
	err := s.pool.QueryRow(ctx, query,
		req.Msg.Description,
		req.Msg.GetWorkspaceId(),
	).Scan(&wID, &pID, &name, &desc, &createdAt, &updatedAt)
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
			Description: desc,
			CreatedAt:   timestamppb.New(createdAt),
			UpdatedAt:   timestamppb.New(updatedAt),
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
