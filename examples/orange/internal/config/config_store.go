package config

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ── Sentinel errors ───────────────────────────────────────────────────────────

// ErrSnapshotNotFound is returned by SnapshotStore.FetchVersion when the
// requested version does not exist or was stored with compiled_ok = false.
var ErrSnapshotNotFound = errors.New("snapshot: version not found")

// ── SnapshotListEntry ─────────────────────────────────────────────────────────

// SnapshotListEntry carries metadata for one row returned by SnapshotStore.List.
// Payload is intentionally absent to keep list responses lightweight.
type SnapshotListEntry struct {
	Envelope   SnapshotEnvelope
	CompiledOK bool
	CompileErr string
	CreatedAt  time.Time
	CreatedBy  string
}

// ── SnapshotStore interface ───────────────────────────────────────────────────

// SnapshotStore abstracts the persistent backing store for compiled
// SnapshotEnvelope blobs. All methods are scoped to a workspaceID so a single
// store instance serves multiple workspaces without cross-contamination.
// Implementations must be safe for concurrent use.
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
	// with compiled_ok = true for the given workspace. Returns (nil, nil) when
	// sinceVersion is already the latest, so the caller can skip unnecessary
	// ApplySnapshotEnvelope calls without parsing the payload.
	FetchLatest(ctx context.Context, workspaceID string, sinceVersion uint64) (*SnapshotEnvelope, error)

	// FetchVersion returns the compiled envelope for a specific version within
	// the workspace. Returns ErrSnapshotNotFound if the version does not exist
	// or was stored with a non-nil compileErr (compiled_ok = false).
	FetchVersion(ctx context.Context, workspaceID string, version uint64) (*SnapshotEnvelope, error)

	// Store writes a new SnapshotEnvelope to the backing store for the given
	// workspace. compiledBy identifies the service account or operator that
	// triggered the compile. compileErr, if non-nil, marks the row as
	// compiled_ok = false and stores the error string for audit; such rows are
	// excluded from serving queries. Store is idempotent: writing the same
	// (workspaceID, version) pair twice is a no-op.
	Store(ctx context.Context, env *SnapshotEnvelope, workspaceID string, compiledBy string, compileErr error) error

	// List returns up to limit snapshot metadata entries for workspaceID in
	// descending version order. afterVersion, if > 0, restricts results to
	// versions strictly less than afterVersion (cursor-based pagination).
	List(ctx context.Context, workspaceID string, limit int, afterVersion uint64) ([]*SnapshotListEntry, error)

	// NextVersion returns the next version number to use for a new snapshot in
	// the given workspace (current max + 1, or 1 if no snapshots exist yet).
	NextVersion(ctx context.Context, workspaceID string) (uint64, error)
}

// ── MemSnapshotStore ──────────────────────────────────────────────────────────

// storedEntry is one row in a MemSnapshotStore.
type storedEntry struct {
	env        SnapshotEnvelope
	compiledBy string
	compiledAt time.Time
	compileErr error // nil ↔ compiled_ok = true
}

// MemSnapshotStore is a thread-safe, in-memory SnapshotStore implementation.
// It is intended for use in tests and local development. It does not persist
// state across process restarts.
//
// MemSnapshotStore satisfies the same idempotency contract as the Postgres
// implementation: calling Store with a (workspaceID, version) pair that already
// exists is silently ignored (matching "on conflict do nothing").
//
// Failed-compile entries are stored but excluded from FetchLatest and
// FetchVersion results, mirroring the Postgres "compiled_ok = true" filter.
type MemSnapshotStore struct {
	mu      sync.RWMutex
	entries map[string]map[uint64]*storedEntry // [workspaceID][version]
}

// NewMemSnapshotStore returns an empty MemSnapshotStore.
func NewMemSnapshotStore() *MemSnapshotStore {
	return &MemSnapshotStore{entries: make(map[string]map[uint64]*storedEntry)}
}

// FetchLatest returns the highest-version compiled entry for workspaceID whose
// version is strictly greater than sinceVersion. Returns (nil, nil) when no
// such entry exists (the caller is already up to date, or the store is empty).
func (s *MemSnapshotStore) FetchLatest(ctx context.Context, workspaceID string, sinceVersion uint64) (*SnapshotEnvelope, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	var best *storedEntry
	for _, e := range s.entries[workspaceID] {
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

// FetchVersion returns the compiled entry for the requested version within
// workspaceID. Returns ErrSnapshotNotFound if the version is absent or has a
// non-nil compileErr (compiled_ok = false).
func (s *MemSnapshotStore) FetchVersion(ctx context.Context, workspaceID string, version uint64) (*SnapshotEnvelope, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	e, ok := s.entries[workspaceID][version]
	if !ok || e.compileErr != nil {
		return nil, fmt.Errorf("%w: %d", ErrSnapshotNotFound, version)
	}
	env := e.env
	return &env, nil
}

// Store adds env to the store under workspaceID. If an entry for
// (workspaceID, env.Version) already exists, the call is a no-op (idempotent).
// compiledBy is stored for audit purposes. compileErr marks the entry as
// compiled_ok = false when non-nil.
func (s *MemSnapshotStore) Store(ctx context.Context, env *SnapshotEnvelope, workspaceID string, compiledBy string, compileErr error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if env == nil {
		return errors.New("snapshot store: nil envelope")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	ws := s.entries[workspaceID]
	if ws == nil {
		ws = make(map[uint64]*storedEntry)
		s.entries[workspaceID] = ws
	}
	if _, exists := ws[env.Version]; exists {
		return nil // idempotent: same (workspace, version) already stored
	}
	cp := *env // copy; store owns the bytes
	ws[env.Version] = &storedEntry{
		env:        cp,
		compiledBy: compiledBy,
		compiledAt: time.Now().UTC(),
		compileErr: compileErr,
	}
	return nil
}

// List returns up to limit entries for workspaceID in descending version order.
// afterVersion, if > 0, restricts to versions strictly less than afterVersion.
func (s *MemSnapshotStore) List(ctx context.Context, workspaceID string, limit int, afterVersion uint64) ([]*SnapshotListEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Collect all entries that match the cursor filter.
	var out []*SnapshotListEntry
	for _, e := range s.entries[workspaceID] {
		if afterVersion > 0 && e.env.Version >= afterVersion {
			continue
		}
		ce := ""
		if e.compileErr != nil {
			ce = e.compileErr.Error()
		}
		env := e.env
		out = append(out, &SnapshotListEntry{
			Envelope:   env,
			CompiledOK: e.compileErr == nil,
			CompileErr: ce,
			CreatedAt:  e.compiledAt,
			CreatedBy:  e.compiledBy,
		})
	}

	// Sort descending by version.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Envelope.Version > out[j-1].Envelope.Version; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// NextVersion returns max(version) + 1 for workspaceID, or 1 if none exist.
func (s *MemSnapshotStore) NextVersion(ctx context.Context, workspaceID string) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	var max uint64
	for v := range s.entries[workspaceID] {
		if v > max {
			max = v
		}
	}
	return max + 1, nil
}

// Len returns the total number of entries (compiled and failed) across all
// workspaces in the store. Useful for test assertions.
func (s *MemSnapshotStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	total := 0
	for _, ws := range s.entries {
		total += len(ws)
	}
	return total
}
