package egressauth

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("not found")

// Store implements KeyLookup for egress assertion validation.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore creates a new Store that fetches egress public keys from the database.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// ActivePublicKeyPEM returns the active Ed25519 public key PEM for an egress.
// It joins egress_keypairs with egresses to ensure we return the currently
// active keypair (referenced by egresses.keypair_id).
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
