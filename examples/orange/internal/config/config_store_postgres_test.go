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
	env, err := s.FetchLatest(context.Background(), 0)
	require.NoError(t, err)
	assert.Nil(t, env)
}

func TestPgSnapshotStore_FetchLatest_ReturnsHighestVersion(t *testing.T) {
	s := newPgStore(t)
	for _, v := range []uint64{1, 3, 2} {
		require.NoError(t, s.Store(context.Background(), makeEnv(v), "test", nil))
	}
	env, err := s.FetchLatest(context.Background(), 0)
	require.NoError(t, err)
	require.NotNil(t, env)
	assert.Equal(t, uint64(3), env.Version)
}

func TestPgSnapshotStore_FetchLatest_AlreadyLatest_ReturnsNil(t *testing.T) {
	s := newPgStore(t)
	require.NoError(t, s.Store(context.Background(), makeEnv(5), "test", nil))
	env, err := s.FetchLatest(context.Background(), 5)
	require.NoError(t, err)
	assert.Nil(t, env)
}

func TestPgSnapshotStore_FetchLatest_NewerThanSince(t *testing.T) {
	s := newPgStore(t)
	for _, v := range []uint64{3, 5, 7} {
		require.NoError(t, s.Store(context.Background(), makeEnv(v), "test", nil))
	}
	env, err := s.FetchLatest(context.Background(), 4)
	require.NoError(t, err)
	require.NotNil(t, env)
	assert.Equal(t, uint64(7), env.Version)
}

func TestPgSnapshotStore_FetchLatest_SkipsFailedCompile(t *testing.T) {
	s := newPgStore(t)
	require.NoError(t, s.Store(context.Background(), makeEnv(2), "ci", nil))
	require.NoError(t, s.Store(context.Background(), makeEnv(3), "ci", errors.New("compile failed")))

	env, err := s.FetchLatest(context.Background(), 0)
	require.NoError(t, err)
	require.NotNil(t, env)
	assert.Equal(t, uint64(2), env.Version, "failed-compile row must not be served")
}

func TestPgSnapshotStore_FetchLatest_AllFailedCompile_ReturnsNil(t *testing.T) {
	s := newPgStore(t)
	for _, v := range []uint64{1, 2, 3} {
		require.NoError(t, s.Store(context.Background(), makeEnv(v), "ci", errors.New("bad config")))
	}
	env, err := s.FetchLatest(context.Background(), 0)
	require.NoError(t, err)
	assert.Nil(t, env)
}

// ── FetchVersion ──────────────────────────────────────────────────────────────

func TestPgSnapshotStore_FetchVersion_Found(t *testing.T) {
	s := newPgStore(t)
	want := makeEnv(42)
	want.Checksum = make([]byte, 32)
	require.NoError(t, s.Store(context.Background(), want, "admin", nil))

	got, err := s.FetchVersion(context.Background(), 42)
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
	_, err := s.FetchVersion(context.Background(), 99)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSnapshotNotFound)
}

func TestPgSnapshotStore_FetchVersion_FailedCompile_IsError(t *testing.T) {
	s := newPgStore(t)
	require.NoError(t, s.Store(context.Background(), makeEnv(7), "ci", errors.New("boom")))
	_, err := s.FetchVersion(context.Background(), 7)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSnapshotNotFound)
}

// ── Store ─────────────────────────────────────────────────────────────────────

func TestPgSnapshotStore_Store_NilEnvelope_IsError(t *testing.T) {
	s := newPgStore(t)
	require.Error(t, s.Store(context.Background(), nil, "test", nil))
}

func TestPgSnapshotStore_Store_VersionZero_IsError(t *testing.T) {
	s := newPgStore(t)
	env := makeEnv(0)
	err := s.Store(context.Background(), env, "test", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "version 0")
}

func TestPgSnapshotStore_Store_Idempotent(t *testing.T) {
	s := newPgStore(t)
	env := makeEnv(1)
	require.NoError(t, s.Store(context.Background(), env, "first", nil))
	require.NoError(t, s.Store(context.Background(), env, "second", nil), "duplicate store must not error")

	// Only one row should exist.
	got, err := s.FetchVersion(context.Background(), 1)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), got.Version)
}

func TestPgSnapshotStore_Store_FailedCompile_StoredButNotServed(t *testing.T) {
	s := newPgStore(t)
	// Failed compile stored for audit; should not be returned by FetchLatest/FetchVersion.
	require.NoError(t, s.Store(context.Background(), makeEnv(1), "ci", errors.New("invalid model ref")))

	env, err := s.FetchLatest(context.Background(), 0)
	require.NoError(t, err)
	assert.Nil(t, env)

	_, err = s.FetchVersion(context.Background(), 1)
	require.ErrorIs(t, err, ErrSnapshotNotFound)
}

func TestPgSnapshotStore_Store_NilChecksum_RoundTrips(t *testing.T) {
	// nil checksum (dev/seed envelope) stores as NULL and comes back as nil.
	s := newPgStore(t)
	env := makeEnv(1)
	env.Checksum = nil
	require.NoError(t, s.Store(context.Background(), env, "dev", nil))

	got, err := s.FetchVersion(context.Background(), 1)
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
		require.NoError(t, s.Store(context.Background(), env, "test", nil))
		got, err := s.FetchVersion(context.Background(), env.Version)
		require.NoError(t, err)
		assert.Equal(t, tc.format, got.Format)
		assert.Equal(t, tc.compression, got.Compression)
	}
}
