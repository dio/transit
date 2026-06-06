package configv1

import (
	"testing"

	"buf.build/go/protovalidate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newValidator creates a protovalidate.Validator once and reuses it.
func newValidator(t *testing.T) protovalidate.Validator {
	t.Helper()
	v, err := protovalidate.New()
	require.NoError(t, err)
	return v
}

// ── FetchRequest.last_checksum ────────────────────────────────────────────────

func TestFetchRequest_Validate_EmptyChecksum_OK(t *testing.T) {
	v := newValidator(t)
	msg := &FetchRequest{LastVersion: 0} // unconditional fetch; no checksum
	require.NoError(t, v.Validate(msg))
}

func TestFetchRequest_Validate_ValidChecksum_OK(t *testing.T) {
	v := newValidator(t)
	msg := &FetchRequest{
		LastVersion:  5,
		LastChecksum: make([]byte, 32), // exactly 32 bytes (SHA-256)
	}
	require.NoError(t, v.Validate(msg))
}

func TestFetchRequest_Validate_ShortChecksum_Rejected(t *testing.T) {
	v := newValidator(t)
	msg := &FetchRequest{
		LastChecksum: []byte("too-short"), // 9 bytes — not a valid SHA-256
	}
	err := v.Validate(msg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "last_checksum must be empty or exactly 32 bytes")
}

func TestFetchRequest_Validate_LongChecksum_Rejected(t *testing.T) {
	v := newValidator(t)
	msg := &FetchRequest{
		LastChecksum: make([]byte, 64), // 64 bytes — SHA-512, not SHA-256
	}
	err := v.Validate(msg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "last_checksum must be empty or exactly 32 bytes")
}

// ── WatchResponse oneof ───────────────────────────────────────────────────────

func TestWatchResponse_Validate_WithSnapshot_OK(t *testing.T) {
	v := newValidator(t)
	msg := &WatchResponse{
		Event: &WatchResponse_Snapshot{Snapshot: &SnapshotEnvelope{Version: 1}},
	}
	require.NoError(t, v.Validate(msg))
}

func TestWatchResponse_Validate_WithHeartbeat_OK(t *testing.T) {
	v := newValidator(t)
	msg := &WatchResponse{
		Event: &WatchResponse_Heartbeat{Heartbeat: &Heartbeat{ServerVersion: 3}},
	}
	require.NoError(t, v.Validate(msg))
}

func TestWatchResponse_Validate_Empty_Rejected(t *testing.T) {
	v := newValidator(t)
	msg := &WatchResponse{} // neither snapshot nor heartbeat — invalid
	err := v.Validate(msg)
	require.Error(t, err, "WatchResponse with no event set must fail validation")
}

// ── FetchResponse oneof ───────────────────────────────────────────────────────

func TestFetchResponse_Validate_WithSnapshot_OK(t *testing.T) {
	v := newValidator(t)
	msg := &FetchResponse{
		Result: &FetchResponse_Snapshot{Snapshot: &SnapshotEnvelope{Version: 2}},
	}
	require.NoError(t, v.Validate(msg))
}

func TestFetchResponse_Validate_WithUnchanged_OK(t *testing.T) {
	v := newValidator(t)
	msg := &FetchResponse{
		Result: &FetchResponse_Unchanged{Unchanged: &Unchanged{}},
	}
	require.NoError(t, v.Validate(msg))
}

func TestFetchResponse_Validate_Empty_Rejected(t *testing.T) {
	v := newValidator(t)
	msg := &FetchResponse{} // neither snapshot nor unchanged — invalid
	err := v.Validate(msg)
	require.Error(t, err, "FetchResponse with no result set must fail validation")
}
