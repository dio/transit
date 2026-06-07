// Package store defines the SecretStore interface and the PostgreSQL backend
// (PGSecretStore) that implements it.
package store

import (
	"context"
	"errors"
	"time"
)

// ErrKeyExists is returned by PutKey when a record with the same (id, version)
// already exists.
var ErrKeyExists = errors.New("store: key version already exists")

// KeyPurpose classifies a Key record within the three-tier hierarchy.
type KeyPurpose string

const (
	PurposeMasterKEK  KeyPurpose = "master-kek"
	PurposeServiceKEK KeyPurpose = "service-kek"
	PurposeDEK        KeyPurpose = "dek"
)

// KeyState is the lifecycle state of a cryptographic key.
type KeyState string

const (
	KeyStatePending          KeyState = "pending"
	KeyStateActive           KeyState = "active"
	KeyStateDisabled         KeyState = "disabled"
	KeyStateDestroyScheduled KeyState = "destroy_scheduled"
	KeyStateDestroyed        KeyState = "destroyed"
)

// Key is a unified record for MASTER_KEK, SERVICE_KEK, and DEK entries.
// Keys are never overwritten; every mutation produces a new integer version.
type Key struct {
	ID      string
	Version int
	Purpose KeyPurpose
	Realm   string

	State KeyState

	ParentID      string
	ParentVersion int

	// WrappedMaterial is base64url(version_byte|hkdf_salt|nonce|xchacha_ct).
	WrappedMaterial string

	CreatedAt time.Time
}

// VersionState is the lifecycle state of a single secret version.
type VersionState int

const (
	VersionStateUnspecified VersionState = iota
	VersionStateEnabled
	VersionStateDisabled
	VersionStateRetired
)

// Secret is the at-rest record for one encrypted version of a secret.
type Secret struct {
	Realm string // canonical: "org/<uuid>/<purpose>", "proj/<uuid>/<purpose>", or "ws/<uuid>/<purpose>"
	Name  string
	VersionID   string // UUID7

	DEKID      string
	DEKVersion int

	// Ciphertext is base64url(version_byte[1] | hkdf_salt[32] | nonce[24] | xchacha_ct[N+16]).
	Ciphertext string
	Checksum   string // hex SHA-256 of plaintext material

	State VersionState

	CreatedAt  time.Time
	CreatedBy  string
	EnabledAt  *time.Time
	EnabledBy  string
	DisabledAt *time.Time
	DisabledBy string
	RetiredAt  *time.Time
	RetiredBy  string
	ShreddedAt *time.Time
}

// SecretID is a lightweight identity tuple used by ListSecrets.
type SecretID struct {
	Realm string
	Name  string
}

// SecretStore is the persistence interface for the secret management plane.
type SecretStore interface {
	// PutKey inserts a new key record. Returns ErrKeyExists if the (id, version)
	// pair already exists.
	PutKey(ctx context.Context, key *Key) error
	GetKey(ctx context.Context, id string, version int) (*Key, error)
	ListKeyVersions(ctx context.Context, id string) ([]*Key, error)
	UpdateKeyState(ctx context.Context, id string, version int, state KeyState) error

	// AllocatePoolSeq atomically reserves the next pool-member sequence number.
	AllocatePoolSeq(ctx context.Context) (int, error)

	// ListKeysByPrefix returns the latest active Key version for every key whose
	// ID starts with prefix.
	ListKeysByPrefix(ctx context.Context, prefix string) ([]*Key, error)

	PutSecret(ctx context.Context, secret *Secret) error
	GetLatestEnabledSecret(ctx context.Context, realm, name string) (*Secret, error)
	GetSecretVersion(ctx context.Context, realm, name, versionID string) (*Secret, error)
	ListSecretVersions(ctx context.Context, realm, name string) ([]*Secret, error)
	ListSecrets(ctx context.Context, realm string) ([]SecretID, error)
}
