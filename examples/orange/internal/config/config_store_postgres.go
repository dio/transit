package config

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ── PgSnapshotStore ───────────────────────────────────────────────────────────

// PgSnapshotStore is a Postgres-backed SnapshotStore backed by pgx v5.
// It stores compiled SnapshotEnvelopes in a single table keyed by
// (workspace_id, version) and supports FetchLatest, FetchVersion, List,
// NextVersion, and idempotent Store.
//
// Production usage: NewPgSnapshotStore(ctx, pool) creates the table and index
// using CREATE TABLE / INDEX IF NOT EXISTS, so it is safe to call on startup.
//
// Test isolation: pass WithTable("config_snapshots_N") so each test gets its
// own table within the shared embedded postgres; see config_store_postgres_test.go.
type PgSnapshotStore struct {
	pool  *pgxpool.Pool
	table string // double-quoted, safe for embedding in SQL
}

// StoreOption configures a PgSnapshotStore.
type StoreOption func(*PgSnapshotStore)

// validTableName rejects names that would need quoting beyond what we apply.
var validTableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// WithTable overrides the default table name "config_snapshots". The name must
// match [A-Za-z_][A-Za-z0-9_]* (letters, digits, underscores, no spaces).
// Intended for test isolation: each test creates its own table so parallel test
// binaries never conflict on a shared embedded postgres.
func WithTable(name string) StoreOption {
	return func(s *PgSnapshotStore) { s.table = name }
}

// NewPgSnapshotStore creates a PgSnapshotStore that writes to the
// "config_snapshots" table (override with WithTable). It calls Migrate
// immediately so the table and serving index exist before any method is called.
func NewPgSnapshotStore(ctx context.Context, pool *pgxpool.Pool, opts ...StoreOption) (*PgSnapshotStore, error) {
	s := &PgSnapshotStore{pool: pool, table: "config_snapshots"}
	for _, o := range opts {
		o(s)
	}
	if !validTableName.MatchString(s.table) {
		return nil, fmt.Errorf("store: invalid table name %q (must match [A-Za-z_][A-Za-z0-9_]*)", s.table)
	}
	if err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Migrate creates the config_snapshots table and its serving index if they do
// not already exist. Safe to call multiple times (idempotent).
func (s *PgSnapshotStore) Migrate(ctx context.Context) error {
	t := pgx.Identifier{s.table}.Sanitize()
	idx := pgx.Identifier{s.table + "_serving_idx"}.Sanitize()

	ddl := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (
    workspace_id  TEXT        NOT NULL,
    version       BIGINT      NOT NULL,
    format        TEXT        NOT NULL,
    compression   TEXT        NOT NULL,
    payload       BYTEA       NOT NULL,
    checksum      BYTEA,
    compiled_ok   BOOLEAN     NOT NULL,
    compile_error TEXT,
    byte_size     INTEGER     NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by    TEXT        NOT NULL,
    PRIMARY KEY (workspace_id, version),
    CHECK (version > 0),
    CHECK (format IN ('proto','yaml','json','msgpack')),
    CHECK (compression IN ('zstd','none')),
    CHECK (compiled_ok OR compile_error IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS %s ON %s (workspace_id, version DESC) WHERE compiled_ok = true;`,
		t, idx, t)

	_, err := s.pool.Exec(ctx, ddl)
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return nil
}

// FetchLatest returns the highest-version compiled envelope for workspaceID
// whose version is strictly greater than sinceVersion. Returns (nil, nil) when
// the caller is already up to date or no compiled snapshots exist yet.
func (s *PgSnapshotStore) FetchLatest(ctx context.Context, workspaceID string, sinceVersion uint64) (*SnapshotEnvelope, error) {
	t := pgx.Identifier{s.table}.Sanitize()
	q := fmt.Sprintf(`
SELECT version, format, compression, payload, checksum
FROM %s
WHERE workspace_id = $1 AND compiled_ok = true AND version > $2
ORDER BY version DESC
LIMIT 1`, t)

	row := s.pool.QueryRow(ctx, q, workspaceID, int64(sinceVersion))
	env, err := scanEnvelope(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return env, err
}

// FetchVersion returns the compiled envelope for the exact (workspaceID, version)
// pair. Returns ErrSnapshotNotFound if the version does not exist or has
// compiled_ok = false.
func (s *PgSnapshotStore) FetchVersion(ctx context.Context, workspaceID string, version uint64) (*SnapshotEnvelope, error) {
	t := pgx.Identifier{s.table}.Sanitize()
	q := fmt.Sprintf(`
SELECT version, format, compression, payload, checksum
FROM %s
WHERE workspace_id = $1 AND version = $2 AND compiled_ok = true`, t)

	row := s.pool.QueryRow(ctx, q, workspaceID, int64(version))
	env, err := scanEnvelope(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %d", ErrSnapshotNotFound, version)
	}
	return env, err
}

// Store persists env to the backing table under workspaceID. compiledBy
// identifies the caller for audit purposes. A non-nil compileErr marks the row
// compiled_ok = false (stored for audit; never returned by Fetch queries).
// Duplicate (workspaceID, version) writes are silently ignored (ON CONFLICT DO NOTHING).
// Returns an error for nil envelopes or version == 0.
func (s *PgSnapshotStore) Store(ctx context.Context, env *SnapshotEnvelope, workspaceID string, compiledBy string, compileErr error) error {
	if env == nil {
		return errors.New("store: nil envelope")
	}
	if env.Version == 0 {
		return errors.New("store: version 0 cannot be persisted; use version > 0")
	}

	t := pgx.Identifier{s.table}.Sanitize()
	q := fmt.Sprintf(`
INSERT INTO %s (workspace_id, version, format, compression, payload, checksum,
                compiled_ok, compile_error, byte_size, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (workspace_id, version) DO NOTHING`, t)

	compiledOK := compileErr == nil
	var compileErrStr *string
	if compileErr != nil {
		s := compileErr.Error()
		compileErrStr = &s
	}
	byteSize := len(env.Payload)
	var checksum *[]byte
	if env.Checksum != nil {
		checksum = &env.Checksum
	}

	_, err := s.pool.Exec(ctx, q,
		workspaceID,
		int64(env.Version),
		string(env.Format),
		string(env.Compression),
		env.Payload,
		checksum,
		compiledOK,
		compileErrStr,
		byteSize,
		compiledBy,
	)
	if err != nil {
		return fmt.Errorf("store: insert version %d: %w", env.Version, err)
	}
	return nil
}

// List returns up to limit snapshot metadata entries for workspaceID in
// descending version order. afterVersion, if > 0, restricts to versions
// strictly less than afterVersion (cursor-based pagination).
func (s *PgSnapshotStore) List(ctx context.Context, workspaceID string, limit int, afterVersion uint64) ([]*SnapshotListEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	t := pgx.Identifier{s.table}.Sanitize()

	var (
		q    string
		args []any
	)
	if afterVersion > 0 {
		q = fmt.Sprintf(`
SELECT version, format, compression, payload, checksum,
       compiled_ok, COALESCE(compile_error,''), created_at, created_by
FROM %s
WHERE workspace_id = $1 AND version < $2
ORDER BY version DESC
LIMIT $3`, t)
		args = []any{workspaceID, int64(afterVersion), limit}
	} else {
		q = fmt.Sprintf(`
SELECT version, format, compression, payload, checksum,
       compiled_ok, COALESCE(compile_error,''), created_at, created_by
FROM %s
WHERE workspace_id = $1
ORDER BY version DESC
LIMIT $2`, t)
		args = []any{workspaceID, limit}
	}

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list: %w", err)
	}
	defer rows.Close()

	var out []*SnapshotListEntry
	for rows.Next() {
		var (
			version     int64
			format      string
			compression string
			payload     []byte
			checksum    []byte
			compiledOK  bool
			compileErr  string
			createdAt   time.Time
			createdBy   string
		)
		if err := rows.Scan(&version, &format, &compression, &payload, &checksum,
			&compiledOK, &compileErr, &createdAt, &createdBy); err != nil {
			return nil, fmt.Errorf("store: list scan: %w", err)
		}
		out = append(out, &SnapshotListEntry{
			Envelope: SnapshotEnvelope{
				Version:     uint64(version),
				Format:      SnapshotFormat(format),
				Compression: CompressionKind(compression),
				Payload:     payload,
				Checksum:    checksum,
			},
			CompiledOK: compiledOK,
			CompileErr: compileErr,
			CreatedAt:  createdAt,
			CreatedBy:  createdBy,
		})
	}
	return out, rows.Err()
}

// NextVersion returns COALESCE(MAX(version), 0) + 1 for the workspace.
func (s *PgSnapshotStore) NextVersion(ctx context.Context, workspaceID string) (uint64, error) {
	t := pgx.Identifier{s.table}.Sanitize()
	q := fmt.Sprintf(`SELECT COALESCE(MAX(version), 0) + 1 FROM %s WHERE workspace_id = $1`, t)
	var next int64
	if err := s.pool.QueryRow(ctx, q, workspaceID).Scan(&next); err != nil {
		return 0, fmt.Errorf("store: next version: %w", err)
	}
	return uint64(next), nil
}

// scanEnvelope reads one row from a QueryRow result into a SnapshotEnvelope.
func scanEnvelope(row pgx.Row) (*SnapshotEnvelope, error) {
	var (
		version     int64
		format      string
		compression string
		payload     []byte
		checksum    []byte // NULL → nil
	)
	if err := row.Scan(&version, &format, &compression, &payload, &checksum); err != nil {
		return nil, err
	}
	return &SnapshotEnvelope{
		Version:     uint64(version),
		Format:      SnapshotFormat(format),
		Compression: CompressionKind(compression),
		Payload:     payload,
		Checksum:    checksum,
	}, nil
}
