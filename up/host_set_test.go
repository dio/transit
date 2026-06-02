package up

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestHostSet_InitialApply verifies that the first Apply adds hosts and marks them HostHealthy.
func TestHostSet_InitialApply(t *testing.T) {
	h := &recordingHandle{}
	s := NewHostSet[string](h)

	snap := HostSnapshot[string]{
		"a": {Address: "127.0.0.1:8001"},
		"b": {Address: "127.0.0.1:8002"},
	}
	s.Apply(snap)

	adds := h.callsOfKind(callAddHosts)
	require.Len(t, adds, 1, "expected one AddHosts call")
	require.Len(t, adds[0].Hosts, 2)

	updates := h.callsOfKind(callUpdateHostHealth)
	require.Len(t, updates, 2)
	for _, u := range updates {
		require.Equal(t, HostHealthy, u.Health)
	}

	removes := h.callsOfKind(callRemoveHosts)
	require.Empty(t, removes, "no hosts to remove on initial apply")
}

// TestHostSet_ReapplyIdentical verifies that reapplying the same snapshot is a no-op.
func TestHostSet_ReapplyIdentical(t *testing.T) {
	h := &recordingHandle{}
	s := NewHostSet[string](h)

	snap := HostSnapshot[string]{
		"a": {Address: "127.0.0.1:8001"},
	}
	s.Apply(snap)
	h.Reset()

	// Apply the exact same snapshot again.
	s.Apply(snap)

	require.Empty(t, h.callsOfKind(callAddHosts), "no AddHosts on identical re-apply")
	require.Empty(t, h.callsOfKind(callRemoveHosts), "no RemoveHosts on identical re-apply")
	require.Empty(t, h.callsOfKind(callUpdateHostHealth), "no UpdateHostHealth on identical re-apply")
}

// TestHostSet_AddBeforePublishBeforeRemove verifies call ordering.
func TestHostSet_AddBeforePublishBeforeRemove(t *testing.T) {
	// We verify ordering indirectly: AddHosts is called before RemoveHosts.
	h := &recordingHandle{}
	s := NewHostSet[string](h)

	snap1 := HostSnapshot[string]{"a": {Address: "10.0.0.1:80"}}
	s.Apply(snap1)

	oldPtr, ok := s.Get("a")
	require.True(t, ok)

	h.Reset()

	// Change address for key "a".
	snap2 := HostSnapshot[string]{"a": {Address: "10.0.0.2:80"}}
	s.Apply(snap2)

	calls := h.Calls()
	require.GreaterOrEqual(t, len(calls), 2)

	// AddHosts must come before RemoveHosts in the call log.
	addIdx, removeIdx := -1, -1
	for i, c := range calls {
		switch c.Kind {
		case callAddHosts:
			addIdx = i
		case callRemoveHosts:
			removeIdx = i
		}
	}
	require.NotEqual(t, -1, addIdx, "AddHosts must be called")
	require.NotEqual(t, -1, removeIdx, "RemoveHosts must be called")
	require.Less(t, addIdx, removeIdx, "AddHosts must come before RemoveHosts")

	// The removed host should be the old pointer.
	removed := h.callsOfKind(callRemoveHosts)
	require.Len(t, removed, 1)
	require.Contains(t, removed[0].Hosts, oldPtr)
}

// TestHostSet_ChangedAddress updates address and verifies new pointer is published, old removed.
func TestHostSet_ChangedAddress(t *testing.T) {
	h := &recordingHandle{}
	s := NewHostSet[string](h)

	s.Apply(HostSnapshot[string]{"a": {Address: "10.0.0.1:80"}})
	oldPtr, _ := s.Get("a")

	s.Apply(HostSnapshot[string]{"a": {Address: "10.0.0.2:80"}})

	newPtr, ok := s.Get("a")
	require.True(t, ok)
	require.NotEqual(t, oldPtr, newPtr, "new pointer published after address change")

	removes := h.callsOfKind(callRemoveHosts)
	require.Len(t, removes, 1)
	require.Contains(t, removes[0].Hosts, oldPtr)
}

// TestHostSet_RemovedKey verifies the host is removed after the new snapshot is published.
func TestHostSet_RemovedKey(t *testing.T) {
	h := &recordingHandle{}
	s := NewHostSet[string](h)

	s.Apply(HostSnapshot[string]{
		"a": {Address: "10.0.0.1:80"},
		"b": {Address: "10.0.0.2:80"},
	})
	oldPtrB, _ := s.Get("b")

	h.Reset()
	// Remove "b" from the snapshot.
	s.Apply(HostSnapshot[string]{"a": {Address: "10.0.0.1:80"}})

	_, ok := s.Get("b")
	require.False(t, ok, "b must no longer be in the published map")

	removes := h.callsOfKind(callRemoveHosts)
	require.Len(t, removes, 1)
	require.Contains(t, removes[0].Hosts, oldPtrB)
}

// TestHostSet_MetadataChangeForceReAdd verifies that a metadata change triggers a re-add.
func TestHostSet_MetadataChangeForceReAdd(t *testing.T) {
	h := &recordingHandle{}
	s := NewHostSet[string](h)

	snap1 := HostSnapshot[string]{"a": {Address: "10.0.0.1:80", Metadata: map[string]string{"sni": "old.example"}}}
	s.Apply(snap1)
	oldPtr, _ := s.Get("a")

	h.Reset()
	snap2 := HostSnapshot[string]{"a": {Address: "10.0.0.1:80", Metadata: map[string]string{"sni": "new.example"}}}
	s.Apply(snap2)

	newPtr, _ := s.Get("a")
	require.NotEqual(t, oldPtr, newPtr, "metadata change must produce a new HostPtr")
	require.NotEmpty(t, h.callsOfKind(callAddHosts))
	require.NotEmpty(t, h.callsOfKind(callRemoveHosts))
}

// TestHostSet_GetReturnsPublishedPointer verifies Get returns the current HostPtr.
func TestHostSet_GetReturnsPublishedPointer(t *testing.T) {
	h := &recordingHandle{}
	s := NewHostSet[string](h)

	s.Apply(HostSnapshot[string]{"x": {Address: "1.2.3.4:9000"}})
	ptr, ok := s.Get("x")
	require.True(t, ok)
	require.NotNil(t, ptr)
}

// TestHostSet_EntryReturnsSpec verifies Entry returns the full HostEntry including spec.
func TestHostSet_EntryReturnsSpec(t *testing.T) {
	h := &recordingHandle{}
	s := NewHostSet[string](h)

	spec := HostSpec{Address: "1.2.3.4:9000", Hostname: "example.com", Weight: 10}
	s.Apply(HostSnapshot[string]{"x": spec})

	e, ok := s.Entry("x")
	require.True(t, ok)
	require.Equal(t, spec.Address, e.Spec.Address)
	require.Equal(t, spec.Hostname, e.Spec.Hostname)
	require.Equal(t, spec.Weight, e.Spec.Weight)
}

// TestHostSet_CurrentReturnsCopy verifies Current returns an independent copy.
func TestHostSet_CurrentReturnsCopy(t *testing.T) {
	h := &recordingHandle{}
	s := NewHostSet[string](h)

	s.Apply(HostSnapshot[string]{"a": {Address: "10.0.0.1:80"}, "b": {Address: "10.0.0.2:80"}})

	m := s.Current()
	require.Len(t, m, 2)

	// Mutate the returned map — the published snapshot must not change.
	delete(m, "a")
	m2 := s.Current()
	require.Len(t, m2, 2, "Current must return independent copies")
}

// TestHostSet_GetMissing verifies Get returns false for unknown keys.
func TestHostSet_GetMissing(t *testing.T) {
	h := &recordingHandle{}
	s := NewHostSet[string](h)

	ptr, ok := s.Get("missing")
	require.False(t, ok)
	require.Nil(t, ptr)
}

// TestHostSet_MetadataPassedToAddHosts verifies Metadata is forwarded correctly.
func TestHostSet_MetadataPassedToAddHosts(t *testing.T) {
	h := &recordingHandle{}
	s := NewHostSet[string](h)

	s.Apply(HostSnapshot[string]{
		"a": {Address: "10.0.0.1:80", Metadata: map[string]string{"sni": "peer.example"}},
	})

	adds := h.callsOfKind(callAddHosts)
	require.Len(t, adds, 1)
	require.Len(t, adds[0].Specs, 1)
	require.Equal(t, "peer.example", adds[0].Specs[0].Metadata["sni"])
}
