package resources

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	profilev1 "github.com/dio/transit/examples/orange/api/orange/profile/admin/v1"
	adminv1connect "github.com/dio/transit/examples/orange/api/orange/profile/admin/v1/adminv1connect"
)

// ProfileService implements adminv1connect.ProfileAdminServiceHandler using PostgreSQL.
//
// The profiles table stores tool_filter_shape and auth_shape as JSONB so the
// config compile pipeline can re-intern shapes into the live snapshot's Pools
// without a DB round-trip (§18.2 of the config system design doc).
type ProfileService struct {
	adminv1connect.UnimplementedProfileAdminServiceHandler
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewProfileService creates the profiles table + indexes and returns a ProfileService.
func NewProfileService(pool *pgxpool.Pool, logger *slog.Logger) (*ProfileService, error) {
	ddl := `
CREATE TABLE IF NOT EXISTS profiles (
  profile_id        TEXT PRIMARY KEY,
  workspace_id      TEXT NOT NULL,
  user_id           TEXT NOT NULL,
  name              TEXT NOT NULL,
  description       TEXT,
  tool_filter_shape JSONB,
  auth_shape        JSONB,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at        TIMESTAMPTZ,
  UNIQUE (workspace_id, user_id, name)
);
CREATE INDEX IF NOT EXISTS profiles_workspace_idx     ON profiles (workspace_id)          WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS profiles_workspace_usr_idx ON profiles (workspace_id, user_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS profiles_updated_at_idx    ON profiles (updated_at DESC)       WHERE deleted_at IS NULL;`

	if _, err := pool.Exec(context.Background(), ddl); err != nil {
		return nil, err
	}
	return &ProfileService{pool: pool, logger: logger}, nil
}

func (s *ProfileService) CreateProfile(ctx context.Context, req *connect.Request[profilev1.CreateProfileRequest]) (*connect.Response[profilev1.CreateProfileResponse], error) {
	profileID := uuid.Must(uuid.NewV7()).String()
	now := time.Now().UTC()

	toolJSON, err := marshalProtoMessages(protoMessages(req.Msg.GetTools()))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	authJSON, err := marshalProtoMessages(protoMessages(req.Msg.GetAuthOverrides()))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	const q = `
INSERT INTO profiles (profile_id, workspace_id, user_id, name, description, tool_filter_shape, auth_shape, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)
RETURNING profile_id, workspace_id, user_id, name, description, tool_filter_shape, auth_shape, created_at, updated_at`

	p, err := scanProfile(s.pool.QueryRow(ctx, q,
		profileID, req.Msg.GetWorkspaceId(), req.Msg.GetUserId(), req.Msg.GetName(),
		req.Msg.Description, toolJSON, authJSON, now,
	))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, connect.NewError(connect.CodeAlreadyExists, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&profilev1.CreateProfileResponse{Profile: p}), nil
}

func (s *ProfileService) GetProfile(ctx context.Context, req *connect.Request[profilev1.GetProfileRequest]) (*connect.Response[profilev1.GetProfileResponse], error) {
	const q = `SELECT profile_id, workspace_id, user_id, name, description, tool_filter_shape, auth_shape, created_at, updated_at
FROM profiles WHERE profile_id = $1 AND deleted_at IS NULL`

	p, err := scanProfile(s.pool.QueryRow(ctx, q, req.Msg.GetProfileId()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&profilev1.GetProfileResponse{Profile: p}), nil
}

func (s *ProfileService) ListProfiles(ctx context.Context, req *connect.Request[profilev1.ListProfilesRequest]) (*connect.Response[profilev1.ListProfilesResponse], error) {
	limit := int(req.Msg.GetLimit())
	if limit <= 0 {
		limit = 100
	}

	var (
		rows pgx.Rows
		err  error
	)

	pageToken := req.Msg.GetPageToken()
	userID := req.Msg.UserId

	switch {
	case pageToken != "" && userID != nil:
		const q = `SELECT profile_id, workspace_id, user_id, name, description, tool_filter_shape, auth_shape, created_at, updated_at
FROM profiles WHERE workspace_id = $1 AND user_id = $2 AND profile_id > $3 AND deleted_at IS NULL ORDER BY profile_id LIMIT $4`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetWorkspaceId(), *userID, pageToken, limit)
	case pageToken != "":
		const q = `SELECT profile_id, workspace_id, user_id, name, description, tool_filter_shape, auth_shape, created_at, updated_at
FROM profiles WHERE workspace_id = $1 AND profile_id > $2 AND deleted_at IS NULL ORDER BY profile_id LIMIT $3`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetWorkspaceId(), pageToken, limit)
	case userID != nil:
		const q = `SELECT profile_id, workspace_id, user_id, name, description, tool_filter_shape, auth_shape, created_at, updated_at
FROM profiles WHERE workspace_id = $1 AND user_id = $2 AND deleted_at IS NULL ORDER BY profile_id LIMIT $3`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetWorkspaceId(), *userID, limit)
	default:
		const q = `SELECT profile_id, workspace_id, user_id, name, description, tool_filter_shape, auth_shape, created_at, updated_at
FROM profiles WHERE workspace_id = $1 AND deleted_at IS NULL ORDER BY profile_id LIMIT $2`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetWorkspaceId(), limit)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer rows.Close()

	var profiles []*profilev1.Profile
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		profiles = append(profiles, p)
	}
	if err := rows.Err(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	var nextPageToken string
	if len(profiles) == limit {
		nextPageToken = profiles[len(profiles)-1].ProfileId
	}
	return connect.NewResponse(&profilev1.ListProfilesResponse{
		Profiles:      profiles,
		NextPageToken: nextPageToken,
	}), nil
}

func (s *ProfileService) UpdateProfile(ctx context.Context, req *connect.Request[profilev1.UpdateProfileRequest]) (*connect.Response[profilev1.UpdateProfileResponse], error) {
	now := time.Now().UTC()

	toolJSON, err := marshalProtoMessages(protoMessages(req.Msg.GetTools()))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	authJSON, err := marshalProtoMessages(protoMessages(req.Msg.GetAuthOverrides()))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	const q = `
UPDATE profiles SET description = $2, tool_filter_shape = $3, auth_shape = $4, updated_at = $5
WHERE profile_id = $1 AND deleted_at IS NULL
RETURNING profile_id, workspace_id, user_id, name, description, tool_filter_shape, auth_shape, created_at, updated_at`

	p, err := scanProfile(s.pool.QueryRow(ctx, q, req.Msg.GetProfileId(), req.Msg.Description, toolJSON, authJSON, now))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&profilev1.UpdateProfileResponse{Profile: p}), nil
}

func (s *ProfileService) DeleteProfile(ctx context.Context, req *connect.Request[profilev1.DeleteProfileRequest]) (*connect.Response[profilev1.DeleteProfileResponse], error) {
	const q = `UPDATE profiles SET deleted_at = now() WHERE profile_id = $1 AND deleted_at IS NULL`

	tag, err := s.pool.Exec(ctx, q, req.Msg.GetProfileId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if tag.RowsAffected() == 0 {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("profile not found"))
	}
	return connect.NewResponse(&profilev1.DeleteProfileResponse{}), nil
}

// ── scan + shape helpers ──────────────────────────────────────────────────────

func scanProfile(row scanner) (*profilev1.Profile, error) {
	var (
		profileID   string
		workspaceID string
		userID      string
		name        string
		description *string
		toolShape   []byte
		authShape   []byte
		createdAt   time.Time
		updatedAt   time.Time
	)
	if err := row.Scan(&profileID, &workspaceID, &userID, &name, &description,
		&toolShape, &authShape, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	tools, err := unmarshalProtoJSONArray(toolShape, func() *profilev1.ToolFilter { return &profilev1.ToolFilter{} })
	if err != nil {
		return nil, err
	}
	auths, err := unmarshalProtoJSONArray(authShape, func() *profilev1.AuthOverride { return &profilev1.AuthOverride{} })
	if err != nil {
		return nil, err
	}
	return &profilev1.Profile{
		ProfileId:     profileID,
		WorkspaceId:   workspaceID,
		UserId:        userID,
		Name:          name,
		Description:   description,
		Tools:         tools,
		AuthOverrides: auths,
		CreatedAt:     timestamppb.New(createdAt),
		UpdatedAt:     timestamppb.New(updatedAt),
	}, nil
}

// protoMessages converts a typed slice to []proto.Message for marshalProtoMessages.
func protoMessages[T proto.Message](msgs []T) []proto.Message {
	out := make([]proto.Message, len(msgs))
	for i, m := range msgs {
		out[i] = m
	}
	return out
}

// marshalProtoMessages marshals a slice of proto.Message to a JSON array for JSONB storage.
// Returns nil when the slice is empty.
func marshalProtoMessages(msgs []proto.Message) ([]byte, error) {
	if len(msgs) == 0 {
		return nil, nil
	}
	arr := make([]json.RawMessage, len(msgs))
	for i, m := range msgs {
		b, err := protojson.Marshal(m)
		if err != nil {
			return nil, err
		}
		arr[i] = b
	}
	return json.Marshal(arr)
}

// unmarshalProtoJSONArray parses a JSON array stored in JSONB back into proto messages.
func unmarshalProtoJSONArray[T proto.Message](data []byte, newFn func() T) ([]T, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(data, &arr); err != nil {
		return nil, err
	}
	result := make([]T, len(arr))
	for i, raw := range arr {
		m := newFn()
		if err := protojson.Unmarshal(raw, m); err != nil {
			return nil, err
		}
		result[i] = m
	}
	return result, nil
}
