package resources

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/teris-io/shortid"
	"google.golang.org/protobuf/types/known/timestamppb"

	projectv1 "github.com/dio/transit/examples/orange/api/orange/project/admin/v1"
	adminv1connect "github.com/dio/transit/examples/orange/api/orange/project/admin/v1/adminv1connect"
)

// ProjectService implements adminv1connect.ProjectAdminServiceHandler using a PostgreSQL pool.
type ProjectService struct {
	adminv1connect.UnimplementedProjectAdminServiceHandler
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewProjectService creates the projects table if it does not exist and returns a new ProjectService.
func NewProjectService(pool *pgxpool.Pool, logger *slog.Logger) (*ProjectService, error) {
	const ddl = `
CREATE TABLE IF NOT EXISTS projects (
  project_id  TEXT PRIMARY KEY,
  org_id      TEXT NOT NULL,
  name        TEXT NOT NULL,
  slug        TEXT NOT NULL DEFAULT '',
  description TEXT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (org_id, name)
);
ALTER TABLE projects ADD COLUMN IF NOT EXISTS slug TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS projects_slug_idx ON projects (slug) WHERE slug != ''`
	for _, stmt := range splitDDL(ddl) {
		if _, err := pool.Exec(context.Background(), stmt); err != nil {
			return nil, err
		}
	}
	return &ProjectService{pool: pool, logger: logger}, nil
}

// CreateProject inserts a new project and returns it.
func (s *ProjectService) CreateProject(ctx context.Context, req *connect.Request[projectv1.CreateProjectRequest]) (*connect.Response[projectv1.CreateProjectResponse], error) {
	projectID := uuid.Must(uuid.NewV7()).String()
	now := time.Now().UTC()

	const q = `
INSERT INTO projects (project_id, org_id, name, slug, description, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $6)
RETURNING project_id, org_id, name, slug, description, created_at, updated_at`

	var (
		id          string
		orgID       string
		name        string
		slug        string
		description *string
		createdAt   time.Time
		updatedAt   time.Time
	)
	for range maxSlugRetries {
		slug, _ = shortid.Generate()
		err := s.pool.QueryRow(ctx, q, projectID, req.Msg.GetOrgId(), req.Msg.GetName(), slug, req.Msg.Description, now).
			Scan(&id, &orgID, &name, &slug, &description, &createdAt, &updatedAt)
		if err == nil {
			break
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			if pgErr.ConstraintName == "projects_slug_idx" {
				continue
			}
			return nil, connect.NewError(connect.CodeAlreadyExists, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if id == "" {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to generate unique project slug after %d attempts", maxSlugRetries))
	}

	return connect.NewResponse(&projectv1.CreateProjectResponse{
		Project: &projectv1.Project{
			ProjectId:   id,
			OrgId:       orgID,
			Name:        name,
			Slug:        "proj-" + slug,
			Description: description,
			CreatedAt:   timestamppb.New(createdAt),
			UpdatedAt:   timestamppb.New(updatedAt),
		},
	}), nil
}

// GetProject fetches a project by project_id.
func (s *ProjectService) GetProject(ctx context.Context, req *connect.Request[projectv1.GetProjectRequest]) (*connect.Response[projectv1.GetProjectResponse], error) {
	const q = `SELECT project_id, org_id, name, slug, description, created_at, updated_at FROM projects WHERE project_id = $1`

	var (
		id          string
		orgID       string
		name        string
		slug        string
		description *string
		createdAt   time.Time
		updatedAt   time.Time
	)
	err := s.pool.QueryRow(ctx, q, req.Msg.GetProjectId()).
		Scan(&id, &orgID, &name, &slug, &description, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&projectv1.GetProjectResponse{
		Project: &projectv1.Project{
			ProjectId:   id,
			OrgId:       orgID,
			Name:        name,
			Slug:        projSlug(slug),
			Description: description,
			CreatedAt:   timestamppb.New(createdAt),
			UpdatedAt:   timestamppb.New(updatedAt),
		},
	}), nil
}

// ListProjects returns a page of projects for the given org_id, ordered by project_id, with optional cursor-based pagination.
func (s *ProjectService) ListProjects(ctx context.Context, req *connect.Request[projectv1.ListProjectsRequest]) (*connect.Response[projectv1.ListProjectsResponse], error) {
	limit := int(req.Msg.GetLimit())
	if limit <= 0 {
		limit = 100
	}

	var (
		rows pgx.Rows
		err  error
	)

	if token := req.Msg.GetPageToken(); token != "" {
		const q = `SELECT project_id, org_id, name, slug, description, created_at, updated_at FROM projects WHERE org_id = $1 AND project_id > $2 ORDER BY project_id LIMIT $3`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetOrgId(), token, limit)
	} else {
		const q = `SELECT project_id, org_id, name, slug, description, created_at, updated_at FROM projects WHERE org_id = $1 ORDER BY project_id LIMIT $2`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetOrgId(), limit)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer rows.Close()

	var projects []*projectv1.Project
	for rows.Next() {
		var (
			id          string
			orgID       string
			name        string
			slug        string
			description *string
			createdAt   time.Time
			updatedAt   time.Time
		)
		if err := rows.Scan(&id, &orgID, &name, &slug, &description, &createdAt, &updatedAt); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		projects = append(projects, &projectv1.Project{
			ProjectId:   id,
			OrgId:       orgID,
			Name:        name,
			Slug:        projSlug(slug),
			Description: description,
			CreatedAt:   timestamppb.New(createdAt),
			UpdatedAt:   timestamppb.New(updatedAt),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	var nextPageToken string
	if len(projects) == limit {
		nextPageToken = projects[len(projects)-1].ProjectId
	}

	return connect.NewResponse(&projectv1.ListProjectsResponse{
		Projects:      projects,
		NextPageToken: nextPageToken,
	}), nil
}

// UpdateProject updates the description and updated_at of a project.
func (s *ProjectService) UpdateProject(ctx context.Context, req *connect.Request[projectv1.UpdateProjectRequest]) (*connect.Response[projectv1.UpdateProjectResponse], error) {
	now := time.Now().UTC()

	const q = `
UPDATE projects SET description = $2, updated_at = $3
WHERE project_id = $1
RETURNING project_id, org_id, name, slug, description, created_at, updated_at`

	var (
		id          string
		orgID       string
		name        string
		slug        string
		description *string
		createdAt   time.Time
		updatedAt   time.Time
	)
	err := s.pool.QueryRow(ctx, q, req.Msg.GetProjectId(), req.Msg.Description, now).
		Scan(&id, &orgID, &name, &slug, &description, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&projectv1.UpdateProjectResponse{
		Project: &projectv1.Project{
			ProjectId:   id,
			OrgId:       orgID,
			Name:        name,
			Slug:        projSlug(slug),
			Description: description,
			CreatedAt:   timestamppb.New(createdAt),
			UpdatedAt:   timestamppb.New(updatedAt),
		},
	}), nil
}

// DeleteProject removes a project by project_id.
func (s *ProjectService) DeleteProject(ctx context.Context, req *connect.Request[projectv1.DeleteProjectRequest]) (*connect.Response[projectv1.DeleteProjectResponse], error) {
	const q = `DELETE FROM projects WHERE project_id = $1`

	tag, err := s.pool.Exec(ctx, q, req.Msg.GetProjectId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if tag.RowsAffected() == 0 {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("project not found"))
	}

	return connect.NewResponse(&projectv1.DeleteProjectResponse{}), nil
}

func projSlug(raw string) string {
	if raw == "" {
		return ""
	}
	return "proj-" + raw
}
