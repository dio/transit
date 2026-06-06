package config

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── fixtures ──────────────────────────────────────────────────────────────────

// minimalLLMYAML is the smallest valid config for AppState tests that do not
// need user records.
const minimalLLMYAML = `
llm:
  providers:
    p1:
      kind: anthropic
      endpoint: https://api.anthropic.com
      auth: {type: anthropic, secret_ref: env://T}
  models:
    m1: {provider: p1}
`

// fullAppStateYAML has one key and one profile so Resolve can succeed.
// Workspace, user, and name come from the map key (workspace/user/name);
// RawKey and RawProfile carry no workspace/user fields of their own.
const fullAppStateYAML = `
llm:
  providers:
    p1:
      kind: anthropic
      endpoint: https://api.anthropic.com
      auth: {type: anthropic, secret_ref: env://T}
  models:
    m1: {provider: p1}
mcp:
  servers:
    s1: {endpoint: https://mcp.example.com, namespace: ns}
keys:
  acme/alice/sk-001: {}
profiles:
  acme/alice/default:
    tools:
      s1: {}
`

// ── NewAppState ───────────────────────────────────────────────────────────────

func TestNewAppState_SnapshotIsNil(t *testing.T) {
	app := NewAppState()
	assert.Nil(t, app.Snapshot(), "snapshot must be nil before first apply")
}

func TestNewAppState_Resolve_BeforeLoad_IsError(t *testing.T) {
	app := NewAppState()
	_, err := app.Resolve("k", "p")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not loaded")
}

// ── LoadConfig ────────────────────────────────────────────────────────────────

func TestAppState_LoadConfig_PublishesSnapshot(t *testing.T) {
	app := NewAppState()
	require.NoError(t, app.LoadConfig([]byte(minimalLLMYAML)))
	snap := app.Snapshot()
	require.NotNil(t, snap)
	assert.Greater(t, snap.Generation, uint64(0))
}

func TestAppState_LoadConfig_InvalidYAML_IsError(t *testing.T) {
	app := NewAppState()
	// Unclosed bracket is a YAML parse error.
	err := app.LoadConfig([]byte("{unclosed: ["))
	require.Error(t, err)
	// Snapshot must remain nil — decode failure leaves old snapshot live.
	assert.Nil(t, app.Snapshot())
}

func TestAppState_LoadConfig_InvalidConfig_IsError(t *testing.T) {
	// A syntactically valid YAML that fails compile (unknown provider reference).
	const bad = `
llm:
  providers: {}
  models:
    m1: {provider: nonexistent}
`
	app := NewAppState()
	err := app.LoadConfig([]byte(bad))
	require.Error(t, err)
	assert.Nil(t, app.Snapshot())
}

func TestAppState_LoadConfig_Replaces_OldSnapshot(t *testing.T) {
	app := NewAppState()
	require.NoError(t, app.LoadConfig([]byte(minimalLLMYAML)))
	gen1 := app.Snapshot().Generation

	require.NoError(t, app.LoadConfig([]byte(minimalLLMYAML)))
	gen2 := app.Snapshot().Generation

	assert.Greater(t, gen2, gen1, "each successful load must bump generation")
}

func TestAppState_LoadConfig_FailedReload_KeepsOldSnapshot(t *testing.T) {
	app := NewAppState()
	require.NoError(t, app.LoadConfig([]byte(minimalLLMYAML)))
	snapBefore := app.Snapshot()
	require.NotNil(t, snapBefore)

	// Unclosed bracket causes a YAML parse error.
	err := app.LoadConfig([]byte("{unclosed: ["))
	require.Error(t, err)

	// Old snapshot must still be live.
	assert.Same(t, snapBefore, app.Snapshot(), "failed reload must not replace snapshot")
}

// ── ApplySnapshotEnvelope — version tracking ──────────────────────────────────

func TestAppState_Apply_Version0_AlwaysApplied(t *testing.T) {
	// Version == 0 bypasses stale-rejection; every call applies.
	app := NewAppState()
	env := SnapshotEnvelope{
		Format:      SnapshotFormatYAML,
		Compression: CompressionNone,
		Payload:     []byte(minimalLLMYAML),
		Version:     0,
	}
	require.NoError(t, app.ApplySnapshotEnvelope(env))
	gen1 := app.Snapshot().Generation

	require.NoError(t, app.ApplySnapshotEnvelope(env))
	gen2 := app.Snapshot().Generation
	assert.Greater(t, gen2, gen1, "version-0 envelopes must always be applied")
}

func TestAppState_Apply_MonotonicVersions_Accepted(t *testing.T) {
	app := NewAppState()
	for _, v := range []uint64{1, 2, 3} {
		env := SnapshotEnvelope{
			Format:      SnapshotFormatYAML,
			Compression: CompressionNone,
			Payload:     []byte(minimalLLMYAML),
			Version:     v,
		}
		require.NoError(t, app.ApplySnapshotEnvelope(env), "version %d must be accepted", v)
	}
	assert.Equal(t, uint64(3), app.lastVersion.Load())
}

func TestAppState_Apply_StaleVersion_SkippedSilently(t *testing.T) {
	app := NewAppState()

	v2 := SnapshotEnvelope{
		Format:      SnapshotFormatYAML,
		Compression: CompressionNone,
		Payload:     []byte(minimalLLMYAML),
		Version:     2,
	}
	require.NoError(t, app.ApplySnapshotEnvelope(v2))
	genAfterV2 := app.Snapshot().Generation

	// Re-delivering version 1 (older) must be silently discarded.
	v1 := SnapshotEnvelope{
		Format:      SnapshotFormatYAML,
		Compression: CompressionNone,
		Payload:     []byte(minimalLLMYAML),
		Version:     1,
	}
	require.NoError(t, app.ApplySnapshotEnvelope(v1), "stale version must not return an error")
	assert.Equal(t, genAfterV2, app.Snapshot().Generation, "stale version must not change generation")
}

func TestAppState_Apply_SameVersion_SkippedSilently(t *testing.T) {
	app := NewAppState()
	env := SnapshotEnvelope{
		Format:      SnapshotFormatYAML,
		Compression: CompressionNone,
		Payload:     []byte(minimalLLMYAML),
		Version:     5,
	}
	require.NoError(t, app.ApplySnapshotEnvelope(env))
	genAfter := app.Snapshot().Generation

	// Same version again — discarded.
	require.NoError(t, app.ApplySnapshotEnvelope(env))
	assert.Equal(t, genAfter, app.Snapshot().Generation, "same version must not re-apply")
}

// ── Resolve ───────────────────────────────────────────────────────────────────

func TestAppState_Resolve_KeyNotFound_IsError(t *testing.T) {
	app := NewAppState()
	require.NoError(t, app.LoadConfig([]byte(minimalLLMYAML)))
	_, err := app.Resolve("no/such/key", "no/such/profile")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestAppState_Resolve_ProfileNotFound_IsError(t *testing.T) {
	app := NewAppState()
	require.NoError(t, app.LoadConfig([]byte(fullAppStateYAML)))
	// Key exists; profile does not.
	_, err := app.Resolve("acme/alice/sk-001", "acme/alice/missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestAppState_Resolve_ReturnsKeyAndProfile(t *testing.T) {
	app := NewAppState()
	require.NoError(t, app.LoadConfig([]byte(fullAppStateYAML)))

	req, err := app.Resolve("acme/alice/sk-001", "acme/alice/default")
	require.NoError(t, err)
	assert.Equal(t, "acme", req.Key.Workspace)
	assert.Equal(t, "alice", req.Key.User)
	assert.Equal(t, "acme", req.Profile.Workspace)
	assert.Equal(t, "alice", req.Profile.User)
}

func TestAppState_Resolve_AfterHotReload_SeesNewSnapshot(t *testing.T) {
	app := NewAppState()
	require.NoError(t, app.LoadConfig([]byte(fullAppStateYAML)))
	gen1 := app.Snapshot().Generation

	// Reload with the same data; generation must increase.
	require.NoError(t, app.LoadConfig([]byte(fullAppStateYAML)))
	gen2 := app.Snapshot().Generation
	assert.Greater(t, gen2, gen1)

	// Resolve must still work after reload.
	req, err := app.Resolve("acme/alice/sk-001", "acme/alice/default")
	require.NoError(t, err)
	assert.Equal(t, "alice", req.Key.User)
}

func TestAppState_Resolve_L1HitAfterTwoResolves(t *testing.T) {
	app := NewAppState()
	require.NoError(t, app.LoadConfig([]byte(fullAppStateYAML)))

	// First resolve populates L1.
	r1, err := app.Resolve("acme/alice/sk-001", "acme/alice/default")
	require.NoError(t, err)

	// Second resolve with same snapshot must return same values (L1 hit).
	r2, err := app.Resolve("acme/alice/sk-001", "acme/alice/default")
	require.NoError(t, err)
	assert.Equal(t, r1, r2)
}

// ── Concurrent safety ─────────────────────────────────────────────────────────

func TestAppState_Concurrent_LoadAndResolve(t *testing.T) {
	app := NewAppState()
	require.NoError(t, app.LoadConfig([]byte(fullAppStateYAML)))

	const workers = 16
	const iters = 50

	var wg sync.WaitGroup
	wg.Add(workers * 2)

	// Concurrent resolvers.
	for range workers {
		go func() {
			defer wg.Done()
			for range iters {
				_, _ = app.Resolve("acme/alice/sk-001", "acme/alice/default")
			}
		}()
	}

	// Concurrent reloaders.
	for range workers {
		go func() {
			defer wg.Done()
			for range iters {
				_ = app.LoadConfig([]byte(fullAppStateYAML))
			}
		}()
	}

	wg.Wait()
}

func TestAppState_Concurrent_ApplyVersioned(t *testing.T) {
	// Multiple goroutines applying versioned envelopes; only the latest version
	// wins. No panics, no data races.
	app := NewAppState()

	const goroutines = 8
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := range goroutines {
		go func(ver uint64) {
			defer wg.Done()
			env := SnapshotEnvelope{
				Format:      SnapshotFormatYAML,
				Compression: CompressionNone,
				Payload:     []byte(minimalLLMYAML),
				Version:     ver + 1,
			}
			_ = app.ApplySnapshotEnvelope(env)
		}(uint64(i))
	}
	wg.Wait()

	// After all goroutines finish, a snapshot must be live.
	assert.NotNil(t, app.Snapshot())
}
