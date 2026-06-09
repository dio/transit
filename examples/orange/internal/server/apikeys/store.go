// Package apikeys manages the api_keys table.
//
// Key format:
//
//	admin scope  → sk-org-<base64url-raw-32bytes>
//	other scopes → sk-<base64url-raw-32bytes>
//
// Only the SHA-256 hex hash is stored; the plaintext is returned once.
package apikeys

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dio/transit/examples/orange/internal/server/scopes"
)

const (
	// ScopeAdmin is the super-admin scope (proto annotation alias; satisfies
	// every scope check). Prefer ScopeOrgAdmin for new code.
	ScopeAdmin    = "admin"
	ScopeOrgAdmin = "org:admin"

	// ScopeUserRead is the minimal scope issued to every new user key.
	ScopeUserRead = "user:read"

	// Base forms of workspace-scoped action scopes. Keys carry the contextual
	// form (e.g. "token:issue[ws-abc]"); these base forms appear in proto
	// annotations and are preserved for compatibility.
	ScopeTokenIssue           = "token:issue"
	ScopeEgressBundleDownload = "egress-bundle:download"
)

// DefaultUserScopes is the scope set issued when --scope is omitted on user create.
var DefaultUserScopes = []string{ScopeUserRead}

// Record is a row from api_keys.
type Record struct {
	KeyID       string
	KeyPrefix   string // first 12 chars of the plaintext token (for display)
	OrgID       string
	UserID      string // empty for org-level admin keys
	WorkspaceID string // empty for non-workspace-scoped keys
	Scopes      []string
	Description string
	CreatedAt   time.Time
}

// Store manages api_keys.
type Store struct {
	pool *pgxpool.Pool
}

const ddl = `
CREATE TABLE IF NOT EXISTS api_keys (
  key_id             TEXT PRIMARY KEY,
  key_hash           TEXT NOT NULL,
  key_prefix         TEXT NOT NULL,
  org_id             TEXT NOT NULL,
  user_id            TEXT,
  workspace_id       TEXT,
  scopes             TEXT[] NOT NULL DEFAULT '{admin}',
  description        TEXT,
  active             BOOL NOT NULL DEFAULT TRUE,
  expires_at         TIMESTAMPTZ,
  last_used_at       TIMESTAMPTZ,
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at         TIMESTAMPTZ,
  supersedes_key_id  TEXT
);
CREATE INDEX IF NOT EXISTS api_keys_org_idx       ON api_keys (org_id)   WHERE active = TRUE;
CREATE INDEX IF NOT EXISTS api_keys_hash_idx      ON api_keys (key_hash) WHERE active = TRUE;
CREATE UNIQUE INDEX IF NOT EXISTS api_keys_hash_active_idx ON api_keys (key_hash) WHERE active = TRUE`

// migrations are applied after CREATE TABLE IF NOT EXISTS to bring older schemas
// up to date without requiring a full purge.
const migrations = `
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS revoked_at        TIMESTAMPTZ;
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS supersedes_key_id TEXT;
ALTER TABLE api_keys DROP CONSTRAINT IF EXISTS api_keys_key_hash_key`

// NewStore creates the api_keys table and returns a Store.
func NewStore(ctx context.Context, pool *pgxpool.Pool) (*Store, error) {
	for _, stmt := range splitStmts(ddl) {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return nil, err
		}
	}
	// Apply incremental migrations (idempotent).
	for _, stmt := range splitStmts(migrations) {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return nil, err
		}
	}
	return &Store{pool: pool}, nil
}

// Issue generates a new API key, stores its hash, and returns the plaintext token.
// The plaintext is returned exactly once; it is never stored.
func (s *Store) Issue(ctx context.Context, orgID, userID, workspaceID string, scopes []string, description string) (plaintext string, rec Record, err error) {
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", Record{}, err
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)

	prefix := "sk-"
	for _, sc := range scopes {
		if sc == ScopeAdmin || sc == ScopeOrgAdmin {
			prefix = "sk-org-"
			break
		}
	}
	plaintext = prefix + encoded
	keyPrefix := plaintext[:min(12, len(plaintext))]
	hash := hashToken(plaintext)
	keyID := uuid.Must(uuid.NewV7()).String()

	const q = `
INSERT INTO api_keys (key_id, key_hash, key_prefix, org_id, user_id, workspace_id, scopes, description)
VALUES ($1, $2, $3, $4, NULLIF($5,''), NULLIF($6,''), $7, NULLIF($8,''))`
	if _, err = s.pool.Exec(ctx, q, keyID, hash, keyPrefix, orgID, userID, workspaceID, scopes, description); err != nil {
		return "", Record{}, err
	}
	return plaintext, Record{
		KeyID:       keyID,
		KeyPrefix:   keyPrefix,
		OrgID:       orgID,
		UserID:      userID,
		WorkspaceID: workspaceID,
		Scopes:      scopes,
		Description: description,
		CreatedAt:   time.Now().UTC(),
	}, nil
}

// Validate looks up a plaintext token and returns its record.
// Updates last_used_at as a side effect.
func (s *Store) Validate(ctx context.Context, plaintext string) (Record, error) {
	hash := hashToken(plaintext)
	const q = `
UPDATE api_keys
SET last_used_at = now()
WHERE key_hash = $1 AND active = TRUE AND (expires_at IS NULL OR expires_at > now())
RETURNING key_id, org_id, COALESCE(user_id,''), COALESCE(workspace_id,''), scopes, COALESCE(description,''), created_at`
	var rec Record
	err := s.pool.QueryRow(ctx, q, hash).
		Scan(&rec.KeyID, &rec.OrgID, &rec.UserID, &rec.WorkspaceID, &rec.Scopes, &rec.Description, &rec.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrInvalidKey
	}
	if err != nil {
		return Record{}, err
	}
	return rec, nil
}

// List returns all active keys for an org, optionally filtered by user_id.
func (s *Store) List(ctx context.Context, orgID, userID string) ([]Record, error) {
	const q = `
SELECT key_id, key_prefix, org_id, COALESCE(user_id,''), COALESCE(workspace_id,''), scopes, COALESCE(description,''), created_at
FROM api_keys
WHERE org_id = $1 AND ($2 = '' OR user_id = $2) AND active = TRUE
ORDER BY created_at DESC`
	rows, err := s.pool.Query(ctx, q, orgID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.KeyID, &r.KeyPrefix, &r.OrgID, &r.UserID, &r.WorkspaceID, &r.Scopes, &r.Description, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Get returns the metadata for a single active key by key_id.
func (s *Store) Get(ctx context.Context, keyID string) (Record, error) {
	const q = `
SELECT key_id, key_prefix, org_id, COALESCE(user_id,''), COALESCE(workspace_id,''), scopes, COALESCE(description,''), created_at
FROM api_keys
WHERE key_id = $1 AND active = TRUE`
	var r Record
	err := s.pool.QueryRow(ctx, q, keyID).
		Scan(&r.KeyID, &r.KeyPrefix, &r.OrgID, &r.UserID, &r.WorkspaceID, &r.Scopes, &r.Description, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrKeyNotFound
	}
	if err != nil {
		return Record{}, err
	}
	return r, nil
}

// AppendScopes merges add into the existing scope list of keyID and returns the
// updated record. The old key row is revoked and superseded by a new row that
// carries the merged scopes but the same key material (key_hash / key_prefix),
// so the caller's plaintext token continues to authenticate.
func (s *Store) AppendScopes(ctx context.Context, keyID string, add []string) (Record, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Record{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	const selectQ = `
SELECT key_id, key_hash, key_prefix, org_id, COALESCE(user_id,''), COALESCE(workspace_id,''), scopes, COALESCE(description,'')
FROM api_keys
WHERE key_id = $1 AND active = TRUE
FOR UPDATE`
	var (
		kID, kHash, kPrefix, orgID, userID, wsID, description string
		currentScopes                                         []string
	)
	err = tx.QueryRow(ctx, selectQ, keyID).
		Scan(&kID, &kHash, &kPrefix, &orgID, &userID, &wsID, &currentScopes, &description)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrKeyNotFound
	}
	if err != nil {
		return Record{}, err
	}

	// Merge: deduplicate add into current.
	have := make(map[string]bool, len(currentScopes))
	for _, s := range currentScopes {
		have[s] = true
	}
	merged := make([]string, len(currentScopes))
	copy(merged, currentScopes)
	for _, s := range add {
		if !have[s] {
			merged = append(merged, s)
			have[s] = true
		}
	}

	now := time.Now().UTC()
	if _, err := tx.Exec(ctx,
		`UPDATE api_keys SET active = FALSE, revoked_at = $1 WHERE key_id = $2`,
		now, kID,
	); err != nil {
		return Record{}, err
	}

	newKeyID := uuid.Must(uuid.NewV7()).String()
	const insertQ = `
INSERT INTO api_keys
  (key_id, key_hash, key_prefix, org_id, user_id, workspace_id, scopes, description, supersedes_key_id)
VALUES ($1, $2, $3, $4, NULLIF($5,''), NULLIF($6,''), $7, NULLIF($8,''), $9)`
	if _, err := tx.Exec(ctx, insertQ,
		newKeyID, kHash, kPrefix, orgID, userID, wsID, merged, description, kID,
	); err != nil {
		return Record{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Record{}, err
	}
	return Record{
		KeyID:       newKeyID,
		KeyPrefix:   kPrefix,
		OrgID:       orgID,
		UserID:      userID,
		WorkspaceID: wsID,
		Scopes:      merged,
		Description: description,
		CreatedAt:   now,
	}, nil
}

// Revoke deactivates a key by key_id.
func (s *Store) Revoke(ctx context.Context, keyID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE api_keys SET active = FALSE WHERE key_id = $1`, keyID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrKeyNotFound
	}
	return nil
}

// BindWorkspace atomically updates all active keys for userID: each key is
// revoked and superseded by a new key with the same key material (key_hash)
// and the workspace member scopes for wsID appended. If the key has no
// workspace_id set yet, it is set to wsID so that workspace-scoped RPCs
// (e.g. IssueNamedToken) can derive the workspace from the key record.
//
// All changes execute in a single transaction. On failure the whole operation
// rolls back — no partial updates are left behind.
func (s *Store) BindWorkspace(ctx context.Context, orgID, userID, wsID string) error {
	return s.updateKeyScopes(ctx, orgID, userID,
		func(current []string) []string {
			return scopes.AppendWorkspaceScopesForUser(current, wsID, userID)
		},
		func(currentWsID string) string {
			if currentWsID == "" {
				return wsID
			}
			return currentWsID
		},
	)
}

// UnbindWorkspace atomically removes all workspace-context scopes for wsID
// from every active key belonging to userID. Same atomicity guarantee as
// BindWorkspace.
func (s *Store) UnbindWorkspace(ctx context.Context, orgID, userID, wsID string) error {
	return s.updateKeyScopes(ctx, orgID, userID,
		func(current []string) []string {
			return scopes.RemoveWorkspaceScopes(current, wsID)
		},
		func(currentWsID string) string {
			if currentWsID == wsID {
				return ""
			}
			return currentWsID
		},
	)
}

// updateKeyScopes is the shared transaction skeleton for Bind/UnbindWorkspace.
// It locks all active keys for userID, applies transformScopes to the scope
// list and transformWsID to the workspace_id column, then revokes the old row
// and inserts a superseding row with the updated values.
func (s *Store) updateKeyScopes(ctx context.Context, orgID, userID string,
	transformScopes func([]string) []string,
	transformWsID func(string) string,
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	const selectQ = `
SELECT key_id, key_hash, key_prefix, scopes, COALESCE(description,''), COALESCE(workspace_id,'')
FROM api_keys
WHERE user_id = $1 AND active = TRUE
ORDER BY created_at
FOR UPDATE`

	rows, err := tx.Query(ctx, selectQ, userID)
	if err != nil {
		return err
	}

	type keyRow struct {
		keyID, keyHash, keyPrefix, description, workspaceID string
		currentScopes                                       []string
	}
	var keys []keyRow
	for rows.Next() {
		var k keyRow
		if err := rows.Scan(&k.keyID, &k.keyHash, &k.keyPrefix, &k.currentScopes, &k.description, &k.workspaceID); err != nil {
			rows.Close()
			return err
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	now := time.Now().UTC()
	for _, k := range keys {
		newScopes := transformScopes(k.currentScopes)
		newWsID := transformWsID(k.workspaceID)

		// Revoke old key.
		if _, err := tx.Exec(ctx,
			`UPDATE api_keys SET active = FALSE, revoked_at = $1 WHERE key_id = $2`,
			now, k.keyID,
		); err != nil {
			return err
		}

		// Insert superseding key with same key material.
		newKeyID := uuid.Must(uuid.NewV7()).String()
		const insertQ = `
INSERT INTO api_keys
  (key_id, key_hash, key_prefix, org_id, user_id, workspace_id, scopes, description, supersedes_key_id)
VALUES ($1, $2, $3, $4, $5, NULLIF($6,''), $7, NULLIF($8,''), $9)`
		if _, err := tx.Exec(ctx, insertQ,
			newKeyID, k.keyHash, k.keyPrefix, orgID, userID, newWsID,
			newScopes, k.description, k.keyID,
		); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

// HasOrgs returns true when at least one org exists (bootstrap guard).
func HasOrgs(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var n int
	err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM orgs LIMIT 1`).Scan(&n)
	return n > 0, err
}

// HasScope reports whether the record carries the given scope.
// Matching rules (see scopes.HasScope for full semantics):
//   - "admin" or "org:admin" in the key satisfies any requirement
//   - exact match: scope == required
//   - base match:  "token:issue[ws-abc]" satisfies "token:issue"
func (r Record) HasScope(s string) bool {
	return scopes.HasScope(r.Scopes, s)
}

// IsMember reports whether user_id has a row in workspace_members for workspace_id.
func (s *Store) IsMember(ctx context.Context, workspaceID, userID string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM workspace_members WHERE workspace_id = $1 AND user_id = $2)`,
		workspaceID, userID,
	).Scan(&exists)
	return exists, err
}

var (
	ErrInvalidKey  = errors.New("invalid or expired API key")
	ErrKeyNotFound = errors.New("key not found")
)

func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

func splitStmts(sql string) []string {
	var out []string
	for _, s := range strings.Split(sql, ";") {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
