package config

import (
	"fmt"
	"sync/atomic"

	lru "github.com/hashicorp/golang-lru/v2"
)

// ── Resolved types ────────────────────────────────────────────────────────────

// ResolvedEntry wraps a resolved value with the snapshot generation it was
// resolved against. A cached entry is valid only while its Generation matches
// the current snapshot's Generation; a mismatch triggers re-resolution from L2.
type ResolvedEntry[R any] struct {
	Generation uint64
	Value      R
}

// ResolvedKey is the fully resolved form of a *KeyRecord. It materialises the
// intern-handle workspace/user names as plain strings and expands shape keys
// into *RoutingConfig pointers so the data plane pays no further lookup cost.
type ResolvedKey struct {
	Workspace string
	User      string
	// RoutingOverrides maps model IDs to per-model routing overrides declared on
	// this key. An absent entry means "use the global routing default for that
	// model". The map is nil when the key carries no overrides.
	RoutingOverrides map[string]*RoutingConfig
}

// ResolvedProfile is the fully resolved form of a *ProfileRecord. Shape keys
// for tool filters and auth overrides are expanded into concrete slices so the
// request path performs a single pointer dereference.
type ResolvedProfile struct {
	Workspace     string
	User          string
	ToolFilters   []ToolFilter
	AuthOverrides []AuthOverride
}

// ── UserTable ─────────────────────────────────────────────────────────────────

// UserTable[M, R] is a two-tier read cache for user-owned records.
//
// L2 holds the minimal representation seeded directly from a ConfigSnapshot
// (an atomic immutable map). L1 is an LRU of fully resolved records tagged
// with the snapshot generation that produced them.
//
//   - L1 hit  (same generation) → ~100 ns; no lock contention
//   - L1 miss / stale generation → resolve from L2 via the provided function
//   - L2 miss → record not found in snapshot; return error
//
// The only way records change is via a new snapshot. Seed atomically replaces
// L2 and purges L1; readers never observe a new snapshot paired with stale L2.
//
// UserTable is safe for concurrent use. lru.Cache uses its own internal mutex;
// atomic.Pointer is lock-free.
type UserTable[M, R any] struct {
	l1 *lru.Cache[string, ResolvedEntry[R]]
	l2 atomic.Pointer[map[string]M]
}

// NewUserTable returns a UserTable with an L1 capacity of 100,000 entries.
func NewUserTable[M, R any]() *UserTable[M, R] {
	cache, _ := lru.New[string, ResolvedEntry[R]](100_000)
	return &UserTable[M, R]{l1: cache}
}

// Seed atomically replaces the L2 record set and purges L1.
// Called by AppState.ApplySnapshotEnvelope before the new ConfigSnapshot is
// published, so readers never see a new snapshot with stale L2 data.
func (t *UserTable[M, R]) Seed(records map[string]M) {
	t.l2.Store(&records)
	t.l1.Purge()
}

// Get returns the resolved record for id. On an L1 hit with a matching
// generation, the cached value is returned directly. Otherwise the minimal
// record is looked up in L2, passed to resolve, and the result is stored in L1.
//
// resolve may be called concurrently for different ids; it is the caller's
// responsibility to ensure resolve is safe for concurrent use.
//
// Returns an error if L2 has not been seeded, the id is absent, or resolve
// returns an error. Errors from resolve are not cached.
func (t *UserTable[M, R]) Get(
	id string,
	snapshot *ConfigSnapshot,
	resolve func(M, *ConfigSnapshot) (R, error),
) (R, error) {
	if entry, ok := t.l1.Get(id); ok && entry.Generation == snapshot.Generation {
		return entry.Value, nil
	}

	m2 := t.l2.Load()
	if m2 == nil {
		var zero R
		return zero, fmt.Errorf("usertable: record %q not found (table not seeded)", id)
	}

	rec, ok := (*m2)[id]
	if !ok {
		var zero R
		return zero, fmt.Errorf("usertable: record %q not found", id)
	}

	v, err := resolve(rec, snapshot)
	if err != nil {
		var zero R
		return zero, err
	}

	t.l1.Add(id, ResolvedEntry[R]{Generation: snapshot.Generation, Value: v})
	return v, nil
}

// ── Resolve helpers ───────────────────────────────────────────────────────────

// ResolveKey converts a *KeyRecord into a ResolvedKey by materialising intern
// handles and expanding routing shape keys via the snapshot's Pools.
// Returns an error if the snapshot has no Pools or its Interns are nil.
func ResolveKey(rec *KeyRecord, snap *ConfigSnapshot) (ResolvedKey, error) {
	if snap.Global == nil || snap.Global.Interns == nil {
		return ResolvedKey{}, fmt.Errorf("usertable: snapshot has no intern pool")
	}
	if snap.Pools == nil || snap.Pools.Routing == nil {
		return ResolvedKey{}, fmt.Errorf("usertable: snapshot has no routing pool")
	}
	interns := snap.Global.Interns
	rk := ResolvedKey{
		Workspace: interns.Lookup(rec.Workspace),
		User:      interns.Lookup(rec.User),
	}
	if len(rec.RoutingShapeKeys) > 0 {
		rk.RoutingOverrides = make(map[string]*RoutingConfig, len(rec.RoutingShapeKeys))
		for modelID, shapeKey := range rec.RoutingShapeKeys {
			rk.RoutingOverrides[modelID] = snap.Pools.Routing.GetByKey(shapeKey)
		}
	}
	return rk, nil
}

// ResolveProfile converts a *ProfileRecord into a ResolvedProfile by
// materialising intern handles and expanding pool shape keys.
// Returns an error if the snapshot has no Pools or its Interns are nil.
func ResolveProfile(rec *ProfileRecord, snap *ConfigSnapshot) (ResolvedProfile, error) {
	if snap.Global == nil || snap.Global.Interns == nil {
		return ResolvedProfile{}, fmt.Errorf("usertable: snapshot has no intern pool")
	}
	if snap.Pools == nil {
		return ResolvedProfile{}, fmt.Errorf("usertable: snapshot has no pools")
	}
	interns := snap.Global.Interns
	rp := ResolvedProfile{
		Workspace: interns.Lookup(rec.Workspace),
		User:      interns.Lookup(rec.User),
	}
	if rec.ToolFilterShapeKey != "" {
		rp.ToolFilters = snap.Pools.ToolFilters.GetByKey(rec.ToolFilterShapeKey)
	}
	if rec.AuthShapeKey != "" {
		rp.AuthOverrides = snap.Pools.Auth.GetByKey(rec.AuthShapeKey)
	}
	return rp, nil
}
