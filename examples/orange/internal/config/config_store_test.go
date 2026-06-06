package config

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── compile-time interface check ──────────────────────────────────────────────

var _ SnapshotStore = (*MemSnapshotStore)(nil)

// ── helpers ───────────────────────────────────────────────────────────────────

const testWorkspace = "ws-test"

// makeEnv returns a minimal SnapshotEnvelope with the given version and a
// small synthetic payload. Format and compression are set to YAML/none so the
// envelope is self-consistent without needing real proto bytes.
func makeEnv(version uint64) *SnapshotEnvelope {
	return &SnapshotEnvelope{
		Version:     version,
		Format:      SnapshotFormatYAML,
		Compression: CompressionNone,
		Payload:     []byte("# synthetic payload v" + string(rune('0'+version%10))),
	}
}

// ── FetchLatest ───────────────────────────────────────────────────────────────

func TestMemSnapshotStore_FetchLatest_EmptyStore(t *testing.T) {
	s := NewMemSnapshotStore()
	env, err := s.FetchLatest(context.Background(), testWorkspace, 0)
	require.NoError(t, err)
	assert.Nil(t, env, "empty store must return nil, nil")
}

func TestMemSnapshotStore_FetchLatest_ReturnsHighestVersion(t *testing.T) {
	s := NewMemSnapshotStore()
	for _, v := range []uint64{1, 3, 2} {
		require.NoError(t, s.Store(context.Background(), makeEnv(v), testWorkspace, "test", nil))
	}
	env, err := s.FetchLatest(context.Background(), testWorkspace, 0)
	require.NoError(t, err)
	require.NotNil(t, env)
	assert.Equal(t, uint64(3), env.Version)
}

func TestMemSnapshotStore_FetchLatest_AlreadyLatest_ReturnsNil(t *testing.T) {
	s := NewMemSnapshotStore()
	require.NoError(t, s.Store(context.Background(), makeEnv(5), testWorkspace, "test", nil))

	// Caller already at version 5 — nothing newer.
	env, err := s.FetchLatest(context.Background(), testWorkspace, 5)
	require.NoError(t, err)
	assert.Nil(t, env, "must return nil when sinceVersion >= latest")
}

func TestMemSnapshotStore_FetchLatest_NewerThanSince(t *testing.T) {
	s := NewMemSnapshotStore()
	for _, v := range []uint64{3, 5, 7} {
		require.NoError(t, s.Store(context.Background(), makeEnv(v), testWorkspace, "test", nil))
	}
	// Caller has version 4; versions 5 and 7 are newer, 7 wins.
	env, err := s.FetchLatest(context.Background(), testWorkspace, 4)
	require.NoError(t, err)
	require.NotNil(t, env)
	assert.Equal(t, uint64(7), env.Version)
}

func TestMemSnapshotStore_FetchLatest_SkipsFailedCompile(t *testing.T) {
	s := NewMemSnapshotStore()
	// Version 2 compiled OK; version 3 failed.
	require.NoError(t, s.Store(context.Background(), makeEnv(2), testWorkspace, "test", nil))
	require.NoError(t, s.Store(context.Background(), makeEnv(3), testWorkspace, "test", errors.New("compile failed")))

	env, err := s.FetchLatest(context.Background(), testWorkspace, 0)
	require.NoError(t, err)
	require.NotNil(t, env)
	assert.Equal(t, uint64(2), env.Version, "failed-compile row must not be served")
}

func TestMemSnapshotStore_FetchLatest_AllFailedCompile_ReturnsNil(t *testing.T) {
	s := NewMemSnapshotStore()
	compileErr := errors.New("bad config")
	for _, v := range []uint64{1, 2, 3} {
		require.NoError(t, s.Store(context.Background(), makeEnv(v), testWorkspace, "test", compileErr))
	}
	env, err := s.FetchLatest(context.Background(), testWorkspace, 0)
	require.NoError(t, err)
	assert.Nil(t, env, "all failed-compile entries must yield nil, nil")
}

func TestMemSnapshotStore_FetchLatest_CancelledContext(t *testing.T) {
	s := NewMemSnapshotStore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.FetchLatest(ctx, testWorkspace, 0)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// ── FetchLatest workspace isolation ──────────────────────────────────────────

func TestMemSnapshotStore_FetchLatest_WorkspaceIsolation(t *testing.T) {
	s := NewMemSnapshotStore()
	require.NoError(t, s.Store(context.Background(), makeEnv(10), "ws-a", "test", nil))
	require.NoError(t, s.Store(context.Background(), makeEnv(1), "ws-b", "test", nil))

	envA, err := s.FetchLatest(context.Background(), "ws-a", 0)
	require.NoError(t, err)
	require.NotNil(t, envA)
	assert.Equal(t, uint64(10), envA.Version, "ws-a must not see ws-b versions")

	envB, err := s.FetchLatest(context.Background(), "ws-b", 0)
	require.NoError(t, err)
	require.NotNil(t, envB)
	assert.Equal(t, uint64(1), envB.Version, "ws-b must not see ws-a versions")
}

// ── FetchVersion ──────────────────────────────────────────────────────────────

func TestMemSnapshotStore_FetchVersion_Found(t *testing.T) {
	s := NewMemSnapshotStore()
	want := makeEnv(42)
	want.Checksum = make([]byte, 32)
	require.NoError(t, s.Store(context.Background(), want, testWorkspace, "admin", nil))

	got, err := s.FetchVersion(context.Background(), testWorkspace, 42)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, uint64(42), got.Version)
	assert.Equal(t, want.Checksum, got.Checksum)
}

func TestMemSnapshotStore_FetchVersion_NotFound_IsError(t *testing.T) {
	s := NewMemSnapshotStore()
	_, err := s.FetchVersion(context.Background(), testWorkspace, 99)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSnapshotNotFound)
}

func TestMemSnapshotStore_FetchVersion_FailedCompile_IsError(t *testing.T) {
	// A failed-compile entry must not be retrievable via FetchVersion (no rollback
	// to a known-bad snapshot).
	s := NewMemSnapshotStore()
	require.NoError(t, s.Store(context.Background(), makeEnv(7), testWorkspace, "test", errors.New("boom")))

	_, err := s.FetchVersion(context.Background(), testWorkspace, 7)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSnapshotNotFound)
}

func TestMemSnapshotStore_FetchVersion_CancelledContext(t *testing.T) {
	s := NewMemSnapshotStore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.FetchVersion(ctx, testWorkspace, 1)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// ── Store ─────────────────────────────────────────────────────────────────────

func TestMemSnapshotStore_Store_NilEnvelope_IsError(t *testing.T) {
	s := NewMemSnapshotStore()
	err := s.Store(context.Background(), nil, testWorkspace, "test", nil)
	require.Error(t, err)
}

func TestMemSnapshotStore_Store_Idempotent(t *testing.T) {
	// Storing the same version twice must be silently ignored (mirrors SQL
	// "on conflict do nothing").
	s := NewMemSnapshotStore()
	env := makeEnv(1)
	require.NoError(t, s.Store(context.Background(), env, testWorkspace, "first", nil))
	require.NoError(t, s.Store(context.Background(), env, testWorkspace, "second", nil), "duplicate store must not error")
	assert.Equal(t, 1, s.Len(), "idempotent store must not add a second row")
}

func TestMemSnapshotStore_Store_FailedCompileAudited(t *testing.T) {
	// A failed-compile entry is stored (for audit) but not served.
	s := NewMemSnapshotStore()
	require.NoError(t, s.Store(context.Background(), makeEnv(1), testWorkspace, "ci", errors.New("invalid model ref")))
	assert.Equal(t, 1, s.Len(), "failed-compile entry must be stored for audit")
}

func TestMemSnapshotStore_Store_CancelledContext(t *testing.T) {
	s := NewMemSnapshotStore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.Store(ctx, makeEnv(1), testWorkspace, "test", nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestMemSnapshotStore_Store_CopiesEnvelope(t *testing.T) {
	// Mutating the original envelope after Store must not affect the stored copy.
	s := NewMemSnapshotStore()
	env := makeEnv(1)
	original := env.Payload
	require.NoError(t, s.Store(context.Background(), env, testWorkspace, "test", nil))

	env.Payload = []byte("mutated")
	got, err := s.FetchVersion(context.Background(), testWorkspace, 1)
	require.NoError(t, err)
	assert.Equal(t, original, got.Payload, "store must copy the envelope, not share the pointer")
}

// ── Len ───────────────────────────────────────────────────────────────────────

func TestMemSnapshotStore_Len_CountsAllEntries(t *testing.T) {
	s := NewMemSnapshotStore()
	assert.Equal(t, 0, s.Len())
	require.NoError(t, s.Store(context.Background(), makeEnv(1), testWorkspace, "t", nil))
	require.NoError(t, s.Store(context.Background(), makeEnv(2), testWorkspace, "t", errors.New("fail")))
	assert.Equal(t, 2, s.Len(), "Len must count both compiled-ok and failed entries")
}

// ── NextVersion ───────────────────────────────────────────────────────────────

func TestMemSnapshotStore_NextVersion_EmptyReturnsOne(t *testing.T) {
	s := NewMemSnapshotStore()
	v, err := s.NextVersion(context.Background(), testWorkspace)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), v)
}

func TestMemSnapshotStore_NextVersion_AfterStores(t *testing.T) {
	s := NewMemSnapshotStore()
	for _, v := range []uint64{1, 3, 2} {
		require.NoError(t, s.Store(context.Background(), makeEnv(v), testWorkspace, "t", nil))
	}
	next, err := s.NextVersion(context.Background(), testWorkspace)
	require.NoError(t, err)
	assert.Equal(t, uint64(4), next)
}

// ── Concurrent safety ─────────────────────────────────────────────────────────

func TestMemSnapshotStore_Concurrent(t *testing.T) {
	s := NewMemSnapshotStore()
	const goroutines = 16
	const versions = 20

	// Pre-populate half the versions.
	for v := uint64(1); v <= versions/2; v++ {
		require.NoError(t, s.Store(context.Background(), makeEnv(v), testWorkspace, "setup", nil))
	}

	var wg sync.WaitGroup
	wg.Add(goroutines * 3)

	// Writers.
	for i := range goroutines {
		go func(i int) {
			defer wg.Done()
			v := uint64(versions/2+1) + uint64(i%(versions/2))
			_ = s.Store(context.Background(), makeEnv(v), testWorkspace, "writer", nil)
		}(i)
	}
	// FetchLatest readers.
	for range goroutines {
		go func() {
			defer wg.Done()
			_, _ = s.FetchLatest(context.Background(), testWorkspace, 0)
		}()
	}
	// FetchVersion readers.
	for i := range goroutines {
		go func(i int) {
			defer wg.Done()
			_, _ = s.FetchVersion(context.Background(), testWorkspace, uint64(i%versions)+1)
		}(i)
	}
	wg.Wait()
}

// ── ErrSnapshotNotFound ───────────────────────────────────────────────────────

func TestErrSnapshotNotFound_IsWrappable(t *testing.T) {
	// Confirm the sentinel can be detected through errors.Is on wrapped errors.
	wrapped := errors.Join(errors.New("outer"), ErrSnapshotNotFound)
	assert.ErrorIs(t, wrapped, ErrSnapshotNotFound)
}
