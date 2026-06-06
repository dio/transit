package config

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// newStringTable returns a UserTable[string, string] for simple unit tests.
func newStringTable() *UserTable[string, string] {
	return NewUserTable[string, string]()
}

// identityResolve is a resolve function that returns the minimal record unchanged.
func identityResolve(m string, _ *ConfigSnapshot) (string, error) {
	return m, nil
}

// minimalSnapshot returns a ConfigSnapshot with the given generation and a
// populated GlobalConfig+Pools so resolve helpers have valid pointers.
func minimalSnapshot(gen uint64) *ConfigSnapshot {
	interns := NewInternPool()
	g := &GlobalConfig{
		Providers:  map[string]*ProviderRecord{},
		Models:     map[string]*ModelRecord{},
		Servers:    map[string]*ServerRecord{},
		RateLimits: map[string][]RateLimitRule{},
		Interns:    interns,
	}
	pools := &Pools{
		Routing:     &RoutingPool{index: map[string]uint32{}},
		ToolFilters: &ToolFilterPool{index: map[string]uint32{}},
		Auth:        &AuthPool{index: map[string]uint32{}},
	}
	return &ConfigSnapshot{
		Generation: gen,
		Global:     g,
		Pools:      pools,
	}
}

// ── UserTable.Get ─────────────────────────────────────────────────────────────

func TestUserTable_Get_BeforeSeed_IsError(t *testing.T) {
	tbl := newStringTable()
	snap := minimalSnapshot(1)
	_, err := tbl.Get("k", snap, identityResolve)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not seeded")
}

func TestUserTable_Get_RecordNotFound_IsError(t *testing.T) {
	tbl := newStringTable()
	tbl.Seed(map[string]string{"a": "alpha"})
	snap := minimalSnapshot(1)
	_, err := tbl.Get("missing", snap, identityResolve)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestUserTable_Get_L2Hit_ResolvesValue(t *testing.T) {
	tbl := newStringTable()
	tbl.Seed(map[string]string{"k": "hello"})
	snap := minimalSnapshot(1)
	v, err := tbl.Get("k", snap, identityResolve)
	require.NoError(t, err)
	assert.Equal(t, "hello", v)
}

func TestUserTable_Get_L1Hit_SameGeneration_SkipsResolve(t *testing.T) {
	tbl := newStringTable()
	tbl.Seed(map[string]string{"k": "hello"})
	snap := minimalSnapshot(1)

	callCount := 0
	resolve := func(m string, _ *ConfigSnapshot) (string, error) {
		callCount++
		return m + "-resolved", nil
	}

	v1, err := tbl.Get("k", snap, resolve)
	require.NoError(t, err)
	assert.Equal(t, "hello-resolved", v1)
	assert.Equal(t, 1, callCount)

	// Second call with the same snapshot generation: must hit L1.
	v2, err := tbl.Get("k", snap, resolve)
	require.NoError(t, err)
	assert.Equal(t, "hello-resolved", v2)
	assert.Equal(t, 1, callCount, "resolve must not be called again on L1 hit")
}

func TestUserTable_Get_L1Stale_DifferentGeneration_ReResolvesFromL2(t *testing.T) {
	tbl := newStringTable()
	tbl.Seed(map[string]string{"k": "v"})
	snap1 := minimalSnapshot(1)
	snap2 := minimalSnapshot(2)

	callCount := 0
	resolve := func(m string, _ *ConfigSnapshot) (string, error) {
		callCount++
		return fmt.Sprintf("%s-gen%d", m, callCount), nil
	}

	// Populate L1 with generation 1.
	_, err := tbl.Get("k", snap1, resolve)
	require.NoError(t, err)
	assert.Equal(t, 1, callCount)

	// Generation changed: L1 entry is stale; re-resolve from L2.
	_, err = tbl.Get("k", snap2, resolve)
	require.NoError(t, err)
	assert.Equal(t, 2, callCount, "must re-resolve when snapshot generation changes")
}

func TestUserTable_Get_ResolveError_NotCached(t *testing.T) {
	tbl := newStringTable()
	tbl.Seed(map[string]string{"k": "v"})
	snap := minimalSnapshot(1)

	wantErr := errors.New("resolve failed")
	callCount := 0
	resolve := func(m string, _ *ConfigSnapshot) (string, error) {
		callCount++
		if callCount == 1 {
			return "", wantErr
		}
		return "ok", nil
	}

	_, err := tbl.Get("k", snap, resolve)
	require.ErrorIs(t, err, wantErr)

	// Error must not be cached: second call should invoke resolve again.
	v, err := tbl.Get("k", snap, resolve)
	require.NoError(t, err)
	assert.Equal(t, "ok", v)
	assert.Equal(t, 2, callCount)
}

// ── UserTable.Seed ────────────────────────────────────────────────────────────

func TestUserTable_Seed_ReplacesL2(t *testing.T) {
	tbl := newStringTable()
	snap := minimalSnapshot(1)

	tbl.Seed(map[string]string{"old": "first"})
	v, err := tbl.Get("old", snap, identityResolve)
	require.NoError(t, err)
	assert.Equal(t, "first", v)

	// Replace with a new map that has "new" but not "old".
	tbl.Seed(map[string]string{"new": "second"})
	snap2 := minimalSnapshot(2)

	_, err = tbl.Get("old", snap2, identityResolve)
	require.Error(t, err, "old key must not be found after re-seed")

	v2, err := tbl.Get("new", snap2, identityResolve)
	require.NoError(t, err)
	assert.Equal(t, "second", v2)
}

func TestUserTable_Seed_PurgesL1(t *testing.T) {
	tbl := newStringTable()
	snap1 := minimalSnapshot(1)

	tbl.Seed(map[string]string{"k": "v1"})
	resolve := func(m string, _ *ConfigSnapshot) (string, error) { return m, nil }

	// Warm up L1.
	_, err := tbl.Get("k", snap1, resolve)
	require.NoError(t, err)
	assert.Equal(t, 1, tbl.l1.Len())

	// Re-seed must purge L1.
	tbl.Seed(map[string]string{"k": "v2"})
	assert.Equal(t, 0, tbl.l1.Len(), "Seed must purge L1")
}

// ── Concurrent safety ─────────────────────────────────────────────────────────

func TestUserTable_Concurrent(t *testing.T) {
	tbl := newStringTable()
	tbl.Seed(map[string]string{"a": "1", "b": "2", "c": "3"})
	snap := minimalSnapshot(1)

	const goroutines = 20
	const iters = 200
	var wg sync.WaitGroup
	wg.Add(goroutines)

	keys := []string{"a", "b", "c", "missing"}
	for i := range goroutines {
		go func(i int) {
			defer wg.Done()
			for j := range iters {
				key := keys[(i+j)%len(keys)]
				_, _ = tbl.Get(key, snap, identityResolve)
			}
		}(i)
	}

	// Intermittent re-seeds during concurrent reads.
	for range 5 {
		tbl.Seed(map[string]string{"a": "x", "b": "y", "c": "z"})
	}
	wg.Wait()
}

// ── ResolveKey ────────────────────────────────────────────────────────────────

func TestResolveKey_NoInternPool_IsError(t *testing.T) {
	snap := &ConfigSnapshot{Generation: 1, Global: &GlobalConfig{}}
	rec := &KeyRecord{Workspace: 0, User: 0}
	_, err := ResolveKey(rec, snap)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "intern pool")
}

func TestResolveKey_NoRoutingPool_IsError(t *testing.T) {
	snap := &ConfigSnapshot{
		Generation: 1,
		Global:     &GlobalConfig{Interns: NewInternPool()},
		// Pools is nil
	}
	rec := &KeyRecord{}
	_, err := ResolveKey(rec, snap)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "routing pool")
}

func TestResolveKey_BasicMapping(t *testing.T) {
	snap := minimalSnapshot(1)
	wsID := snap.Global.Interns.Intern("acme")
	userID := snap.Global.Interns.Intern("alice")

	rec := &KeyRecord{
		Workspace: wsID,
		User:      userID,
	}
	rk, err := ResolveKey(rec, snap)
	require.NoError(t, err)
	assert.Equal(t, "acme", rk.Workspace)
	assert.Equal(t, "alice", rk.User)
	assert.Nil(t, rk.RoutingOverrides)
}

func TestResolveKey_WithRoutingOverrides(t *testing.T) {
	snap := minimalSnapshot(1)
	wsID := snap.Global.Interns.Intern("ws")
	userID := snap.Global.Interns.Intern("usr")

	routing := RoutingConfig{Kind: RoutingKindTarget}
	shapeKey := "shape-1"
	snap.Pools.Routing.Intern(shapeKey, routing)

	rec := &KeyRecord{
		Workspace:        wsID,
		User:             userID,
		RoutingShapeKeys: map[string]string{"claude-3-5-sonnet": shapeKey},
	}
	rk, err := ResolveKey(rec, snap)
	require.NoError(t, err)
	require.NotNil(t, rk.RoutingOverrides)
	rc := rk.RoutingOverrides["claude-3-5-sonnet"]
	require.NotNil(t, rc)
	assert.Equal(t, RoutingKindTarget, rc.Kind)
}

// ── ResolveProfile ────────────────────────────────────────────────────────────

func TestResolveProfile_NoInternPool_IsError(t *testing.T) {
	snap := &ConfigSnapshot{Generation: 1, Global: &GlobalConfig{}}
	rec := &ProfileRecord{}
	_, err := ResolveProfile(rec, snap)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "intern pool")
}

func TestResolveProfile_NoPool_IsError(t *testing.T) {
	snap := &ConfigSnapshot{
		Generation: 1,
		Global:     &GlobalConfig{Interns: NewInternPool()},
	}
	rec := &ProfileRecord{}
	_, err := ResolveProfile(rec, snap)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pools")
}

func TestResolveProfile_BasicMapping(t *testing.T) {
	snap := minimalSnapshot(1)
	wsID := snap.Global.Interns.Intern("acme")
	userID := snap.Global.Interns.Intern("bob")

	rec := &ProfileRecord{Workspace: wsID, User: userID}
	rp, err := ResolveProfile(rec, snap)
	require.NoError(t, err)
	assert.Equal(t, "acme", rp.Workspace)
	assert.Equal(t, "bob", rp.User)
	assert.Nil(t, rp.ToolFilters)
	assert.Nil(t, rp.AuthOverrides)
}

func TestResolveProfile_WithShapeKeys(t *testing.T) {
	snap := minimalSnapshot(1)
	wsID := snap.Global.Interns.Intern("ws")
	userID := snap.Global.Interns.Intern("usr")

	filters := []ToolFilter{{ServerID: "srv1", Include: []string{"tool_a"}}}
	tfKey := "tf-shape"
	snap.Pools.ToolFilters.Intern(tfKey, filters)

	auths := []AuthOverride{{ServerID: "srv1"}}
	authKey := "auth-shape"
	snap.Pools.Auth.Intern(authKey, auths)

	rec := &ProfileRecord{
		Workspace:          wsID,
		User:               userID,
		ToolFilterShapeKey: tfKey,
		AuthShapeKey:       authKey,
	}
	rp, err := ResolveProfile(rec, snap)
	require.NoError(t, err)
	assert.Equal(t, "ws", rp.Workspace)
	assert.Equal(t, filters, rp.ToolFilters)
	require.Len(t, rp.AuthOverrides, 1)
	assert.Equal(t, "srv1", rp.AuthOverrides[0].ServerID)
}

// ── Integration: UserTable with typed records ─────────────────────────────────

func TestUserTable_KeyRecord_Integration(t *testing.T) {
	snap := minimalSnapshot(5)
	wsID := snap.Global.Interns.Intern("demo")
	userID := snap.Global.Interns.Intern("adi")

	routing := RoutingConfig{Kind: RoutingKindTarget}
	shapeKey := "sk"
	snap.Pools.Routing.Intern(shapeKey, routing)

	keyID := "demo/adi/sk-direct"
	rec := &KeyRecord{
		Workspace:        wsID,
		User:             userID,
		RoutingShapeKeys: map[string]string{"*": shapeKey},
	}

	tbl := NewUserTable[*KeyRecord, ResolvedKey]()
	tbl.Seed(map[string]*KeyRecord{keyID: rec})

	rk, err := tbl.Get(keyID, snap, ResolveKey)
	require.NoError(t, err)
	assert.Equal(t, "demo", rk.Workspace)
	assert.Equal(t, "adi", rk.User)
	assert.NotNil(t, rk.RoutingOverrides["*"])

	// Second Get: L1 hit; ResolveKey not called again (generation unchanged).
	rk2, err := tbl.Get(keyID, snap, ResolveKey)
	require.NoError(t, err)
	assert.Equal(t, rk, rk2)
}

func TestUserTable_ProfileRecord_Integration(t *testing.T) {
	snap := minimalSnapshot(3)
	wsID := snap.Global.Interns.Intern("org")
	userID := snap.Global.Interns.Intern("user1")

	profileID := "org/user1/default"
	rec := &ProfileRecord{Workspace: wsID, User: userID}

	tbl := NewUserTable[*ProfileRecord, ResolvedProfile]()
	tbl.Seed(map[string]*ProfileRecord{profileID: rec})

	rp, err := tbl.Get(profileID, snap, ResolveProfile)
	require.NoError(t, err)
	assert.Equal(t, "org", rp.Workspace)
	assert.Equal(t, "user1", rp.User)
}
