package resources

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	userv1 "github.com/dio/transit/examples/orange/api/orange/user/admin/v1"
	adminv1connect "github.com/dio/transit/examples/orange/api/orange/user/admin/v1/adminv1connect"
)

const userDDL = `
CREATE TABLE IF NOT EXISTS users (
  user_id     TEXT PRIMARY KEY,
  org_id      TEXT NOT NULL,
  email       TEXT NOT NULL,
  description TEXT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (org_id, email)
);
CREATE TABLE IF NOT EXISTS workspace_members (
  workspace_id TEXT NOT NULL,
  user_id      TEXT NOT NULL,
  joined_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, user_id)
)`

// KeyBinder is implemented by apikeys.Store. It is declared here to avoid an
// import cycle between resources ↔ apikeys.
type KeyBinder interface {
	BindWorkspace(ctx context.Context, orgID, userID, wsID string) error
	UnbindWorkspace(ctx context.Context, orgID, userID, wsID string) error
}

// UserService implements adminv1connect.UserAdminServiceHandler over PostgreSQL.
type UserService struct {
	adminv1connect.UnimplementedUserAdminServiceHandler

	pool      *pgxpool.Pool
	logger    *slog.Logger
	keyBinder KeyBinder // may be nil in tests; nil → skip key binding
}

// NewUserService creates a UserService. kb is called on workspace membership
// changes to update API key scopes atomically; pass nil to skip binding (tests).
func NewUserService(pool *pgxpool.Pool, logger *slog.Logger, kb KeyBinder) *UserService {
	return &UserService{pool: pool, logger: logger, keyBinder: kb}
}

// EnsureSchema creates the users and workspace_members tables if they do not exist.
func (s *UserService) EnsureSchema(ctx context.Context) error {
	for _, stmt := range strings.Split(userDDL, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// CreateUser inserts a new user and returns the created record.
func (s *UserService) CreateUser(
	ctx context.Context,
	req *connect.Request[userv1.CreateUserRequest],
) (*connect.Response[userv1.CreateUserResponse], error) {
	userID := uuid.Must(uuid.NewV7()).String()

	const query = `
INSERT INTO users (user_id, org_id, email, description)
VALUES ($1, $2, $3, $4)
RETURNING user_id, org_id, email, description, created_at, updated_at`

	var (
		uID, orgID, email string
		desc              *string
		createdAt         time.Time
		updatedAt         time.Time
	)
	err := s.pool.QueryRow(ctx, query,
		userID,
		req.Msg.GetOrgId(),
		req.Msg.GetEmail(),
		req.Msg.Description,
	).Scan(&uID, &orgID, &email, &desc, &createdAt, &updatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, connect.NewError(connect.CodeAlreadyExists, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&userv1.CreateUserResponse{
		User: &userv1.User{
			UserId:      uID,
			OrgId:       orgID,
			Email:       email,
			Description: desc,
			CreatedAt:   timestamppb.New(createdAt),
			UpdatedAt:   timestamppb.New(updatedAt),
		},
	}), nil
}

// GetUser retrieves a user by user_id.
func (s *UserService) GetUser(
	ctx context.Context,
	req *connect.Request[userv1.GetUserRequest],
) (*connect.Response[userv1.GetUserResponse], error) {
	const query = `
SELECT user_id, org_id, email, description, created_at, updated_at
FROM users
WHERE user_id = $1`

	var (
		uID, orgID, email string
		desc              *string
		createdAt         time.Time
		updatedAt         time.Time
	)
	err := s.pool.QueryRow(ctx, query, req.Msg.GetUserId()).
		Scan(&uID, &orgID, &email, &desc, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&userv1.GetUserResponse{
		User: &userv1.User{
			UserId:      uID,
			OrgId:       orgID,
			Email:       email,
			Description: desc,
			CreatedAt:   timestamppb.New(createdAt),
			UpdatedAt:   timestamppb.New(updatedAt),
		},
	}), nil
}

// ListUsers returns users belonging to an org, with keyset pagination.
func (s *UserService) ListUsers(
	ctx context.Context,
	req *connect.Request[userv1.ListUsersRequest],
) (*connect.Response[userv1.ListUsersResponse], error) {
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
SELECT user_id, org_id, email, description, created_at, updated_at
FROM users
WHERE org_id = $1 AND user_id > $2
ORDER BY user_id ASC
LIMIT $3`
		rows, err = s.pool.Query(ctx, query, req.Msg.GetOrgId(), pageToken, limit)
	} else {
		const query = `
SELECT user_id, org_id, email, description, created_at, updated_at
FROM users
WHERE org_id = $1
ORDER BY user_id ASC
LIMIT $2`
		rows, err = s.pool.Query(ctx, query, req.Msg.GetOrgId(), limit)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer rows.Close()

	var users []*userv1.User
	for rows.Next() {
		var (
			uID, orgID, email string
			desc              *string
			createdAt         time.Time
			updatedAt         time.Time
		)
		if err := rows.Scan(&uID, &orgID, &email, &desc, &createdAt, &updatedAt); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		users = append(users, &userv1.User{
			UserId:      uID,
			OrgId:       orgID,
			Email:       email,
			Description: desc,
			CreatedAt:   timestamppb.New(createdAt),
			UpdatedAt:   timestamppb.New(updatedAt),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	var nextPageToken string
	if len(users) == limit {
		nextPageToken = users[len(users)-1].UserId
	}

	return connect.NewResponse(&userv1.ListUsersResponse{
		Users:         users,
		NextPageToken: nextPageToken,
	}), nil
}

// UpdateUser updates the description and updated_at of an existing user.
func (s *UserService) UpdateUser(
	ctx context.Context,
	req *connect.Request[userv1.UpdateUserRequest],
) (*connect.Response[userv1.UpdateUserResponse], error) {
	const query = `
UPDATE users
SET description = $1, updated_at = now()
WHERE user_id = $2
RETURNING user_id, org_id, email, description, created_at, updated_at`

	var (
		uID, orgID, email string
		desc              *string
		createdAt         time.Time
		updatedAt         time.Time
	)
	err := s.pool.QueryRow(ctx, query,
		req.Msg.Description,
		req.Msg.GetUserId(),
	).Scan(&uID, &orgID, &email, &desc, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&userv1.UpdateUserResponse{
		User: &userv1.User{
			UserId:      uID,
			OrgId:       orgID,
			Email:       email,
			Description: desc,
			CreatedAt:   timestamppb.New(createdAt),
			UpdatedAt:   timestamppb.New(updatedAt),
		},
	}), nil
}

// DeleteUser removes a user by user_id.
func (s *UserService) DeleteUser(
	ctx context.Context,
	req *connect.Request[userv1.DeleteUserRequest],
) (*connect.Response[userv1.DeleteUserResponse], error) {
	result, err := s.pool.Exec(ctx,
		`DELETE FROM users WHERE user_id = $1`,
		req.Msg.GetUserId(),
	)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if result.RowsAffected() == 0 {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("user not found"))
	}

	return connect.NewResponse(&userv1.DeleteUserResponse{}), nil
}

// AddWorkspaceMember inserts a workspace_members record and, if a KeyBinder is
// configured, atomically supersedes every active API key for the user with
// updated workspace-scoped permissions.
func (s *UserService) AddWorkspaceMember(
	ctx context.Context,
	req *connect.Request[userv1.AddWorkspaceMemberRequest],
) (*connect.Response[userv1.AddWorkspaceMemberResponse], error) {
	wsID := req.Msg.GetWorkspaceId()
	uID := req.Msg.GetUserId()

	const query = `
INSERT INTO workspace_members (workspace_id, user_id)
VALUES ($1, $2)
RETURNING workspace_id, user_id, joined_at`

	var (
		wID      string
		joinedAt time.Time
	)
	err := s.pool.QueryRow(ctx, query, wsID, uID).Scan(&wID, &uID, &joinedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, connect.NewError(connect.CodeAlreadyExists, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if s.keyBinder != nil {
		// Resolve org_id for the user (required by BindWorkspace).
		var orgID string
		if err := s.pool.QueryRow(ctx,
			`SELECT org_id FROM users WHERE user_id = $1`, uID,
		).Scan(&orgID); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		if err := s.keyBinder.BindWorkspace(ctx, orgID, uID, wsID); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}

	return connect.NewResponse(&userv1.AddWorkspaceMemberResponse{
		Member: &userv1.WorkspaceMember{
			WorkspaceId: wID,
			UserId:      uID,
			JoinedAt:    timestamppb.New(joinedAt),
		},
	}), nil
}

// RemoveWorkspaceMember deletes a workspace_members record and, if a KeyBinder
// is configured, atomically strips workspace-scoped permissions from every
// active API key for the user.
func (s *UserService) RemoveWorkspaceMember(
	ctx context.Context,
	req *connect.Request[userv1.RemoveWorkspaceMemberRequest],
) (*connect.Response[userv1.RemoveWorkspaceMemberResponse], error) {
	wsID := req.Msg.GetWorkspaceId()
	uID := req.Msg.GetUserId()

	result, err := s.pool.Exec(ctx,
		`DELETE FROM workspace_members WHERE workspace_id = $1 AND user_id = $2`,
		wsID, uID,
	)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if result.RowsAffected() == 0 {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("workspace member not found"))
	}

	if s.keyBinder != nil {
		var orgID string
		if err := s.pool.QueryRow(ctx,
			`SELECT org_id FROM users WHERE user_id = $1`, uID,
		).Scan(&orgID); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		if err := s.keyBinder.UnbindWorkspace(ctx, orgID, uID, wsID); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}

	return connect.NewResponse(&userv1.RemoveWorkspaceMemberResponse{}), nil
}

// ListWorkspaceMembers returns members of a workspace, with keyset pagination on user_id.
func (s *UserService) ListWorkspaceMembers(
	ctx context.Context,
	req *connect.Request[userv1.ListWorkspaceMembersRequest],
) (*connect.Response[userv1.ListWorkspaceMembersResponse], error) {
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
SELECT workspace_id, user_id, joined_at
FROM workspace_members
WHERE workspace_id = $1 AND user_id > $2
ORDER BY user_id ASC
LIMIT $3`
		rows, err = s.pool.Query(ctx, query, req.Msg.GetWorkspaceId(), pageToken, limit)
	} else {
		const query = `
SELECT workspace_id, user_id, joined_at
FROM workspace_members
WHERE workspace_id = $1
ORDER BY user_id ASC
LIMIT $2`
		rows, err = s.pool.Query(ctx, query, req.Msg.GetWorkspaceId(), limit)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer rows.Close()

	var members []*userv1.WorkspaceMember
	for rows.Next() {
		var (
			wID, uID string
			joinedAt time.Time
		)
		if err := rows.Scan(&wID, &uID, &joinedAt); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		members = append(members, &userv1.WorkspaceMember{
			WorkspaceId: wID,
			UserId:      uID,
			JoinedAt:    timestamppb.New(joinedAt),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	var nextPageToken string
	if len(members) == limit {
		nextPageToken = members[len(members)-1].UserId
	}

	return connect.NewResponse(&userv1.ListWorkspaceMembersResponse{
		Members:       members,
		NextPageToken: nextPageToken,
	}), nil
}
