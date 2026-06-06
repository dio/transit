package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dio/transit/examples/orange/internal/embeddedpg/testpg"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testpg.Cleanup()
	os.Exit(code)
}

// tableSeq makes every newPgStore call use a unique table name so tests are
// fully isolated against the shared embedded postgres instance.
var tableSeq atomic.Uint64

func newPgStore(t *testing.T) *PgSnapshotStore {
	t.Helper()
	pool := testpg.Pool(t)
	name := fmt.Sprintf("config_snapshots_%d", tableSeq.Add(1))
	s, err := NewPgSnapshotStore(context.Background(), pool, WithTable(name))
	require.NoError(t, err)
	return s
}

// ── compile-time interface check ──────────────────────────────────────────────

var _ SnapshotStore = (*PgSnapshotStore)(nil)

// ── Migrate ───────────────────────────────────────────────────────────────────

func TestPgSnapshotStore_Migrate_Idempotent(t *testing.T) {
	pool := testpg.Pool(t)
	name := fmt.Sprintf("config_snapshots_%d", tableSeq.Add(1))
	// Call Migrate twice via NewPgSnapshotStore + explicit Migrate.
	s, err := NewPgSnapshotStore(context.Background(), pool, WithTable(name))
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()), "second Migrate must be idempotent")
}

func TestPgSnapshotStore_InvalidTableName_IsError(t *testing.T) {
	pool := testpg.Pool(t)
	_, err := NewPgSnapshotStore(context.Background(), pool, WithTable("bad name!"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid table name")
}

// ── FetchLatest ───────────────────────────────────────────────────────────────

func TestPgSnapshotStore_FetchLatest_EmptyStore(t *testing.T) {
	s := newPgStore(t)
	env, err := s.FetchLatest(context.Background(), testWorkspace, 0)
	require.NoError(t, err)
	assert.Nil(t, env)
}

func TestPgSnapshotStore_FetchLatest_ReturnsHighestVersion(t *testing.T) {
	s := newPgStore(t)
	for _, v := range []uint64{1, 3, 2} {
		require.NoError(t, s.Store(context.Background(), makeEnv(v), testWorkspace, "test", nil))
	}
	env, err := s.FetchLatest(context.Background(), testWorkspace, 0)
	require.NoError(t, err)
	require.NotNil(t, env)
	assert.Equal(t, uint64(3), env.Version)
}

func TestPgSnapshotStore_FetchLatest_AlreadyLatest_ReturnsNil(t *testing.T) {
	s := newPgStore(t)
	require.NoError(t, s.Store(context.Background(), makeEnv(5), testWorkspace, "test", nil))
	env, err := s.FetchLatest(context.Background(), testWorkspace, 5)
	require.NoError(t, err)
	assert.Nil(t, env)
}

func TestPgSnapshotStore_FetchLatest_NewerThanSince(t *testing.T) {
	s := newPgStore(t)
	for _, v := range []uint64{3, 5, 7} {
		require.NoError(t, s.Store(context.Background(), makeEnv(v), testWorkspace, "test", nil))
	}
	env, err := s.FetchLatest(context.Background(), testWorkspace, 4)
	require.NoError(t, err)
	require.NotNil(t, env)
	assert.Equal(t, uint64(7), env.Version)
}

func TestPgSnapshotStore_FetchLatest_SkipsFailedCompile(t *testing.T) {
	s := newPgStore(t)
	require.NoError(t, s.Store(context.Background(), makeEnv(2), testWorkspace, "ci", nil))
	require.NoError(t, s.Store(context.Background(), makeEnv(3), testWorkspace, "ci", errors.New("compile failed")))

	env, err := s.FetchLatest(context.Background(), testWorkspace, 0)
	require.NoError(t, err)
	require.NotNil(t, env)
	assert.Equal(t, uint64(2), env.Version, "failed-compile row must not be served")
}

func TestPgSnapshotStore_FetchLatest_AllFailedCompile_ReturnsNil(t *testing.T) {
	s := newPgStore(t)
	for _, v := range []uint64{1, 2, 3} {
		require.NoError(t, s.Store(context.Background(), makeEnv(v), testWorkspace, "ci", errors.New("bad config")))
	}
	env, err := s.FetchLatest(context.Background(), testWorkspace, 0)
	require.NoError(t, err)
	assert.Nil(t, env)
}

// ── FetchLatest workspace isolation ──────────────────────────────────────────

func TestPgSnapshotStore_FetchLatest_WorkspaceIsolation(t *testing.T) {
	s := newPgStore(t)
	require.NoError(t, s.Store(context.Background(), makeEnv(10), "ws-a", "test", nil))
	require.NoError(t, s.Store(context.Background(), makeEnv(1), "ws-b", "test", nil))

	envA, err := s.FetchLatest(context.Background(), "ws-a", 0)
	require.NoError(t, err)
	require.NotNil(t, envA)
	assert.Equal(t, uint64(10), envA.Version)

	envB, err := s.FetchLatest(context.Background(), "ws-b", 0)
	require.NoError(t, err)
	require.NotNil(t, envB)
	assert.Equal(t, uint64(1), envB.Version)
}

// ── FetchVersion ──────────────────────────────────────────────────────────────

func TestPgSnapshotStore_FetchVersion_Found(t *testing.T) {
	s := newPgStore(t)
	want := makeEnv(42)
	want.Checksum = make([]byte, 32)
	require.NoError(t, s.Store(context.Background(), want, testWorkspace, "admin", nil))

	got, err := s.FetchVersion(context.Background(), testWorkspace, 42)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, uint64(42), got.Version)
	assert.Equal(t, want.Format, got.Format)
	assert.Equal(t, want.Compression, got.Compression)
	assert.Equal(t, want.Payload, got.Payload)
	assert.Equal(t, want.Checksum, got.Checksum)
}

func TestPgSnapshotStore_FetchVersion_NotFound_IsError(t *testing.T) {
	s := newPgStore(t)
	_, err := s.FetchVersion(context.Background(), testWorkspace, 99)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSnapshotNotFound)
}

func TestPgSnapshotStore_FetchVersion_FailedCompile_IsError(t *testing.T) {
	s := newPgStore(t)
	require.NoError(t, s.Store(context.Background(), makeEnv(7), testWorkspace, "ci", errors.New("boom")))
	_, err := s.FetchVersion(context.Background(), testWorkspace, 7)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSnapshotNotFound)
}

// ── Store ─────────────────────────────────────────────────────────────────────

func TestPgSnapshotStore_Store_NilEnvelope_IsError(t *testing.T) {
	s := newPgStore(t)
	require.Error(t, s.Store(context.Background(), nil, testWorkspace, "test", nil))
}

func TestPgSnapshotStore_Store_VersionZero_IsError(t *testing.T) {
	s := newPgStore(t)
	env := makeEnv(0)
	err := s.Store(context.Background(), env, testWorkspace, "test", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "version 0")
}

func TestPgSnapshotStore_Store_Idempotent(t *testing.T) {
	s := newPgStore(t)
	env := makeEnv(1)
	require.NoError(t, s.Store(context.Background(), env, testWorkspace, "first", nil))
	require.NoError(t, s.Store(context.Background(), env, testWorkspace, "second", nil), "duplicate store must not error")

	// Only one row should exist.
	got, err := s.FetchVersion(context.Background(), testWorkspace, 1)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), got.Version)
}

func TestPgSnapshotStore_Store_FailedCompile_StoredButNotServed(t *testing.T) {
	s := newPgStore(t)
	// Failed compile stored for audit; should not be returned by FetchLatest/FetchVersion.
	require.NoError(t, s.Store(context.Background(), makeEnv(1), testWorkspace, "ci", errors.New("invalid model ref")))

	env, err := s.FetchLatest(context.Background(), testWorkspace, 0)
	require.NoError(t, err)
	assert.Nil(t, env)

	_, err = s.FetchVersion(context.Background(), testWorkspace, 1)
	require.ErrorIs(t, err, ErrSnapshotNotFound)
}

func TestPgSnapshotStore_Store_NilChecksum_RoundTrips(t *testing.T) {
	// nil checksum (dev/seed envelope) stores as NULL and comes back as nil.
	s := newPgStore(t)
	env := makeEnv(1)
	env.Checksum = nil
	require.NoError(t, s.Store(context.Background(), env, testWorkspace, "dev", nil))

	got, err := s.FetchVersion(context.Background(), testWorkspace, 1)
	require.NoError(t, err)
	assert.Nil(t, got.Checksum)
}

func TestPgSnapshotStore_Store_AllFormatsAndCompressions(t *testing.T) {
	tests := []struct {
		format      SnapshotFormat
		compression CompressionKind
	}{
		{SnapshotFormatYAML, CompressionNone},
		{SnapshotFormatJSON, CompressionNone},
		{SnapshotFormatProto, CompressionZstd},
		{SnapshotFormatMsgpack, CompressionNone},
	}
	s := newPgStore(t)
	for i, tc := range tests {
		env := &SnapshotEnvelope{
			Version:     uint64(i + 1),
			Format:      tc.format,
			Compression: tc.compression,
			Payload:     []byte("payload"),
		}
		require.NoError(t, s.Store(context.Background(), env, testWorkspace, "test", nil))
		got, err := s.FetchVersion(context.Background(), testWorkspace, env.Version)
		require.NoError(t, err)
		assert.Equal(t, tc.format, got.Format)
		assert.Equal(t, tc.compression, got.Compression)
	}
}

// ── NextVersion ───────────────────────────────────────────────────────────────

func TestPgSnapshotStore_NextVersion_EmptyReturnsOne(t *testing.T) {
	s := newPgStore(t)
	v, err := s.NextVersion(context.Background(), testWorkspace)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), v)
}

func TestPgSnapshotStore_NextVersion_AfterStores(t *testing.T) {
	s := newPgStore(t)
	for _, v := range []uint64{1, 3, 2} {
		require.NoError(t, s.Store(context.Background(), makeEnv(v), testWorkspace, "t", nil))
	}
	next, err := s.NextVersion(context.Background(), testWorkspace)
	require.NoError(t, err)
	assert.Equal(t, uint64(4), next)
}

// ── List ──────────────────────────────────────────────────────────────────────

func TestPgSnapshotStore_List_ReturnsDescendingVersions(t *testing.T) {
	s := newPgStore(t)
	for _, v := range []uint64{1, 2, 3} {
		require.NoError(t, s.Store(context.Background(), makeEnv(v), testWorkspace, "test", nil))
	}
	entries, err := s.List(context.Background(), testWorkspace, 10, 0)
	require.NoError(t, err)
	require.Len(t, entries, 3)
	assert.Equal(t, uint64(3), entries[0].Envelope.Version)
	assert.Equal(t, uint64(2), entries[1].Envelope.Version)
	assert.Equal(t, uint64(1), entries[2].Envelope.Version)
}

func TestPgSnapshotStore_List_RespectsCursor(t *testing.T) {
	s := newPgStore(t)
	for _, v := range []uint64{1, 2, 3, 4, 5} {
		require.NoError(t, s.Store(context.Background(), makeEnv(v), testWorkspace, "test", nil))
	}
	// afterVersion=3 → only versions 1 and 2
	entries, err := s.List(context.Background(), testWorkspace, 10, 3)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, uint64(2), entries[0].Envelope.Version)
	assert.Equal(t, uint64(1), entries[1].Envelope.Version)
}

func TestPgSnapshotStore_List_RespectsLimit(t *testing.T) {
	s := newPgStore(t)
	for _, v := range []uint64{1, 2, 3, 4, 5} {
		require.NoError(t, s.Store(context.Background(), makeEnv(v), testWorkspace, "test", nil))
	}
	entries, err := s.List(context.Background(), testWorkspace, 2, 0)
	require.NoError(t, err)
	assert.Len(t, entries, 2)
}
