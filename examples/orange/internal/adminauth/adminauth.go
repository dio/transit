// Package adminauth provides API-key authentication for the orange admin plane.
//
// Keys are prefixed with "osk_" (orange secret key) and stored as SHA-256
// hashes in the admin_api_keys table.  The Connect interceptor checks the
// Authorization: Bearer header on every inbound admin RPC.
package adminauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const tokenPrefix = "osk_"

// Store manages the admin_api_keys table.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore creates the admin_api_keys table if needed and returns a Store.
func NewStore(ctx context.Context, pool *pgxpool.Pool) (*Store, error) {
	const ddl = `
CREATE TABLE IF NOT EXISTS admin_api_keys (
  key_id     TEXT PRIMARY KEY,
  org_id     TEXT NOT NULL,
  user_id    TEXT NOT NULL,
  key_hash   TEXT NOT NULL UNIQUE,
  scopes     TEXT[] NOT NULL DEFAULT '{"admin"}',
  active     BOOL NOT NULL DEFAULT TRUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`
	if _, err := pool.Exec(ctx, ddl); err != nil {
		return nil, err
	}
	return &Store{pool: pool}, nil
}

// Record is a row from admin_api_keys.
type Record struct {
	KeyID     string
	OrgID     string
	UserID    string
	Scopes    []string
	CreatedAt time.Time
}

// Issue generates a new admin API key, stores its hash, and returns the plaintext token.
// The plaintext is returned exactly once; it is never stored.
func (s *Store) Issue(ctx context.Context, orgID, userID string, scopes []string) (plaintext string, rec Record, err error) {
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", Record{}, err
	}
	plaintext = tokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	hash := hashToken(plaintext)
	keyID := uuid.Must(uuid.NewV7()).String()
	now := time.Now().UTC()

	const q = `
INSERT INTO admin_api_keys (key_id, org_id, user_id, key_hash, scopes, created_at)
VALUES ($1, $2, $3, $4, $5, $6)`
	if _, err = s.pool.Exec(ctx, q, keyID, orgID, userID, hash, scopes, now); err != nil {
		return "", Record{}, err
	}
	return plaintext, Record{KeyID: keyID, OrgID: orgID, UserID: userID, Scopes: scopes, CreatedAt: now}, nil
}

// Validate looks up a plaintext token and returns its record, or an error if
// it is not found / inactive.
func (s *Store) Validate(ctx context.Context, plaintext string) (Record, error) {
	hash := hashToken(plaintext)
	const q = `
SELECT key_id, org_id, user_id, scopes, created_at
FROM admin_api_keys
WHERE key_hash = $1 AND active = TRUE`
	var rec Record
	err := s.pool.QueryRow(ctx, q, hash).
		Scan(&rec.KeyID, &rec.OrgID, &rec.UserID, &rec.Scopes, &rec.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid or inactive API key"))
	}
	if err != nil {
		return Record{}, connect.NewError(connect.CodeInternal, err)
	}
	return rec, nil
}

// HasOrgs returns true when at least one org exists (used for bootstrap guard).
func HasOrgs(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var n int
	err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM orgs LIMIT 1`).Scan(&n)
	return n > 0, err
}

// contextKey is unexported to avoid collisions.
type contextKey struct{}

// WithRecord attaches a validated Record to ctx.
func WithRecord(ctx context.Context, rec Record) context.Context {
	return context.WithValue(ctx, contextKey{}, rec)
}

// RecordFromContext retrieves the validated Record, panicking if absent.
func RecordFromContext(ctx context.Context) Record {
	return ctx.Value(contextKey{}).(Record)
}

// Interceptor returns a Connect interceptor that validates admin API keys.
// Every request must carry Authorization: Bearer <osk_...>.
func Interceptor(store *Store) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			raw := req.Header().Get("Authorization")
			token, ok := strings.CutPrefix(raw, "Bearer ")
			if !ok || !strings.HasPrefix(token, tokenPrefix) {
				return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("missing or malformed Authorization header"))
			}
			rec, err := store.Validate(ctx, token)
			if err != nil {
				return nil, err
			}
			return next(WithRecord(ctx, rec), req)
		}
	}
}

func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}
