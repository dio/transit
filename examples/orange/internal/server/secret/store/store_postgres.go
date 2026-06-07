package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGSecretStore implements SecretStore over PostgreSQL via pgx.
type PGSecretStore struct {
	pool *pgxpool.Pool
	mu   sync.RWMutex
}

// NewPGSecretStore returns a new PGSecretStore backed by pool.
// DDL is executed on construction to initialize tables and indices.
func NewPGSecretStore(ctx context.Context, pool *pgxpool.Pool) (*PGSecretStore, error) {
	ddl := `
CREATE TABLE IF NOT EXISTS secret_keys (
	id               TEXT    NOT NULL,
	version          INTEGER NOT NULL,
	purpose          TEXT    NOT NULL,
	realm            TEXT    NOT NULL DEFAULT '',
	state            TEXT    NOT NULL,
	parent_id        TEXT    NOT NULL DEFAULT '',
	parent_version   INTEGER NOT NULL DEFAULT 0,
	wrapped_material TEXT    NOT NULL DEFAULT '',
	created_at       TIMESTAMPTZ NOT NULL,
	PRIMARY KEY (id, version)
);
CREATE INDEX IF NOT EXISTS idx_secret_keys_prefix_state ON secret_keys (id, state, version DESC);

CREATE TABLE IF NOT EXISTS secret_versions (
	realm        TEXT    NOT NULL,
	name         TEXT    NOT NULL,
	version_id   TEXT    NOT NULL,
	dek_id       TEXT    NOT NULL,
	dek_version  INTEGER NOT NULL,
	ciphertext   TEXT    NOT NULL DEFAULT '',
	checksum     TEXT    NOT NULL DEFAULT '',
	state        INTEGER NOT NULL DEFAULT 0,
	created_at   TIMESTAMPTZ NOT NULL,
	created_by   TEXT    NOT NULL DEFAULT '',
	enabled_at   TIMESTAMPTZ,
	enabled_by   TEXT    NOT NULL DEFAULT '',
	disabled_at  TIMESTAMPTZ,
	disabled_by  TEXT    NOT NULL DEFAULT '',
	retired_at   TIMESTAMPTZ,
	retired_by   TEXT    NOT NULL DEFAULT '',
	shredded_at  TIMESTAMPTZ,
	PRIMARY KEY (realm, name, version_id)
);
CREATE INDEX IF NOT EXISTS idx_secret_versions_realm_name ON secret_versions (realm, name, version_id DESC);

CREATE SEQUENCE IF NOT EXISTS pool_seq_seq START 1;
`
	for _, stmt := range strings.Split(ddl, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return nil, fmt.Errorf("pg: init: %w", err)
		}
	}
	return &PGSecretStore{pool: pool}, nil
}

func (s *PGSecretStore) PutKey(ctx context.Context, key *Key) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	query := `
INSERT INTO secret_keys (id, version, purpose, realm, state, parent_id, parent_version, wrapped_material, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (id, version) DO NOTHING
`
	result, err := s.pool.Exec(ctx, query,
		key.ID, key.Version, key.Purpose, key.Realm, key.State,
		key.ParentID, key.ParentVersion, key.WrappedMaterial, key.CreatedAt)
	if err != nil {
		return fmt.Errorf("pg: put key %s@%d: %w", key.ID, key.Version, err)
	}
	if result.RowsAffected() == 0 {
		return fmt.Errorf("pg: %w", ErrKeyExists)
	}
	return nil
}

func (s *PGSecretStore) GetKey(ctx context.Context, id string, version int) (*Key, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	query := `
SELECT id, version, purpose, realm, state, parent_id, parent_version, wrapped_material, created_at
FROM secret_keys WHERE id = $1 AND version = $2
`
	row := s.pool.QueryRow(ctx, query, id, version)
	return s.scanKey(row)
}

func (s *PGSecretStore) ListKeyVersions(ctx context.Context, id string) ([]*Key, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	query := `
SELECT id, version, purpose, realm, state, parent_id, parent_version, wrapped_material, created_at
FROM secret_keys WHERE id = $1 ORDER BY version ASC
`
	rows, err := s.pool.Query(ctx, query, id)
	if err != nil {
		return nil, fmt.Errorf("pg: list key versions %s: %w", id, err)
	}
	defer rows.Close()

	var keys []*Key
	for rows.Next() {
		k, err := s.scanKeyFromRows(rows)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: list key versions rows: %w", err)
	}
	return keys, nil
}

func (s *PGSecretStore) UpdateKeyState(ctx context.Context, id string, version int, state KeyState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	result, err := s.pool.Exec(ctx, `UPDATE secret_keys SET state = $1 WHERE id = $2 AND version = $3`, state, id, version)
	if err != nil {
		return fmt.Errorf("pg: update key state %s@%d: %w", id, version, err)
	}
	if result.RowsAffected() == 0 {
		return fmt.Errorf("pg: key %s@%d not found", id, version)
	}
	return nil
}

func (s *PGSecretStore) AllocatePoolSeq(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	var nextVal int
	if err := s.pool.QueryRow(ctx, "SELECT nextval('pool_seq_seq')").Scan(&nextVal); err != nil {
		return 0, fmt.Errorf("pg: allocate pool seq: %w", err)
	}
	return nextVal, nil
}

func (s *PGSecretStore) ListKeysByPrefix(ctx context.Context, prefix string) ([]*Key, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	query := `
SELECT DISTINCT ON (id) id, version, purpose, realm, state, parent_id, parent_version, wrapped_material, created_at
FROM secret_keys
WHERE id LIKE $1 || '%' AND state = $2
ORDER BY id, version DESC
`
	rows, err := s.pool.Query(ctx, query, prefix, KeyStateActive)
	if err != nil {
		return nil, fmt.Errorf("pg: list keys by prefix %q: %w", prefix, err)
	}
	defer rows.Close()

	var keys []*Key
	for rows.Next() {
		k, err := s.scanKeyFromRows(rows)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: list keys by prefix rows: %w", err)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })
	return keys, nil
}

func (s *PGSecretStore) PutSecret(ctx context.Context, secret *Secret) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	query := `
INSERT INTO secret_versions
(realm, name, version_id, dek_id, dek_version, ciphertext, checksum, state,
 created_at, created_by, enabled_at, enabled_by, disabled_at, disabled_by, retired_at, retired_by, shredded_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
ON CONFLICT (realm, name, version_id) DO UPDATE
SET ciphertext = EXCLUDED.ciphertext, checksum = EXCLUDED.checksum, state = EXCLUDED.state,
	enabled_at = EXCLUDED.enabled_at, enabled_by = EXCLUDED.enabled_by,
	disabled_at = EXCLUDED.disabled_at, disabled_by = EXCLUDED.disabled_by,
	retired_at = EXCLUDED.retired_at, retired_by = EXCLUDED.retired_by,
	shredded_at = EXCLUDED.shredded_at
`
	_, err := s.pool.Exec(ctx, query,
		secret.Realm, secret.Name, secret.VersionID,
		secret.DEKID, secret.DEKVersion, secret.Ciphertext, secret.Checksum, secret.State,
		secret.CreatedAt, secret.CreatedBy,
		secret.EnabledAt, secret.EnabledBy,
		secret.DisabledAt, secret.DisabledBy,
		secret.RetiredAt, secret.RetiredBy,
		secret.ShreddedAt)
	if err != nil {
		return fmt.Errorf("pg: put secret %s/%s@%s: %w", secret.Realm, secret.Name, secret.VersionID, err)
	}
	return nil
}

func (s *PGSecretStore) GetLatestEnabledSecret(ctx context.Context, realm, name string) (*Secret, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	query := `
SELECT realm, name, version_id, dek_id, dek_version, ciphertext, checksum, state,
       created_at, created_by, enabled_at, enabled_by, disabled_at, disabled_by, retired_at, retired_by, shredded_at
FROM secret_versions
WHERE realm = $1 AND name = $2 AND state = $3
ORDER BY version_id DESC LIMIT 1
`
	row := s.pool.QueryRow(ctx, query, realm, name, VersionStateEnabled)
	return s.scanSecret(row)
}

func (s *PGSecretStore) GetSecretVersion(ctx context.Context, realm, name, versionID string) (*Secret, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	query := `
SELECT realm, name, version_id, dek_id, dek_version, ciphertext, checksum, state,
       created_at, created_by, enabled_at, enabled_by, disabled_at, disabled_by, retired_at, retired_by, shredded_at
FROM secret_versions
WHERE realm = $1 AND name = $2 AND version_id = $3
`
	row := s.pool.QueryRow(ctx, query, realm, name, versionID)
	return s.scanSecret(row)
}

func (s *PGSecretStore) ListSecretVersions(ctx context.Context, realm, name string) ([]*Secret, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	query := `
SELECT realm, name, version_id, dek_id, dek_version, ciphertext, checksum, state,
       created_at, created_by, enabled_at, enabled_by, disabled_at, disabled_by, retired_at, retired_by, shredded_at
FROM secret_versions
WHERE realm = $1 AND name = $2
ORDER BY version_id ASC
`
	rows, err := s.pool.Query(ctx, query, realm, name)
	if err != nil {
		return nil, fmt.Errorf("pg: list secret versions %s/%s: %w", realm, name, err)
	}
	defer rows.Close()

	var secrets []*Secret
	for rows.Next() {
		sv, err := s.scanSecretFromRows(rows)
		if err != nil {
			return nil, err
		}
		secrets = append(secrets, sv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: list secret versions rows: %w", err)
	}
	return secrets, nil
}

func (s *PGSecretStore) ListSecrets(ctx context.Context, realm string) ([]SecretID, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// realm is used as a prefix filter: empty = all; non-empty = realm LIKE '<realm>%'
	// This lets callers pass "org/<uuid>/" to list all secrets under an org.
	query := `
SELECT DISTINCT realm, name
FROM secret_versions
WHERE $1 = '' OR realm LIKE $2
ORDER BY realm, name
`
	rows, err := s.pool.Query(ctx, query, realm, realm+"%")
	if err != nil {
		return nil, fmt.Errorf("pg: list secrets: %w", err)
	}
	defer rows.Close()

	var secrets []SecretID
	for rows.Next() {
		var id SecretID
		if err := rows.Scan(&id.Realm, &id.Name); err != nil {
			return nil, fmt.Errorf("pg: scan secret id: %w", err)
		}
		secrets = append(secrets, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: list secrets rows: %w", err)
	}
	return secrets, nil
}

func (s *PGSecretStore) scanKey(row pgx.Row) (*Key, error) {
	var k Key
	if err := row.Scan(&k.ID, &k.Version, &k.Purpose, &k.Realm, &k.State,
		&k.ParentID, &k.ParentVersion, &k.WrappedMaterial, &k.CreatedAt); err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("pg: key not found")
		}
		return nil, fmt.Errorf("pg: scan key: %w", err)
	}
	return &k, nil
}

func (s *PGSecretStore) scanKeyFromRows(rows pgx.Rows) (*Key, error) {
	var k Key
	if err := rows.Scan(&k.ID, &k.Version, &k.Purpose, &k.Realm, &k.State,
		&k.ParentID, &k.ParentVersion, &k.WrappedMaterial, &k.CreatedAt); err != nil {
		return nil, fmt.Errorf("pg: scan key from rows: %w", err)
	}
	return &k, nil
}

func (s *PGSecretStore) scanSecret(row pgx.Row) (*Secret, error) {
	var sv Secret
	if err := row.Scan(&sv.Realm, &sv.Name, &sv.VersionID,
		&sv.DEKID, &sv.DEKVersion, &sv.Ciphertext, &sv.Checksum, &sv.State,
		&sv.CreatedAt, &sv.CreatedBy,
		&sv.EnabledAt, &sv.EnabledBy,
		&sv.DisabledAt, &sv.DisabledBy,
		&sv.RetiredAt, &sv.RetiredBy,
		&sv.ShreddedAt); err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("pg: secret not found")
		}
		return nil, fmt.Errorf("pg: scan secret: %w", err)
	}
	return &sv, nil
}

func (s *PGSecretStore) scanSecretFromRows(rows pgx.Rows) (*Secret, error) {
	var sv Secret
	if err := rows.Scan(&sv.Realm, &sv.Name, &sv.VersionID,
		&sv.DEKID, &sv.DEKVersion, &sv.Ciphertext, &sv.Checksum, &sv.State,
		&sv.CreatedAt, &sv.CreatedBy,
		&sv.EnabledAt, &sv.EnabledBy,
		&sv.DisabledAt, &sv.DisabledBy,
		&sv.RetiredAt, &sv.RetiredBy,
		&sv.ShreddedAt); err != nil {
		return nil, fmt.Errorf("pg: scan secret from rows: %w", err)
	}
	return &sv, nil
}
