package resources

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"google.golang.org/protobuf/types/known/timestamppb"

	keyv1 "github.com/dio/transit/examples/orange/api/orange/key/admin/v1"
	adminv1connect "github.com/dio/transit/examples/orange/api/orange/key/admin/v1/adminv1connect"
	routingv1 "github.com/dio/transit/examples/orange/api/orange/routing/v1"
)

// KeyService implements adminv1connect.KeyAdminServiceHandler using a PostgreSQL pool.
type KeyService struct {
	adminv1connect.UnimplementedKeyAdminServiceHandler
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewKeyService creates the keys, paseto_tokens, and key_secrets tables if they do not exist
// and returns a new KeyService.
func NewKeyService(pool *pgxpool.Pool, logger *slog.Logger) (*KeyService, error) {
	ddl := `
CREATE TABLE IF NOT EXISTS keys (
  key_id         TEXT PRIMARY KEY,
  workspace_id   TEXT NOT NULL,
  user_id        TEXT NOT NULL,
  name           TEXT NOT NULL,
  key_format     TEXT NOT NULL DEFAULT 'paseto_v4.public',
  description    TEXT,
  routing_shape  JSONB,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at     TIMESTAMPTZ,
  UNIQUE (workspace_id, user_id, name)
);
CREATE INDEX IF NOT EXISTS keys_workspace_idx     ON keys (workspace_id)             WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS keys_workspace_usr_idx ON keys (workspace_id, user_id)    WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS keys_updated_at_idx    ON keys (updated_at DESC)          WHERE deleted_at IS NULL;
CREATE TABLE IF NOT EXISTS paseto_tokens (
  token_id    TEXT PRIMARY KEY,
  key_id      TEXT NOT NULL,
  jti         TEXT NOT NULL UNIQUE,
  iat         TIMESTAMPTZ NOT NULL,
  exp         TIMESTAMPTZ NOT NULL,
  pol         TEXT,
  token_hash  TEXT NOT NULL,
  revoked     BOOLEAN NOT NULL DEFAULT false,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS key_secrets (
  key_secret_id   TEXT PRIMARY KEY,
  key_id          TEXT NOT NULL,
  upstream_target TEXT NOT NULL,
  version         INT NOT NULL DEFAULT 1,
  value_encrypted TEXT NOT NULL,
  active          BOOLEAN NOT NULL DEFAULT true,
  description     TEXT,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (key_id, upstream_target)
)`
	if _, err := pool.Exec(context.Background(), ddl); err != nil {
		return nil, err
	}
	return &KeyService{pool: pool, logger: logger}, nil
}

// ── Key CRUD ──────────────────────────────────────────────────────────────────

// CreateKey inserts a new key and returns it.
func (s *KeyService) CreateKey(ctx context.Context, req *connect.Request[keyv1.CreateKeyRequest]) (*connect.Response[keyv1.CreateKeyResponse], error) {
	keyID := uuid.Must(uuid.NewV7()).String()
	now := time.Now().UTC()

	routingJSON, err := marshalRoutingOverrides(req.Msg.GetRoutingOverrides())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	const q = `
INSERT INTO keys (key_id, workspace_id, user_id, name, key_format, description, routing_shape, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'paseto_v4.public', $5, $6, $7, $7)
RETURNING key_id, workspace_id, user_id, name, key_format, description, routing_shape, created_at, updated_at`

	key, err := scanKey(s.pool.QueryRow(ctx, q,
		keyID, req.Msg.GetWorkspaceId(), req.Msg.GetUserId(), req.Msg.GetName(),
		req.Msg.Description, routingJSON, now,
	))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, connect.NewError(connect.CodeAlreadyExists, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&keyv1.CreateKeyResponse{Key: key}), nil
}

// GetKey fetches an active key by key_id.
func (s *KeyService) GetKey(ctx context.Context, req *connect.Request[keyv1.GetKeyRequest]) (*connect.Response[keyv1.GetKeyResponse], error) {
	const q = `SELECT key_id, workspace_id, user_id, name, key_format, description, routing_shape, created_at, updated_at
FROM keys WHERE key_id = $1 AND deleted_at IS NULL`

	key, err := scanKey(s.pool.QueryRow(ctx, q, req.Msg.GetKeyId()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&keyv1.GetKeyResponse{Key: key}), nil
}

// ListKeys returns a page of keys for a workspace, optionally filtered by user_id.
func (s *KeyService) ListKeys(ctx context.Context, req *connect.Request[keyv1.ListKeysRequest]) (*connect.Response[keyv1.ListKeysResponse], error) {
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
		const q = `SELECT key_id, workspace_id, user_id, name, key_format, description, routing_shape, created_at, updated_at
FROM keys WHERE workspace_id = $1 AND user_id = $2 AND key_id > $3 AND deleted_at IS NULL ORDER BY key_id LIMIT $4`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetWorkspaceId(), *userID, pageToken, limit)
	case pageToken != "":
		const q = `SELECT key_id, workspace_id, user_id, name, key_format, description, routing_shape, created_at, updated_at
FROM keys WHERE workspace_id = $1 AND key_id > $2 AND deleted_at IS NULL ORDER BY key_id LIMIT $3`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetWorkspaceId(), pageToken, limit)
	case userID != nil:
		const q = `SELECT key_id, workspace_id, user_id, name, key_format, description, routing_shape, created_at, updated_at
FROM keys WHERE workspace_id = $1 AND user_id = $2 AND deleted_at IS NULL ORDER BY key_id LIMIT $3`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetWorkspaceId(), *userID, limit)
	default:
		const q = `SELECT key_id, workspace_id, user_id, name, key_format, description, routing_shape, created_at, updated_at
FROM keys WHERE workspace_id = $1 AND deleted_at IS NULL ORDER BY key_id LIMIT $2`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetWorkspaceId(), limit)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer rows.Close()

	var keys []*keyv1.Key
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	var nextPageToken string
	if len(keys) == limit {
		nextPageToken = keys[len(keys)-1].KeyId
	}

	return connect.NewResponse(&keyv1.ListKeysResponse{
		Keys:          keys,
		NextPageToken: nextPageToken,
	}), nil
}

// UpdateKey updates the description and/or routing overrides of a key.
func (s *KeyService) UpdateKey(ctx context.Context, req *connect.Request[keyv1.UpdateKeyRequest]) (*connect.Response[keyv1.UpdateKeyResponse], error) {
	now := time.Now().UTC()

	routingJSON, err := marshalRoutingOverrides(req.Msg.GetRoutingOverrides())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	const q = `
UPDATE keys SET description = $2, routing_shape = $3, updated_at = $4
WHERE key_id = $1 AND deleted_at IS NULL
RETURNING key_id, workspace_id, user_id, name, key_format, description, routing_shape, created_at, updated_at`

	key, err := scanKey(s.pool.QueryRow(ctx, q, req.Msg.GetKeyId(), req.Msg.Description, routingJSON, now))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&keyv1.UpdateKeyResponse{Key: key}), nil
}

// DeleteKey soft-deletes a key by setting deleted_at.
func (s *KeyService) DeleteKey(ctx context.Context, req *connect.Request[keyv1.DeleteKeyRequest]) (*connect.Response[keyv1.DeleteKeyResponse], error) {
	const q = `UPDATE keys SET deleted_at = now() WHERE key_id = $1 AND deleted_at IS NULL`

	tag, err := s.pool.Exec(ctx, q, req.Msg.GetKeyId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if tag.RowsAffected() == 0 {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("key not found"))
	}
	return connect.NewResponse(&keyv1.DeleteKeyResponse{}), nil
}

// ── PASETOToken management ────────────────────────────────────────────────────

// IssueToken generates a placeholder PASETO token record and stores its metadata.
func (s *KeyService) IssueToken(ctx context.Context, req *connect.Request[keyv1.IssueTokenRequest]) (*connect.Response[keyv1.IssueTokenResponse], error) {
	tokenID := uuid.Must(uuid.NewV7()).String()
	jti := uuid.Must(uuid.NewV7()).String()
	now := time.Now().UTC()
	exp := now.Add(time.Duration(req.Msg.GetTtlSeconds()) * time.Second)

	// Placeholder token hash: sha256 of the tokenID bytes encoded as hex.
	h := sha256.Sum256([]byte(tokenID))
	tokenHash := hex.EncodeToString(h[:])

	const q = `
INSERT INTO paseto_tokens (token_id, key_id, jti, iat, exp, pol, token_hash, revoked, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, false, $4)
RETURNING token_id, key_id, jti, iat, exp, pol, token_hash, revoked, created_at`

	tok, err := scanPASETOToken(s.pool.QueryRow(ctx, q,
		tokenID, req.Msg.GetKeyId(), jti, now, exp, req.Msg.Pol, tokenHash,
	))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, connect.NewError(connect.CodeAlreadyExists, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&keyv1.IssueTokenResponse{
		// Placeholder token string; actual PASETO signing is out of scope.
		Token:    tokenID,
		Metadata: tok,
	}), nil
}

// GetToken returns metadata for a previously issued token.
func (s *KeyService) GetToken(ctx context.Context, req *connect.Request[keyv1.GetTokenRequest]) (*connect.Response[keyv1.GetTokenResponse], error) {
	const q = `SELECT token_id, key_id, jti, iat, exp, pol, token_hash, revoked, created_at FROM paseto_tokens WHERE token_id = $1`

	tok, err := scanPASETOToken(s.pool.QueryRow(ctx, q, req.Msg.GetTokenId()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&keyv1.GetTokenResponse{Token: tok}), nil
}

// ListTokens lists token metadata records for a key.
func (s *KeyService) ListTokens(ctx context.Context, req *connect.Request[keyv1.ListTokensRequest]) (*connect.Response[keyv1.ListTokensResponse], error) {
	limit := int(req.Msg.GetLimit())
	if limit <= 0 {
		limit = 100
	}

	includeRevoked := req.Msg.GetIncludeRevoked()
	pageToken := req.Msg.GetPageToken()

	var (
		rows pgx.Rows
		err  error
	)

	switch {
	case pageToken != "" && !includeRevoked:
		const q = `SELECT token_id, key_id, jti, iat, exp, pol, token_hash, revoked, created_at
FROM paseto_tokens WHERE key_id = $1 AND NOT revoked AND token_id > $2 ORDER BY token_id LIMIT $3`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetKeyId(), pageToken, limit)
	case pageToken != "":
		const q = `SELECT token_id, key_id, jti, iat, exp, pol, token_hash, revoked, created_at
FROM paseto_tokens WHERE key_id = $1 AND token_id > $2 ORDER BY token_id LIMIT $3`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetKeyId(), pageToken, limit)
	case !includeRevoked:
		const q = `SELECT token_id, key_id, jti, iat, exp, pol, token_hash, revoked, created_at
FROM paseto_tokens WHERE key_id = $1 AND NOT revoked ORDER BY token_id LIMIT $2`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetKeyId(), limit)
	default:
		const q = `SELECT token_id, key_id, jti, iat, exp, pol, token_hash, revoked, created_at
FROM paseto_tokens WHERE key_id = $1 ORDER BY token_id LIMIT $2`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetKeyId(), limit)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer rows.Close()

	var tokens []*keyv1.PASETOToken
	for rows.Next() {
		tok, err := scanPASETOTokenRow(rows)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		tokens = append(tokens, tok)
	}
	if err := rows.Err(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	var nextPageToken string
	if len(tokens) == limit {
		nextPageToken = tokens[len(tokens)-1].TokenId
	}

	return connect.NewResponse(&keyv1.ListTokensResponse{
		Tokens:        tokens,
		NextPageToken: nextPageToken,
	}), nil
}

// RevokeToken marks a token as revoked.
func (s *KeyService) RevokeToken(ctx context.Context, req *connect.Request[keyv1.RevokeTokenRequest]) (*connect.Response[keyv1.RevokeTokenResponse], error) {
	const q = `UPDATE paseto_tokens SET revoked = true WHERE token_id = $1`

	tag, err := s.pool.Exec(ctx, q, req.Msg.GetTokenId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if tag.RowsAffected() == 0 {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("token not found"))
	}

	return connect.NewResponse(&keyv1.RevokeTokenResponse{}), nil
}

// ── KeySecret (BYOK) management ──────────────────────────────────────────────

// CreateKeySecret creates a versioned upstream credential bound to a key.
func (s *KeyService) CreateKeySecret(ctx context.Context, req *connect.Request[keyv1.CreateKeySecretRequest]) (*connect.Response[keyv1.CreateKeySecretResponse], error) {
	keySecretID := uuid.Must(uuid.NewV7()).String()
	now := time.Now().UTC()

	const q = `
INSERT INTO key_secrets (key_secret_id, key_id, upstream_target, version, value_encrypted, active, description, created_at, updated_at)
VALUES ($1, $2, $3, 1, $4, true, $5, $6, $6)
RETURNING key_secret_id, key_id, upstream_target, version, active, description, created_at, updated_at`

	sec, err := scanKeySecret(s.pool.QueryRow(ctx, q,
		keySecretID, req.Msg.GetKeyId(), req.Msg.GetUpstreamTarget(), req.Msg.GetValue(), req.Msg.Description, now,
	))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, connect.NewError(connect.CodeAlreadyExists, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&keyv1.CreateKeySecretResponse{Secret: sec}), nil
}

// GetKeySecret returns a KeySecret record (value is never returned).
func (s *KeyService) GetKeySecret(ctx context.Context, req *connect.Request[keyv1.GetKeySecretRequest]) (*connect.Response[keyv1.GetKeySecretResponse], error) {
	const q = `SELECT key_secret_id, key_id, upstream_target, version, active, description, created_at, updated_at FROM key_secrets WHERE key_secret_id = $1`

	sec, err := scanKeySecret(s.pool.QueryRow(ctx, q, req.Msg.GetKeySecretId()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&keyv1.GetKeySecretResponse{Secret: sec}), nil
}

// ListKeySecrets lists all KeySecrets for a key.
func (s *KeyService) ListKeySecrets(ctx context.Context, req *connect.Request[keyv1.ListKeySecretsRequest]) (*connect.Response[keyv1.ListKeySecretsResponse], error) {
	limit := int(req.Msg.GetLimit())
	if limit <= 0 {
		limit = 100
	}

	var (
		rows pgx.Rows
		err  error
	)

	if pageToken := req.Msg.GetPageToken(); pageToken != "" {
		const q = `SELECT key_secret_id, key_id, upstream_target, version, active, description, created_at, updated_at
FROM key_secrets WHERE key_id = $1 AND key_secret_id > $2 ORDER BY key_secret_id LIMIT $3`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetKeyId(), pageToken, limit)
	} else {
		const q = `SELECT key_secret_id, key_id, upstream_target, version, active, description, created_at, updated_at
FROM key_secrets WHERE key_id = $1 ORDER BY key_secret_id LIMIT $2`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetKeyId(), limit)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer rows.Close()

	var secrets []*keyv1.KeySecret
	for rows.Next() {
		sec, err := scanKeySecretRow(rows)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		secrets = append(secrets, sec)
	}
	if err := rows.Err(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	var nextPageToken string
	if len(secrets) == limit {
		nextPageToken = secrets[len(secrets)-1].KeySecretId
	}

	return connect.NewResponse(&keyv1.ListKeySecretsResponse{
		Secrets:       secrets,
		NextPageToken: nextPageToken,
	}), nil
}

// RotateKeySecret creates a new version of a KeySecret.
func (s *KeyService) RotateKeySecret(ctx context.Context, req *connect.Request[keyv1.RotateKeySecretRequest]) (*connect.Response[keyv1.RotateKeySecretResponse], error) {
	now := time.Now().UTC()

	const q = `
UPDATE key_secrets SET version = version + 1, value_encrypted = $2, updated_at = $3
WHERE key_secret_id = $1
RETURNING key_secret_id, key_id, upstream_target, version, active, description, created_at, updated_at`

	sec, err := scanKeySecret(s.pool.QueryRow(ctx, q, req.Msg.GetKeySecretId(), req.Msg.GetValue(), now))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&keyv1.RotateKeySecretResponse{Secret: sec}), nil
}

// DeleteKeySecret permanently removes a KeySecret.
func (s *KeyService) DeleteKeySecret(ctx context.Context, req *connect.Request[keyv1.DeleteKeySecretRequest]) (*connect.Response[keyv1.DeleteKeySecretResponse], error) {
	const q = `DELETE FROM key_secrets WHERE key_secret_id = $1`

	tag, err := s.pool.Exec(ctx, q, req.Msg.GetKeySecretId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if tag.RowsAffected() == 0 {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("key secret not found"))
	}

	return connect.NewResponse(&keyv1.DeleteKeySecretResponse{}), nil
}

// ── scan helpers ──────────────────────────────────────────────────────────────

type scanner interface {
	Scan(dest ...any) error
}

func scanKey(row scanner) (*keyv1.Key, error) {
	var (
		id           string
		workspaceID  string
		userID       string
		name         string
		keyFormat    string
		description  *string
		routingShape []byte
		createdAt    time.Time
		updatedAt    time.Time
	)
	if err := row.Scan(&id, &workspaceID, &userID, &name, &keyFormat, &description, &routingShape, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	overrides, err := unmarshalRoutingOverrides(routingShape)
	if err != nil {
		return nil, err
	}
	return &keyv1.Key{
		KeyId:            id,
		WorkspaceId:      workspaceID,
		UserId:           userID,
		Name:             name,
		KeyFormat:        keyFormat,
		Description:      description,
		RoutingOverrides: overrides,
		CreatedAt:        timestamppb.New(createdAt),
		UpdatedAt:        timestamppb.New(updatedAt),
	}, nil
}

func marshalRoutingOverrides(overrides []*routingv1.RoutingOverride) ([]byte, error) {
	if len(overrides) == 0 {
		return nil, nil
	}
	arr := make([]json.RawMessage, len(overrides))
	for i, o := range overrides {
		b, err := protojson.Marshal(o)
		if err != nil {
			return nil, err
		}
		arr[i] = b
	}
	return json.Marshal(arr)
}

func unmarshalRoutingOverrides(data []byte) ([]*routingv1.RoutingOverride, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(data, &arr); err != nil {
		return nil, err
	}
	result := make([]*routingv1.RoutingOverride, len(arr))
	for i, raw := range arr {
		o := &routingv1.RoutingOverride{}
		if err := protojson.Unmarshal(raw, o); err != nil {
			return nil, err
		}
		result[i] = o
	}
	return result, nil
}

func scanPASETOToken(row scanner) (*keyv1.PASETOToken, error) {
	var (
		tokenID   string
		keyID     string
		jti       string
		iat       time.Time
		exp       time.Time
		pol       *string
		tokenHash string
		revoked   bool
		createdAt time.Time
	)
	if err := row.Scan(&tokenID, &keyID, &jti, &iat, &exp, &pol, &tokenHash, &revoked, &createdAt); err != nil {
		return nil, err
	}
	return &keyv1.PASETOToken{
		TokenId:   tokenID,
		KeyId:     keyID,
		Jti:       jti,
		Iat:       timestamppb.New(iat),
		Exp:       timestamppb.New(exp),
		Pol:       pol,
		TokenHash: tokenHash,
		Revoked:   revoked,
		CreatedAt: timestamppb.New(createdAt),
	}, nil
}

func scanPASETOTokenRow(rows pgx.Rows) (*keyv1.PASETOToken, error) {
	var (
		tokenID   string
		keyID     string
		jti       string
		iat       time.Time
		exp       time.Time
		pol       *string
		tokenHash string
		revoked   bool
		createdAt time.Time
	)
	if err := rows.Scan(&tokenID, &keyID, &jti, &iat, &exp, &pol, &tokenHash, &revoked, &createdAt); err != nil {
		return nil, err
	}
	return &keyv1.PASETOToken{
		TokenId:   tokenID,
		KeyId:     keyID,
		Jti:       jti,
		Iat:       timestamppb.New(iat),
		Exp:       timestamppb.New(exp),
		Pol:       pol,
		TokenHash: tokenHash,
		Revoked:   revoked,
		CreatedAt: timestamppb.New(createdAt),
	}, nil
}

func scanKeySecret(row scanner) (*keyv1.KeySecret, error) {
	var (
		keySecretID    string
		keyID          string
		upstreamTarget string
		version        int32
		active         bool
		description    *string
		createdAt      time.Time
		updatedAt      time.Time
	)
	if err := row.Scan(&keySecretID, &keyID, &upstreamTarget, &version, &active, &description, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	return &keyv1.KeySecret{
		KeySecretId:    keySecretID,
		KeyId:          keyID,
		UpstreamTarget: upstreamTarget,
		Version:        version,
		Active:         active,
		Description:    description,
		CreatedAt:      timestamppb.New(createdAt),
		UpdatedAt:      timestamppb.New(updatedAt),
	}, nil
}

func scanKeySecretRow(rows pgx.Rows) (*keyv1.KeySecret, error) {
	var (
		keySecretID    string
		keyID          string
		upstreamTarget string
		version        int32
		active         bool
		description    *string
		createdAt      time.Time
		updatedAt      time.Time
	)
	if err := rows.Scan(&keySecretID, &keyID, &upstreamTarget, &version, &active, &description, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	return &keyv1.KeySecret{
		KeySecretId:    keySecretID,
		KeyId:          keyID,
		UpstreamTarget: upstreamTarget,
		Version:        version,
		Active:         active,
		Description:    description,
		CreatedAt:      timestamppb.New(createdAt),
		UpdatedAt:      timestamppb.New(updatedAt),
	}, nil
}
