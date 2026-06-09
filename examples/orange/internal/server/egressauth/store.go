package egressauth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("not found")

// Store implements KeyLookup and WorkspaceAncestryLookup for egress validation.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore creates a new Store backed by pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// ActivePublicKeyPEM returns the active Ed25519 public key PEM for an egress.
func (s *Store) ActivePublicKeyPEM(ctx context.Context, egressID string) (string, error) {
	const q = `
SELECT ek.public_key_pem
FROM egress_keypairs ek
JOIN egresses e ON e.egress_id = ek.egress_id AND e.keypair_id = ek.keypair_id
WHERE ek.egress_id = $1 AND ek.active = true
LIMIT 1
`
	var pubKeyPEM string
	err := s.pool.QueryRow(ctx, q, egressID).Scan(&pubKeyPEM)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	return pubKeyPEM, nil
}

// WorkspaceAncestry resolves the project and org that own a workspace in a single
// round-trip (workspaces ⋈ projects). Returns ErrNotFound when the workspace
// does not exist.
func (s *Store) WorkspaceAncestry(ctx context.Context, workspaceID string) (projID, orgID string, err error) {
	const q = `
SELECT w.project_id, p.org_id
FROM workspaces w
JOIN projects p ON p.project_id = w.project_id
WHERE w.workspace_id = $1
LIMIT 1
`
	err = s.pool.QueryRow(ctx, q, workspaceID).Scan(&projID, &orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", fmt.Errorf("workspace %q: %w", workspaceID, ErrNotFound)
	}
	return projID, orgID, err
}

// WorkspaceName returns the human-readable name of a workspace by its UUID.
// The lookup is a primary-key scan (workspace_id TEXT PRIMARY KEY) so it is O(1).
// Returns ErrNotFound when the workspace does not exist.
func (s *Store) WorkspaceName(ctx context.Context, workspaceID string) (string, error) {
	const q = `SELECT name FROM workspaces WHERE workspace_id = $1 LIMIT 1`
	var name string
	err := s.pool.QueryRow(ctx, q, workspaceID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("workspace %q: %w", workspaceID, ErrNotFound)
	}
	return name, err
}
