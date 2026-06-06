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

	orgv1 "github.com/dio/transit/examples/orange/api/orange/org/admin/v1"
	adminv1connect "github.com/dio/transit/examples/orange/api/orange/org/admin/v1/adminv1connect"
)

// OrgService implements adminv1connect.OrgAdminServiceHandler using a PostgreSQL pool.
type OrgService struct {
	adminv1connect.UnimplementedOrgAdminServiceHandler
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewOrgService creates the orgs table if it does not exist and returns a new OrgService.
func NewOrgService(pool *pgxpool.Pool, logger *slog.Logger) (*OrgService, error) {
	const ddl = `CREATE TABLE IF NOT EXISTS orgs (
  org_id      TEXT PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,
  description TEXT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
)`
	if _, err := pool.Exec(context.Background(), ddl); err != nil {
		return nil, err
	}
	return &OrgService{pool: pool, logger: logger}, nil
}

// CreateOrg inserts a new org and returns it.
func (s *OrgService) CreateOrg(ctx context.Context, req *connect.Request[orgv1.CreateOrgRequest]) (*connect.Response[orgv1.CreateOrgResponse], error) {
	orgID := uuid.Must(uuid.NewV7()).String()
	now := time.Now().UTC()

	const q = `
INSERT INTO orgs (org_id, name, description, created_at, updated_at)
VALUES ($1, $2, $3, $4, $4)
RETURNING org_id, name, description, created_at, updated_at`

	var (
		id          string
		name        string
		description *string
		createdAt   time.Time
		updatedAt   time.Time
	)
	err := s.pool.QueryRow(ctx, q, orgID, req.Msg.GetName(), req.Msg.Description, now).
		Scan(&id, &name, &description, &createdAt, &updatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, connect.NewError(connect.CodeAlreadyExists, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&orgv1.CreateOrgResponse{
		Org: &orgv1.Org{
			OrgId:       id,
			Name:        name,
			Description: description,
			CreatedAt:   timestamppb.New(createdAt),
			UpdatedAt:   timestamppb.New(updatedAt),
		},
	}), nil
}

// GetOrg fetches an org by org_id.
func (s *OrgService) GetOrg(ctx context.Context, req *connect.Request[orgv1.GetOrgRequest]) (*connect.Response[orgv1.GetOrgResponse], error) {
	const q = `SELECT org_id, name, description, created_at, updated_at FROM orgs WHERE org_id = $1`

	var (
		id          string
		name        string
		description *string
		createdAt   time.Time
		updatedAt   time.Time
	)
	err := s.pool.QueryRow(ctx, q, req.Msg.GetOrgId()).
		Scan(&id, &name, &description, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&orgv1.GetOrgResponse{
		Org: &orgv1.Org{
			OrgId:       id,
			Name:        name,
			Description: description,
			CreatedAt:   timestamppb.New(createdAt),
			UpdatedAt:   timestamppb.New(updatedAt),
		},
	}), nil
}

// ListOrgs returns a page of orgs ordered by org_id, with optional cursor-based pagination.
func (s *OrgService) ListOrgs(ctx context.Context, req *connect.Request[orgv1.ListOrgsRequest]) (*connect.Response[orgv1.ListOrgsResponse], error) {
	limit := int(req.Msg.GetLimit())
	if limit <= 0 {
		limit = 100
	}

	var (
		rows pgx.Rows
		err  error
	)

	if token := req.Msg.GetPageToken(); token != "" {
		const q = `SELECT org_id, name, description, created_at, updated_at FROM orgs WHERE org_id > $1 ORDER BY org_id LIMIT $2`
		rows, err = s.pool.Query(ctx, q, token, limit)
	} else {
		const q = `SELECT org_id, name, description, created_at, updated_at FROM orgs ORDER BY org_id LIMIT $1`
		rows, err = s.pool.Query(ctx, q, limit)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer rows.Close()

	var orgs []*orgv1.Org
	for rows.Next() {
		var (
			id          string
			name        string
			description *string
			createdAt   time.Time
			updatedAt   time.Time
		)
		if err := rows.Scan(&id, &name, &description, &createdAt, &updatedAt); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		orgs = append(orgs, &orgv1.Org{
			OrgId:       id,
			Name:        name,
			Description: description,
			CreatedAt:   timestamppb.New(createdAt),
			UpdatedAt:   timestamppb.New(updatedAt),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	var nextPageToken string
	if len(orgs) == limit {
		nextPageToken = orgs[len(orgs)-1].OrgId
	}

	return connect.NewResponse(&orgv1.ListOrgsResponse{
		Orgs:          orgs,
		NextPageToken: nextPageToken,
	}), nil
}

// UpdateOrg updates the description and updated_at of an org.
func (s *OrgService) UpdateOrg(ctx context.Context, req *connect.Request[orgv1.UpdateOrgRequest]) (*connect.Response[orgv1.UpdateOrgResponse], error) {
	now := time.Now().UTC()

	const q = `
UPDATE orgs SET description = $2, updated_at = $3
WHERE org_id = $1
RETURNING org_id, name, description, created_at, updated_at`

	var (
		id          string
		name        string
		description *string
		createdAt   time.Time
		updatedAt   time.Time
	)
	err := s.pool.QueryRow(ctx, q, req.Msg.GetOrgId(), req.Msg.Description, now).
		Scan(&id, &name, &description, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&orgv1.UpdateOrgResponse{
		Org: &orgv1.Org{
			OrgId:       id,
			Name:        name,
			Description: description,
			CreatedAt:   timestamppb.New(createdAt),
			UpdatedAt:   timestamppb.New(updatedAt),
		},
	}), nil
}

// DeleteOrg removes an org by org_id.
func (s *OrgService) DeleteOrg(ctx context.Context, req *connect.Request[orgv1.DeleteOrgRequest]) (*connect.Response[orgv1.DeleteOrgResponse], error) {
	const q = `DELETE FROM orgs WHERE org_id = $1`

	tag, err := s.pool.Exec(ctx, q, req.Msg.GetOrgId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if tag.RowsAffected() == 0 {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("org not found"))
	}

	return connect.NewResponse(&orgv1.DeleteOrgResponse{}), nil
}
