package resources

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	keyentryv1 "github.com/dio/transit/examples/orange/api/orange/keyentry/admin/v1"
	adminv1connect "github.com/dio/transit/examples/orange/api/orange/keyentry/admin/v1/adminv1connect"
	routingv1 "github.com/dio/transit/examples/orange/api/orange/routing/v1"
	"github.com/dio/transit/examples/orange/internal/server/apikeys"
	"github.com/dio/transit/examples/orange/internal/server/secret"
)

// KeyEntryService implements adminv1connect.KeyEntryAdminServiceHandler using a PostgreSQL pool.
//
// # Terminology
//
// There are three distinct "key" concepts in the system; this service owns the
// first one and the two tables that hang off it:
//
//  1. key_entries — a named credential slot that binds a user to a workspace.
//     It is NOT an API key and NOT a PASETO token. Think of it as the identity
//     record that the egress proxy uses to look up how to route requests:
//     "when a PASETO token for workspace/user/name arrives, apply these
//     routing_overrides." One user can have multiple named slots per workspace
//     (e.g. "default", "batch"), each with independent routing configuration.
//     Created on first token issue: `orange token create --name=<slot>` inserts
//     the key_entry row and issues a PASETO token against it atomically.
//
//  2. paseto_tokens — PASETO v4.public tokens issued against a key_entry.
//     The plaintext token is returned once on issue and never stored; only its
//     SHA-256 hash (token_hash) is persisted. Tokens are revocable. The egress
//     validates them offline using the workspace PASETO public keypair.
//
//  3. key_secrets (BYOK) — per-key upstream credentials a user supplies to
//     override the workspace-level provider secret for a specific upstream
//     target. Stored encrypted; never returned by read APIs.
//
// Contrast with api_keys (managed by apikeys.Store): those are the admin/user
// credentials for authenticating to the management plane (Authorization: Bearer
// sk-...). They are completely separate from key_entries.
type KeyEntryService struct {
	adminv1connect.UnimplementedKeyEntryAdminServiceHandler
	pool      *pgxpool.Pool
	logger    *slog.Logger
	secretSvc *secret.Service
}

// NewKeyEntryService creates the key_entries, paseto_tokens, and key_secrets
// tables if they do not exist and returns a new KeyEntryService.
func NewKeyEntryService(pool *pgxpool.Pool, logger *slog.Logger, secretSvc *secret.Service) (*KeyEntryService, error) {
	ddl := `
CREATE TABLE IF NOT EXISTS key_entries (
  -- key_entry_id is the stable surrogate primary key (UUIDv7).
  key_entry_id         TEXT PRIMARY KEY,
  -- (workspace_id, user_id, name) is the natural key: one user may have
  -- multiple named slots per workspace but the triple must be unique.
  workspace_id   TEXT NOT NULL,
  user_id        TEXT NOT NULL,
  name           TEXT NOT NULL,
  -- key_format is always 'paseto_v4.public' for now (reserved for future key types)
  key_format     TEXT NOT NULL DEFAULT 'paseto_v4.public',
  description    TEXT,
  -- routing_shape holds a JSONB array of protojson-encoded RoutingOverride
  -- messages. It maps client-facing model IDs to routing tree roots and is
  -- included verbatim in the config snapshot delivered to the egress proxy.
  routing_shape  JSONB,
  slug           TEXT NOT NULL DEFAULT '',
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at     TIMESTAMPTZ,
  UNIQUE (workspace_id, user_id, name)
);
ALTER TABLE key_entries ADD COLUMN IF NOT EXISTS slug TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS key_entries_slug_idx ON key_entries (slug) WHERE slug != '';
CREATE INDEX IF NOT EXISTS key_entries_workspace_idx     ON key_entries (workspace_id)             WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS key_entries_workspace_usr_idx ON key_entries (workspace_id, user_id)    WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS key_entries_updated_at_idx    ON key_entries (updated_at DESC)          WHERE deleted_at IS NULL;
CREATE TABLE IF NOT EXISTS paseto_tokens (
  token_id    TEXT PRIMARY KEY,
  -- key_entry_id references the key_entry this token was issued against.
  -- The token inherits the routing_shape of the key_entry at request time
  -- (not baked in at issue time), so routing changes take effect immediately.
  key_entry_id      TEXT NOT NULL,
  jti         TEXT NOT NULL UNIQUE,
  iat         TIMESTAMPTZ NOT NULL,
  exp         TIMESTAMPTZ NOT NULL,
  -- pol is an optional JSON policy embedded in the token payload.
  pol         TEXT,
  -- token_hash is SHA-256(plaintext_token). The plaintext is never stored.
  token_hash  TEXT NOT NULL,
  revoked     BOOLEAN NOT NULL DEFAULT false,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE paseto_tokens ADD COLUMN IF NOT EXISTS paseto_keypair_id TEXT NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS key_secrets (
  key_secret_id   TEXT PRIMARY KEY,
  -- key_entry_id references the key_entry this BYOK credential is scoped to.
  key_entry_id          TEXT NOT NULL,
  -- upstream_target identifies the provider this credential overrides,
  -- e.g. "openai" or "anthropic". One entry per (key_entry_id, upstream_target).
  upstream_target TEXT NOT NULL,
  version         INT NOT NULL DEFAULT 1,
  -- value_encrypted holds the ciphertext (the plaintext is never returned).
  value_encrypted TEXT NOT NULL,
  active          BOOLEAN NOT NULL DEFAULT true,
  description     TEXT,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (key_entry_id, upstream_target)
)`
	for _, stmt := range splitDDL(ddl) {
		if _, err := pool.Exec(context.Background(), stmt); err != nil {
			return nil, err
		}
	}
	return &KeyEntryService{pool: pool, logger: logger, secretSvc: secretSvc}, nil
}

// ── Key CRUD ──────────────────────────────────────────────────────────────────

// CreateKey inserts a new key and returns it.
func (s *KeyEntryService) CreateKey(ctx context.Context, req *connect.Request[keyentryv1.CreateKeyRequest]) (*connect.Response[keyentryv1.CreateKeyResponse], error) {
	keyID := uuid.Must(uuid.NewV7()).String()
	now := time.Now().UTC()

	routingJSON, err := marshalRoutingOverrides(req.Msg.GetRoutingOverrides())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	const q = `
INSERT INTO key_entries (key_entry_id, workspace_id, user_id, name, key_format, description, routing_shape, slug, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'paseto_v4.public', $5, $6, $7, $8, $8)
RETURNING key_entry_id, workspace_id, user_id, name, key_format, description, routing_shape, created_at, updated_at`

	var key *keyentryv1.Key
	for range maxSlugRetries {
		slug, slugErr := newKeyEntrySlug()
		if slugErr != nil {
			return nil, connect.NewError(connect.CodeInternal, slugErr)
		}
		var scanErr error
		key, scanErr = scanKey(s.pool.QueryRow(ctx, q,
			keyID, req.Msg.GetWorkspaceId(), req.Msg.GetUserId(), req.Msg.GetName(),
			req.Msg.Description, routingJSON, slug, now,
		))
		if scanErr == nil {
			break
		}
		var pgErr *pgconn.PgError
		if errors.As(scanErr, &pgErr) && pgErr.Code == "23505" {
			if pgErr.ConstraintName == "key_entries_slug_idx" {
				continue
			}
			return nil, connect.NewError(connect.CodeAlreadyExists, scanErr)
		}
		return nil, connect.NewError(connect.CodeInternal, scanErr)
	}
	if key == nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to generate unique key entry slug after %d attempts", maxSlugRetries))
	}
	return connect.NewResponse(&keyentryv1.CreateKeyResponse{Key: key}), nil
}

// IssueNamedToken atomically creates (or reuses) the named key entry, then
// issues a PASETO v4.public token. workspace_id and user_id are derived from
// the authenticated API key record injected by the apikeys interceptor — the
// key must be workspace-scoped. Requires scope "token:issue".
func (s *KeyEntryService) IssueNamedToken(ctx context.Context, req *connect.Request[keyentryv1.IssueNamedTokenRequest]) (*connect.Response[keyentryv1.IssueNamedTokenResponse], error) {
	rec, ok := apikeys.RecordFromContext(ctx)
	if !ok || rec.WorkspaceID == "" || rec.UserID == "" {
		return nil, connect.NewError(connect.CodeUnauthenticated,
			errors.New("IssueNamedToken requires a workspace-scoped API key (workspace_id and user_id must be set)"))
	}

	now := time.Now().UTC()

	// Upsert the key entry: insert if (workspace_id, user_id, name) is new,
	// otherwise return the existing row unchanged. Retry if the generated slug
	// collides with an existing one (extremely rare but correct to handle).
	const upsertQ = `
INSERT INTO key_entries (key_entry_id, workspace_id, user_id, name, key_format, description, routing_shape, slug, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'paseto_v4.public', $5, NULL, $6, $7, $7)
ON CONFLICT (workspace_id, user_id, name) DO UPDATE SET updated_at = key_entries.updated_at
RETURNING key_entry_id, workspace_id, user_id, name, key_format, description, routing_shape, created_at, updated_at`

	var key *keyentryv1.Key
	for range maxSlugRetries {
		slug, slugErr := newKeyEntrySlug()
		if slugErr != nil {
			return nil, connect.NewError(connect.CodeInternal, slugErr)
		}
		var scanErr error
		key, scanErr = scanKey(s.pool.QueryRow(ctx, upsertQ,
			uuid.Must(uuid.NewV7()).String(),
			rec.WorkspaceID, rec.UserID, req.Msg.GetName(),
			req.Msg.Description, slug, now,
		))
		if scanErr == nil {
			break
		}
		var pgErr *pgconn.PgError
		if errors.As(scanErr, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "key_entries_slug_idx" {
			continue
		}
		return nil, connect.NewError(connect.CodeInternal, scanErr)
	}
	if key == nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to generate unique key entry slug after %d attempts", maxSlugRetries))
	}

	// Reuse the shared signing path.
	issueResp, err := s.IssueToken(ctx, connect.NewRequest(&keyentryv1.IssueTokenRequest{
		KeyEntryId: key.GetKeyEntryId(),
		TtlSeconds: req.Msg.GetTtlSeconds(),
		Pol:        req.Msg.Pol,
	}))
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&keyentryv1.IssueNamedTokenResponse{
		Token:    issueResp.Msg.GetToken(),
		Metadata: issueResp.Msg.GetMetadata(),
		Key:      key,
	}), nil
}

// GetKey fetches an active key by key_entry_id.
func (s *KeyEntryService) GetKey(ctx context.Context, req *connect.Request[keyentryv1.GetKeyRequest]) (*connect.Response[keyentryv1.GetKeyResponse], error) {
	const q = `SELECT key_entry_id, workspace_id, user_id, name, key_format, description, routing_shape, created_at, updated_at
FROM key_entries WHERE key_entry_id = $1 AND deleted_at IS NULL`

	key, err := scanKey(s.pool.QueryRow(ctx, q, req.Msg.GetKeyEntryId()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&keyentryv1.GetKeyResponse{Key: key}), nil
}

// ListKeys returns a page of keys for a workspace, optionally filtered by user_id.
func (s *KeyEntryService) ListKeys(ctx context.Context, req *connect.Request[keyentryv1.ListKeysRequest]) (*connect.Response[keyentryv1.ListKeysResponse], error) {
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
		const q = `SELECT key_entry_id, workspace_id, user_id, name, key_format, description, routing_shape, created_at, updated_at
FROM key_entries WHERE workspace_id = $1 AND user_id = $2 AND key_entry_id > $3 AND deleted_at IS NULL ORDER BY key_entry_id LIMIT $4`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetWorkspaceId(), *userID, pageToken, limit)
	case pageToken != "":
		const q = `SELECT key_entry_id, workspace_id, user_id, name, key_format, description, routing_shape, created_at, updated_at
FROM key_entries WHERE workspace_id = $1 AND key_entry_id > $2 AND deleted_at IS NULL ORDER BY key_entry_id LIMIT $3`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetWorkspaceId(), pageToken, limit)
	case userID != nil:
		const q = `SELECT key_entry_id, workspace_id, user_id, name, key_format, description, routing_shape, created_at, updated_at
FROM key_entries WHERE workspace_id = $1 AND user_id = $2 AND deleted_at IS NULL ORDER BY key_entry_id LIMIT $3`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetWorkspaceId(), *userID, limit)
	default:
		const q = `SELECT key_entry_id, workspace_id, user_id, name, key_format, description, routing_shape, created_at, updated_at
FROM key_entries WHERE workspace_id = $1 AND deleted_at IS NULL ORDER BY key_entry_id LIMIT $2`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetWorkspaceId(), limit)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer rows.Close()

	var keys []*keyentryv1.Key
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
		nextPageToken = keys[len(keys)-1].KeyEntryId
	}

	return connect.NewResponse(&keyentryv1.ListKeysResponse{
		Keys:          keys,
		NextPageToken: nextPageToken,
	}), nil
}

// UpdateKey updates the description and/or routing overrides of a key.
func (s *KeyEntryService) UpdateKey(ctx context.Context, req *connect.Request[keyentryv1.UpdateKeyRequest]) (*connect.Response[keyentryv1.UpdateKeyResponse], error) {
	now := time.Now().UTC()

	routingJSON, err := marshalRoutingOverrides(req.Msg.GetRoutingOverrides())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	const q = `
UPDATE key_entries SET description = $2, routing_shape = $3, updated_at = $4
WHERE key_entry_id = $1 AND deleted_at IS NULL
RETURNING key_entry_id, workspace_id, user_id, name, key_format, description, routing_shape, created_at, updated_at`

	key, err := scanKey(s.pool.QueryRow(ctx, q, req.Msg.GetKeyEntryId(), req.Msg.Description, routingJSON, now))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&keyentryv1.UpdateKeyResponse{Key: key}), nil
}

// DeleteKey soft-deletes a key by setting deleted_at.
func (s *KeyEntryService) DeleteKey(ctx context.Context, req *connect.Request[keyentryv1.DeleteKeyRequest]) (*connect.Response[keyentryv1.DeleteKeyResponse], error) {
	const q = `UPDATE key_entries SET deleted_at = now() WHERE key_entry_id = $1 AND deleted_at IS NULL`

	tag, err := s.pool.Exec(ctx, q, req.Msg.GetKeyEntryId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if tag.RowsAffected() == 0 {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("key not found"))
	}
	return connect.NewResponse(&keyentryv1.DeleteKeyResponse{}), nil
}

// ── PASETOToken management ────────────────────────────────────────────────────

// IssueToken signs a PASETO v4.public token using the workspace's active slot-1
// PASETO keypair created by EgressService.ProvisionForWorkspace. The private key
// is loaded from the secret service via private_key_ref; the egress validates
// tokens offline using the matching public key from its config snapshot.
func (s *KeyEntryService) IssueToken(ctx context.Context, req *connect.Request[keyentryv1.IssueTokenRequest]) (*connect.Response[keyentryv1.IssueTokenResponse], error) {
	// Load workspace_id and both active PASETO keypairs provisioned by
	// EgressService.ProvisionForWorkspace. We randomly pick one to spread
	// signing load; the egress accepts tokens from either slot.
	type pasetoKP struct {
		keypairID   string
		privKeyRef  string
		workspaceID string
		wsSlug      string
		slot        int
		keSlug      string
	}
	const lookupQ = `
SELECT ke.workspace_id, ep.paseto_keypair_id, ep.private_key_ref, ep.slot,
       COALESCE(ke.slug,''), COALESCE(w.slug,'')
FROM key_entries ke
JOIN egresses e ON e.workspace_id = ke.workspace_id
JOIN egress_paseto_keypairs ep ON ep.egress_id = e.egress_id
LEFT JOIN workspaces w ON w.workspace_id = ke.workspace_id
WHERE ke.key_entry_id = $1 AND ke.deleted_at IS NULL AND ep.active = true
ORDER BY ep.slot`
	rows, err := s.pool.Query(ctx, lookupQ, req.Msg.GetKeyEntryId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	var kps []pasetoKP
	for rows.Next() {
		var kp pasetoKP
		if err := rows.Scan(&kp.workspaceID, &kp.keypairID, &kp.privKeyRef, &kp.slot, &kp.keSlug, &kp.wsSlug); err != nil {
			rows.Close()
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		kps = append(kps, kp)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if len(kps) == 0 {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("key entry or workspace keypairs not found"))
	}
	chosen := kps[mrand.Intn(len(kps))]
	workspaceID, privKeyRef, wsSlug := chosen.workspaceID, chosen.privKeyRef, chosen.wsSlug
	pasetoKeypairID, slot, keSlug := chosen.keypairID, chosen.slot, chosen.keSlug

	// Resolve the private key DER from the secret service. Ref format: "<realm>/<secret_id>".
	idx := strings.Index(privKeyRef, "/")
	if idx < 0 {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("malformed private_key_ref: %q", privKeyRef))
	}
	realm, secretID := privKeyRef[:idx], privKeyRef[idx+1:]
	der, _, _, err := s.secretSvc.ResolveSecret(ctx, workspaceID, realm, secretID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("resolve paseto signing key: %w", err))
	}
	privKey, err := parseEd25519PrivateDER(der)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("parse paseto signing key: %w", err))
	}

	tokenID := uuid.Must(uuid.NewV7()).String()
	now := time.Now().UTC()

	ttlSec := req.Msg.GetTtlSeconds()
	var exp time.Time
	if ttlSec <= 0 {
		exp = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	} else {
		exp = now.Add(time.Duration(ttlSec) * time.Second)
	}

	tokenExp := exp
	if ttlSec <= 0 {
		tokenExp = time.Time{} // encode as 0 = never expires in the payload
	}
	payload, err := packTokenPayload(uint8(slot), keSlug, tokenID, tokenExp)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("pack token payload: %w", err))
	}
	signed, err := signPasetoV4Public(privKey, payload)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("sign token: %w", err))
	}
	// Format: sk-<workspace-slug>.<base64url-body>
	// "." separates slug from body because base64url never contains ".";
	// this stays unambiguous even when the workspace slug contains "-".
	// Egress parses by stripping "sk-" then splitting on the first ".".
	prefix := "sk-"
	if wsSlug != "" {
		prefix = "sk-" + wsSlug
	}
	token := prefix + "." + signed

	h := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(h[:])

	// jti reuses tokenID — they are the same unique identifier.
	const insertQ = `
INSERT INTO paseto_tokens (token_id, key_entry_id, jti, iat, exp, pol, token_hash, paseto_keypair_id, revoked, created_at)
VALUES ($1, $2, $1, $3, $4, $5, $6, $7, false, $3)
RETURNING token_id, key_entry_id, jti, iat, exp, pol, token_hash, revoked, created_at`

	tok, err := scanPASETOToken(s.pool.QueryRow(ctx, insertQ,
		tokenID, req.Msg.GetKeyEntryId(), now, exp, req.Msg.Pol, tokenHash, pasetoKeypairID,
	))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, connect.NewError(connect.CodeAlreadyExists, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&keyentryv1.IssueTokenResponse{
		Token:    token,
		Metadata: tok,
	}), nil
}

// GetToken returns metadata for a previously issued token.
func (s *KeyEntryService) GetToken(ctx context.Context, req *connect.Request[keyentryv1.GetTokenRequest]) (*connect.Response[keyentryv1.GetTokenResponse], error) {
	const q = `SELECT token_id, key_entry_id, jti, iat, exp, pol, token_hash, revoked, created_at FROM paseto_tokens WHERE token_id = $1`

	tok, err := scanPASETOToken(s.pool.QueryRow(ctx, q, req.Msg.GetTokenId()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&keyentryv1.GetTokenResponse{Token: tok}), nil
}

// ListTokens lists token metadata records for a key.
func (s *KeyEntryService) ListTokens(ctx context.Context, req *connect.Request[keyentryv1.ListTokensRequest]) (*connect.Response[keyentryv1.ListTokensResponse], error) {
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
		const q = `SELECT token_id, key_entry_id, jti, iat, exp, pol, token_hash, revoked, created_at
FROM paseto_tokens WHERE key_entry_id = $1 AND NOT revoked AND token_id > $2 ORDER BY token_id LIMIT $3`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetKeyEntryId(), pageToken, limit)
	case pageToken != "":
		const q = `SELECT token_id, key_entry_id, jti, iat, exp, pol, token_hash, revoked, created_at
FROM paseto_tokens WHERE key_entry_id = $1 AND token_id > $2 ORDER BY token_id LIMIT $3`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetKeyEntryId(), pageToken, limit)
	case !includeRevoked:
		const q = `SELECT token_id, key_entry_id, jti, iat, exp, pol, token_hash, revoked, created_at
FROM paseto_tokens WHERE key_entry_id = $1 AND NOT revoked ORDER BY token_id LIMIT $2`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetKeyEntryId(), limit)
	default:
		const q = `SELECT token_id, key_entry_id, jti, iat, exp, pol, token_hash, revoked, created_at
FROM paseto_tokens WHERE key_entry_id = $1 ORDER BY token_id LIMIT $2`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetKeyEntryId(), limit)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer rows.Close()

	var tokens []*keyentryv1.PASETOToken
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

	return connect.NewResponse(&keyentryv1.ListTokensResponse{
		Tokens:        tokens,
		NextPageToken: nextPageToken,
	}), nil
}

// RevokeToken marks a token as revoked.
func (s *KeyEntryService) RevokeToken(ctx context.Context, req *connect.Request[keyentryv1.RevokeTokenRequest]) (*connect.Response[keyentryv1.RevokeTokenResponse], error) {
	const q = `UPDATE paseto_tokens SET revoked = true WHERE token_id = $1`

	tag, err := s.pool.Exec(ctx, q, req.Msg.GetTokenId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if tag.RowsAffected() == 0 {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("token not found"))
	}

	return connect.NewResponse(&keyentryv1.RevokeTokenResponse{}), nil
}

// ── KeySecret (BYOK) management ──────────────────────────────────────────────

// CreateKeySecret creates a versioned upstream credential bound to a key.
func (s *KeyEntryService) CreateKeySecret(ctx context.Context, req *connect.Request[keyentryv1.CreateKeySecretRequest]) (*connect.Response[keyentryv1.CreateKeySecretResponse], error) {
	keySecretID := uuid.Must(uuid.NewV7()).String()
	now := time.Now().UTC()

	const q = `
INSERT INTO key_secrets (key_secret_id, key_entry_id, upstream_target, version, value_encrypted, active, description, created_at, updated_at)
VALUES ($1, $2, $3, 1, $4, true, $5, $6, $6)
RETURNING key_secret_id, key_entry_id, upstream_target, version, active, description, created_at, updated_at`

	sec, err := scanKeySecret(s.pool.QueryRow(ctx, q,
		keySecretID, req.Msg.GetKeyEntryId(), req.Msg.GetUpstreamTarget(), req.Msg.GetValue(), req.Msg.Description, now,
	))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, connect.NewError(connect.CodeAlreadyExists, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&keyentryv1.CreateKeySecretResponse{Secret: sec}), nil
}

// GetKeySecret returns a KeySecret record (value is never returned).
func (s *KeyEntryService) GetKeySecret(ctx context.Context, req *connect.Request[keyentryv1.GetKeySecretRequest]) (*connect.Response[keyentryv1.GetKeySecretResponse], error) {
	const q = `SELECT key_secret_id, key_entry_id, upstream_target, version, active, description, created_at, updated_at FROM key_secrets WHERE key_secret_id = $1`

	sec, err := scanKeySecret(s.pool.QueryRow(ctx, q, req.Msg.GetKeySecretId()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&keyentryv1.GetKeySecretResponse{Secret: sec}), nil
}

// ListKeySecrets lists all KeySecrets for a key.
func (s *KeyEntryService) ListKeySecrets(ctx context.Context, req *connect.Request[keyentryv1.ListKeySecretsRequest]) (*connect.Response[keyentryv1.ListKeySecretsResponse], error) {
	limit := int(req.Msg.GetLimit())
	if limit <= 0 {
		limit = 100
	}

	var (
		rows pgx.Rows
		err  error
	)

	if pageToken := req.Msg.GetPageToken(); pageToken != "" {
		const q = `SELECT key_secret_id, key_entry_id, upstream_target, version, active, description, created_at, updated_at
FROM key_secrets WHERE key_entry_id = $1 AND key_secret_id > $2 ORDER BY key_secret_id LIMIT $3`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetKeyEntryId(), pageToken, limit)
	} else {
		const q = `SELECT key_secret_id, key_entry_id, upstream_target, version, active, description, created_at, updated_at
FROM key_secrets WHERE key_entry_id = $1 ORDER BY key_secret_id LIMIT $2`
		rows, err = s.pool.Query(ctx, q, req.Msg.GetKeyEntryId(), limit)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer rows.Close()

	var secrets []*keyentryv1.KeySecret
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

	return connect.NewResponse(&keyentryv1.ListKeySecretsResponse{
		Secrets:       secrets,
		NextPageToken: nextPageToken,
	}), nil
}

// RotateKeySecret creates a new version of a KeySecret.
func (s *KeyEntryService) RotateKeySecret(ctx context.Context, req *connect.Request[keyentryv1.RotateKeySecretRequest]) (*connect.Response[keyentryv1.RotateKeySecretResponse], error) {
	now := time.Now().UTC()

	const q = `
UPDATE key_secrets SET version = version + 1, value_encrypted = $2, updated_at = $3
WHERE key_secret_id = $1
RETURNING key_secret_id, key_entry_id, upstream_target, version, active, description, created_at, updated_at`

	sec, err := scanKeySecret(s.pool.QueryRow(ctx, q, req.Msg.GetKeySecretId(), req.Msg.GetValue(), now))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&keyentryv1.RotateKeySecretResponse{Secret: sec}), nil
}

// DeleteKeySecret permanently removes a KeySecret.
func (s *KeyEntryService) DeleteKeySecret(ctx context.Context, req *connect.Request[keyentryv1.DeleteKeySecretRequest]) (*connect.Response[keyentryv1.DeleteKeySecretResponse], error) {
	const q = `DELETE FROM key_secrets WHERE key_secret_id = $1`

	tag, err := s.pool.Exec(ctx, q, req.Msg.GetKeySecretId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if tag.RowsAffected() == 0 {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("key secret not found"))
	}

	return connect.NewResponse(&keyentryv1.DeleteKeySecretResponse{}), nil
}

// ── scan helpers ──────────────────────────────────────────────────────────────

type scanner interface {
	Scan(dest ...any) error
}

func scanKey(row scanner) (*keyentryv1.Key, error) {
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
	return &keyentryv1.Key{
		KeyEntryId:       id,
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

func scanPASETOToken(row scanner) (*keyentryv1.PASETOToken, error) {
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
	return &keyentryv1.PASETOToken{
		TokenId:    tokenID,
		KeyEntryId: keyID,
		Jti:        jti,
		Iat:        timestamppb.New(iat),
		Exp:        timestamppb.New(exp),
		Pol:        pol,
		TokenHash:  tokenHash,
		Revoked:    revoked,
		CreatedAt:  timestamppb.New(createdAt),
	}, nil
}

func scanPASETOTokenRow(rows pgx.Rows) (*keyentryv1.PASETOToken, error) {
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
	return &keyentryv1.PASETOToken{
		TokenId:    tokenID,
		KeyEntryId: keyID,
		Jti:        jti,
		Iat:        timestamppb.New(iat),
		Exp:        timestamppb.New(exp),
		Pol:        pol,
		TokenHash:  tokenHash,
		Revoked:    revoked,
		CreatedAt:  timestamppb.New(createdAt),
	}, nil
}

func scanKeySecret(row scanner) (*keyentryv1.KeySecret, error) {
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
	return &keyentryv1.KeySecret{
		KeySecretId:    keySecretID,
		KeyEntryId:     keyID,
		UpstreamTarget: upstreamTarget,
		Version:        version,
		Active:         active,
		Description:    description,
		CreatedAt:      timestamppb.New(createdAt),
		UpdatedAt:      timestamppb.New(updatedAt),
	}, nil
}

// ── token payload ─────────────────────────────────────────────────────────────
//
// Two payload shapes, selected by the high bit (bit 7) of the slot byte:
//
//	Shape A — never expires (slot bit 7 = 0):  18 bytes → 82 raw → 110 base64url chars
//	Shape B — has expiry    (slot bit 7 = 1):  22 bytes → 86 raw → 115 base64url chars
//
//	Offset  Size  Field
//	     0     1  slot — low 7 bits: egress_paseto_keypairs.slot (1 or 2)
//	                     bit 7: 0 = shape A (never expires), 1 = shape B (has expiry)
//	     1     9  ent  — key_entries.slug, up to 9 ASCII bytes, zero-padded
//	    10     8  tid  — first 8 bytes of paseto_tokens.token_id (UUIDv7); serves as jti
//	── shape B only ──
//	    18     4  exp  — expiry, uint32 little-endian, seconds since tokenEpoch (2024-01-01 UTC)
//
// Signed with PASETO v4.public (Ed25519 + PAE):
//
//	msg = PAE("v4.public.", payload, "")
//	sig = ed25519.Sign(privKey, msg)   // 64 bytes
//	raw = payload || sig
//
// On the wire:
//
//	sk-<workspace-slug>.<base64url_nopad(raw)>
//
// "." separates slug from body because base64url never contains ".";
// unambiguous even when the workspace slug contains "-".
//
// Egress verification: strip "sk-", split on first ".", base64url-decode body.
// len(raw)==82 → shape A, len(raw)==86 → shape B.
// slot = raw[0] & 0x7F, ent = raw[1:10] (trim trailing zeros), tid = raw[10:18].
// Shape B: exp = tokenEpoch + LE32(raw[18:22]).

// tokenEpoch is the base for the 4-byte exp field (2024-01-01 UTC).
// uint32 seconds from this epoch overflows in ~2160.
var tokenEpoch = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Unix()

const (
	tokenPayloadNoExp   = 18 // shape A: slot + ent + tid
	tokenPayloadWithExp = 22 // shape B: slot + ent + tid + exp
	slotHasExpiry       = uint8(1 << 7)
	keSlugSize          = 9
	tidSize             = 8
)

// keSlugAlphabet is the 64-character set for key entry slugs.
// 64 divides 256 evenly so uniform byte mod 64 is unbiased.
const keSlugAlphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ_-"

// packTokenPayload serialises a token payload using shape A (no expiry) or
// shape B (has expiry). Pass exp.IsZero() for shape A; exp is then omitted
// and bit 7 of slot remains clear. For shape B, bit 7 of slot is set.
func packTokenPayload(slot uint8, keSlug string, tokenID string, exp time.Time) ([]byte, error) {
	id, err := uuid.Parse(tokenID)
	if err != nil {
		return nil, fmt.Errorf("parse token id: %w", err)
	}

	hasExp := !exp.IsZero()
	size := tokenPayloadNoExp
	if hasExp {
		size = tokenPayloadWithExp
		slot |= slotHasExpiry
	}

	buf := make([]byte, size)
	buf[0] = slot
	copy(buf[1:10], keSlug) // zero-padded to 9 bytes
	copy(buf[10:18], id[:tidSize])
	if hasExp {
		expSec := exp.Unix() - tokenEpoch
		if expSec < 0 {
			expSec = 0
		}
		binary.LittleEndian.PutUint32(buf[18:], uint32(expSec))
	}
	return buf, nil
}

// newKeyEntrySlug generates a cryptographically random 9-character slug from
// keSlugAlphabet. Callers must retry on key_entries_slug_idx constraint violations.
func newKeyEntrySlug() (string, error) {
	raw := make([]byte, keSlugSize)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	out := make([]byte, keSlugSize)
	for i, b := range raw {
		out[i] = keSlugAlphabet[int(b)%len(keSlugAlphabet)]
	}
	return string(out), nil
}

// signPasetoV4Public signs payload with PASETO v4.public (Ed25519 + PAE) and
// returns the bare base64url(payload||sig) without the "v4.public." header.
// Callers prepend their own prefix (e.g. "sk-<slug>-").
func signPasetoV4Public(key ed25519.PrivateKey, payload []byte) (string, error) {
	const header = "v4.public."
	msg := pasetoPreAuth([]byte(header), payload, []byte(""))
	sig := ed25519.Sign(key, msg)
	return base64.RawURLEncoding.EncodeToString(append(payload, sig...)), nil
}

// pasetoPreAuth implements PASETO Pre-Authentication Encoding (PAE).
func pasetoPreAuth(pieces ...[]byte) []byte {
	b := make([]byte, 8)
	var out []byte
	binary.LittleEndian.PutUint64(b, uint64(len(pieces)))
	out = append(out, b...)
	for _, p := range pieces {
		tmp := make([]byte, 8)
		binary.LittleEndian.PutUint64(tmp, uint64(len(p)))
		out = append(out, tmp...)
		out = append(out, p...)
	}
	return out
}

// ── key parsing ───────────────────────────────────────────────────────────────

// parseEd25519PrivateDER parses a PKCS8 DER-encoded Ed25519 private key as
// stored by EgressService.storeKey / secret.Service.
func parseEd25519PrivateDER(der []byte) (ed25519.PrivateKey, error) {
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	key, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("expected ed25519.PrivateKey, got %T", k)
	}
	return key, nil
}

func scanKeySecretRow(rows pgx.Rows) (*keyentryv1.KeySecret, error) {
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
	return &keyentryv1.KeySecret{
		KeySecretId:    keySecretID,
		KeyEntryId:     keyID,
		UpstreamTarget: upstreamTarget,
		Version:        version,
		Active:         active,
		Description:    description,
		CreatedAt:      timestamppb.New(createdAt),
		UpdatedAt:      timestamppb.New(updatedAt),
	}, nil
}
