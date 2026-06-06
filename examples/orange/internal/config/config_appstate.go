package config

import (
	"errors"
	"fmt"
	"sync/atomic"
)

// ── ResolvedRequest ───────────────────────────────────────────────────────────

// ResolvedRequest is the fully resolved context for one data-plane request. It
// is produced by AppState.Resolve and holds everything the request handler needs
// to make routing and policy decisions without further config lookups.
type ResolvedRequest struct {
	Key     ResolvedKey
	Profile ResolvedProfile
}

// ── AppState ──────────────────────────────────────────────────────────────────

// AppState is the boundary between dynamic control-plane changes and lock-free
// request handling. Writers build complete snapshots off to the side and publish
// them through AppState; readers only load the current snapshot and never observe
// partially compiled state.
//
// One atomic pointer covers the full ConfigSnapshot. This prevents a reader from
// ever seeing a new GlobalConfig paired with old pools, or vice versa.
//
// Reload sequence:
//
//  1. Decode and compile the new snapshot on any goroutine.
//  2. If compile fails, return an error and leave the old snapshot live.
//  3. Seed user tables from the new snapshot (atomically replaces L2, purges L1).
//  4. Atomically store the new *ConfigSnapshot.
//  5. Inflight requests finish with whichever snapshot they loaded at step 4.
//
// AppState is safe for concurrent use.
type AppState struct {
	snapshot atomic.Pointer[ConfigSnapshot]

	keys     *UserTable[*KeyRecord, ResolvedKey]
	profiles *UserTable[*ProfileRecord, ResolvedProfile]

	// interns is shared across all snapshots: the pool is append-only so handles
	// assigned in earlier snapshots remain valid indefinitely.
	interns *InternPool

	// generation monotonically increases on every successful apply. It tags each
	// ConfigSnapshot so UserTable.Get can detect stale L1 entries.
	generation atomic.Uint64

	// lastVersion tracks the highest accepted envelope Version (SoTW semantics).
	// An incoming envelope whose Version is not strictly greater than lastVersion
	// is silently discarded; the current snapshot stays live.
	// Version 0 is exempt: it is treated as "no version tracking" and is always
	// applied (used by LoadConfig and dev/seed callers).
	lastVersion atomic.Uint64
}

// NewAppState returns an AppState ready to accept snapshots. The snapshot
// pointer is nil until the first successful Apply or Load call; Resolve returns
// an error until a snapshot is published.
func NewAppState() *AppState {
	return &AppState{
		keys:     NewUserTable[*KeyRecord, ResolvedKey](),
		profiles: NewUserTable[*ProfileRecord, ResolvedProfile](),
		interns:  NewInternPool(),
	}
}

// Snapshot returns the current *ConfigSnapshot, or nil if no snapshot has been
// published yet. The caller must not modify the returned value.
func (s *AppState) Snapshot() *ConfigSnapshot {
	return s.snapshot.Load()
}

// ApplySnapshotEnvelope decodes and compiles envelope, then atomically publishes
// the result. It is safe to call from multiple goroutines; each call competes to
// publish but compile errors leave the current snapshot live.
//
// SoTW stale rejection: if envelope.Version is non-zero and not strictly greater
// than the last accepted version, the envelope is silently discarded and nil is
// returned. This is not an error — the current snapshot is already up to date.
func (s *AppState) ApplySnapshotEnvelope(envelope SnapshotEnvelope) error {
	if envelope.Version > 0 && envelope.Version <= s.lastVersion.Load() {
		return nil // stale SoTW payload; current snapshot stays live
	}

	raw, err := decodeRawConfig(envelope)
	if err != nil {
		return fmt.Errorf("appstate: decode: %w", err)
	}

	generation := s.generation.Add(1)
	snapshot, err := compile(raw, s.interns, generation)
	if err != nil {
		return fmt.Errorf("appstate: compile: %w", err)
	}

	// Seed tables before publishing the snapshot so readers never observe a new
	// snapshot paired with stale L2 data (§14).
	s.keys.Seed(snapshot.Keys)
	s.profiles.Seed(snapshot.Profiles)
	s.snapshot.Store(snapshot)

	if envelope.Version > 0 {
		s.lastVersion.Store(envelope.Version)
	}
	return nil
}

// LoadConfig is a convenience wrapper for loading a raw YAML config. It wraps
// the bytes in a YAML SnapshotEnvelope with no version tracking, so it always
// replaces the current snapshot regardless of any prior version. Intended for
// local development and seed files.
func (s *AppState) LoadConfig(yamlBytes []byte) error {
	return s.ApplySnapshotEnvelope(SnapshotEnvelope{
		Format:      SnapshotFormatYAML,
		Compression: CompressionNone,
		Payload:     yamlBytes,
	})
}

// Resolve returns the fully resolved key and profile for a request. It performs
// one atomic snapshot load followed by two UserTable lookups.
//
// An error is returned if no snapshot has been published, if either ID is absent
// from the current snapshot's user records, or if the resolve function fails.
func (s *AppState) Resolve(keyID, profileID string) (*ResolvedRequest, error) {
	snapshot := s.snapshot.Load()
	if snapshot == nil {
		return nil, errors.New("appstate: config is not loaded")
	}

	key, err := s.keys.Get(keyID, snapshot, ResolveKey)
	if err != nil {
		return nil, fmt.Errorf("appstate: key %q: %w", keyID, err)
	}

	profile, err := s.profiles.Get(profileID, snapshot, ResolveProfile)
	if err != nil {
		return nil, fmt.Errorf("appstate: profile %q: %w", profileID, err)
	}

	return &ResolvedRequest{Key: key, Profile: profile}, nil
}
