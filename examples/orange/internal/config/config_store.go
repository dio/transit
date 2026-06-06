package config

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ── Sentinel errors ───────────────────────────────────────────────────────────

// ErrSnapshotNotFound is returned by SnapshotStore.FetchVersion when the
// requested version does not exist or was stored with compiled_ok = false.
var ErrSnapshotNotFound = errors.New("snapshot: version not found")

// ── SnapshotStore interface ───────────────────────────────────────────────────

// SnapshotStore abstracts the persistent backing store for compiled
// SnapshotEnvelope blobs. Implementations must be safe for concurrent use.
//
// The store layer is intentionally thin: it moves opaque bytes between the
// control plane and proxies without interpreting envelope contents. Envelope
// decoding, checksum verification, and compile() all happen inside
// AppState.ApplySnapshotEnvelope.
//
// The Postgres implementation (config_store_postgres.go) maps these methods to
// the config_snapshots table described in §19 of the design doc.
type SnapshotStore interface {
	// FetchLatest returns the current compiled envelope — the highest version
	// with compiled_ok = true. Returns (nil, nil) when sinceVersion is already
	// the latest, so the caller can skip unnecessary ApplySnapshotEnvelope
	// calls without parsing the payload.
	FetchLatest(ctx context.Context, sinceVersion uint64) (*SnapshotEnvelope, error)

	// FetchVersion returns the compiled envelope for a specific version.
	// Returns ErrSnapshotNotFound if the version does not exist or was stored
	// with a non-nil compileErr (compiled_ok = false).
	// Intended for operator rollback; the caller applies the result via
	// AppState.ApplySnapshotEnvelope.
	FetchVersion(ctx context.Context, version uint64) (*SnapshotEnvelope, error)

	// Store writes a new SnapshotEnvelope to the backing store. compiledBy
	// identifies the service account or operator that triggered the compile.
	// compileErr, if non-nil, marks the row as compiled_ok = false and stores
	// the error string for audit; such rows are excluded from serving queries.
	// Store is idempotent: writing the same version twice is a no-op.
	Store(ctx context.Context, env *SnapshotEnvelope, compiledBy string, compileErr error) error
}

// ── MemSnapshotStore ──────────────────────────────────────────────────────────

// storedEntry is one row in a MemSnapshotStore.
type storedEntry struct {
	env        SnapshotEnvelope
	compiledBy string
	compileErr error // nil ↔ compiled_ok = true
}

// MemSnapshotStore is a thread-safe, in-memory SnapshotStore implementation.
// It is intended for use in tests and local development. It does not persist
// state across process restarts.
//
// MemSnapshotStore satisfies the same idempotency contract as the Postgres
// implementation: calling Store with a version that already exists is silently
// ignored (matching "on conflict do nothing").
//
// Failed-compile entries are stored but excluded from FetchLatest and
// FetchVersion results, mirroring the Postgres "compiled_ok = true" filter.
type MemSnapshotStore struct {
	mu      sync.RWMutex
	entries map[uint64]*storedEntry // keyed by version
}

// NewMemSnapshotStore returns an empty MemSnapshotStore.
func NewMemSnapshotStore() *MemSnapshotStore {
	return &MemSnapshotStore{entries: make(map[uint64]*storedEntry)}
}

// FetchLatest returns the highest-version compiled entry whose version is
// strictly greater than sinceVersion. Returns (nil, nil) when no such entry
// exists (the caller is already up to date, or the store is empty).
func (s *MemSnapshotStore) FetchLatest(ctx context.Context, sinceVersion uint64) (*SnapshotEnvelope, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	var best *storedEntry
	for _, e := range s.entries {
		if e.compileErr != nil {
			continue // skip failed-compile rows
		}
		if e.env.Version <= sinceVersion {
			continue // caller already has this version
		}
		if best == nil || e.env.Version > best.env.Version {
			best = e
		}
	}
	if best == nil {
		return nil, nil
	}
	env := best.env // copy; caller must not mutate
	return &env, nil
}

// FetchVersion returns the compiled entry for the requested version.
// Returns ErrSnapshotNotFound if the version is absent or has a non-nil
// compileErr (compiled_ok = false).
func (s *MemSnapshotStore) FetchVersion(ctx context.Context, version uint64) (*SnapshotEnvelope, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	e, ok := s.entries[version]
	if !ok || e.compileErr != nil {
		return nil, fmt.Errorf("%w: %d", ErrSnapshotNotFound, version)
	}
	env := e.env
	return &env, nil
}

// Store adds env to the store. If an entry for env.Version already exists, the
// call is a no-op (idempotent). compiledBy is stored for audit purposes.
// compileErr marks the entry as compiled_ok = false when non-nil.
func (s *MemSnapshotStore) Store(ctx context.Context, env *SnapshotEnvelope, compiledBy string, compileErr error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if env == nil {
		return errors.New("snapshot store: nil envelope")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.entries[env.Version]; exists {
		return nil // idempotent: same version already stored
	}
	cp := *env // copy; store owns the bytes
	s.entries[env.Version] = &storedEntry{
		env:        cp,
		compiledBy: compiledBy,
		compileErr: compileErr,
	}
	return nil
}

// Len returns the total number of entries (compiled and failed) in the store.
// Useful for test assertions.
func (s *MemSnapshotStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}
