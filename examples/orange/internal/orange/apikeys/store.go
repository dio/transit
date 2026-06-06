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
)

const (
	ScopeAdmin = "admin"
	ScopeProxy = "proxy"
	ScopeUser  = "user"
)

// Record is a row from api_keys.
type Record struct {
	KeyID       string
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
  key_id        TEXT PRIMARY KEY,
  key_hash      TEXT NOT NULL UNIQUE,
  key_prefix    TEXT NOT NULL,
  org_id        TEXT NOT NULL,
  user_id       TEXT,
  workspace_id  TEXT,
  scopes        TEXT[] NOT NULL DEFAULT '{admin}',
  description   TEXT,
  active        BOOL NOT NULL DEFAULT TRUE,
  expires_at    TIMESTAMPTZ,
  last_used_at  TIMESTAMPTZ,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS api_keys_org_idx  ON api_keys (org_id)  WHERE active = TRUE;
CREATE INDEX IF NOT EXISTS api_keys_hash_idx ON api_keys (key_hash) WHERE active = TRUE;`

// NewStore creates the api_keys table and returns a Store.
func NewStore(ctx context.Context, pool *pgxpool.Pool) (*Store, error) {
	for _, stmt := range splitStmts(ddl) {
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
		if sc == ScopeAdmin {
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

// HasOrgs returns true when at least one org exists (bootstrap guard).
func HasOrgs(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var n int
	err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM orgs LIMIT 1`).Scan(&n)
	return n > 0, err
}

// HasScope reports whether the record carries the given scope.
func (r Record) HasScope(s string) bool {
	for _, sc := range r.Scopes {
		if sc == s {
			return true
		}
	}
	return false
}

var (
	ErrInvalidKey = errors.New("invalid or expired API key")
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

