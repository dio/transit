package up

import (
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/down"
)

// testCompletion creates a ClusterLBCompletion wired to record the host and
// errDetail passed to Complete. completeFired is closed when Complete is called.
func testCompletion() (*ClusterLBCompletion, *HostPtr, *string, chan struct{}) {
	comp := &down.ClusterLBCompletion{}
	var gotHost HostPtr
	var gotErr string
	fired := make(chan struct{}, 1)
	comp.SetCompleteFn(func(host HostPtr, errDetail string) {
		gotHost = host
		gotErr = errDetail
		fired <- struct{}{}
	})
	return comp, &gotHost, &gotErr, fired
}

// fakeCtx is a ClusterLBContext for AsyncHostSelector tests.
// It carries an optional object bag and returns a pre-configured completion.
type fakeCtx struct {
	objects    map[string]any
	completion *ClusterLBCompletion
}

func (f *fakeCtx) GetStreamObject(key string) (any, bool) {
	v, ok := f.objects[key]
	return v, ok
}
func (f *fakeCtx) NewCompletion() *ClusterLBCompletion { return f.completion }

// Remaining ClusterLBContext no-ops.
func (f *fakeCtx) GetAllHeaders() [][2]string                                          { return nil }
func (f *fakeCtx) GetFilterState(_ string) (string, bool)                              { return "", false }
func (f *fakeCtx) GetFilterStateTyped(_ string) (string, bool)                         { return "", false }
func (f *fakeCtx) GetOverrideHost() (string, bool)                                     { return "", false }
func (f *fakeCtx) GetHeader(_ string) (string, bool)                                   { return "", false }
func (f *fakeCtx) GetDownstreamSNI() (string, bool)                                    { return "", false }
func (f *fakeCtx) ComputeHashKey() (uint64, bool)                                      { return 0, false }
func (f *fakeCtx) GetHostSelectionRetryCount() uint32                                  { return 0 }
func (f *fakeCtx) ShouldSelectAnotherHost(_ ClusterLBHandle, _ uint32, _ int) bool     { return false }

// fakeHandle is a minimal ClusterHandle that runs Schedule callbacks synchronously.
type fakeHandle struct {
	scheduled []func()
	runSync   bool // when true, Schedule executes fn() immediately
}

func newSyncHandle() *fakeHandle   { return &fakeHandle{runSync: true} }
func newDeferHandle() *fakeHandle  { return &fakeHandle{runSync: false} }

func (h *fakeHandle) Schedule(fn func()) {
	if h.runSync {
		fn()
	} else {
		h.scheduled = append(h.scheduled, fn)
	}
}
func (h *fakeHandle) runAll() {
	for _, fn := range h.scheduled {
		fn()
	}
	h.scheduled = nil
}

// Remaining ClusterHandle no-ops.
func (h *fakeHandle) AddHosts(_ []HostSpec) []HostPtr          { return nil }
func (h *fakeHandle) RemoveHosts(_ []HostPtr)                  {}
func (h *fakeHandle) UpdateHostHealth(_ HostPtr, _ HostHealth) {}
func (h *fakeHandle) FindHostByAddress(_ string) HostPtr       { return nil }
func (h *fakeHandle) PreInitComplete()                         {}

// dummyHost returns a non-nil HostPtr for test assertions.
func dummyHost() HostPtr { return HostPtr(unsafe.Pointer(&struct{}{})) }

// selectorKey is the StreamKey used in all selector tests.
var selectorKey = NewStreamKey[*StreamPromise[string]]("test.selector")

// newSelector builds a selector backed by a sync handle that resolves a string
// value to either a dummy host or an error.
func newSelector(handle *fakeHandle, obs SelectorObserver) *AsyncHostSelector[string] {
	return NewAsyncHostSelector(
		handle,
		selectorKey,
		func(v string) HostResult {
			if v == "err" {
				return HostResult{ErrDetail: "lookup.failed"}
			}
			return HostResult{Host: dummyHost()}
		},
		obs,
	)
}

// ──────────────────────────────────────────────────────────────────────────────
// Test cases
// ──────────────────────────────────────────────────────────────────────────────

// TestAsyncHostSelector_alreadyResolved verifies that a promise resolved before
// ChooseHost schedules Complete synchronously (via the sync handle).
func TestAsyncHostSelector_alreadyResolved(t *testing.T) {
	promise := NewStreamPromise[string]()
	promise.Resolve("ok")

	comp, gotHost, gotErr, fired := testCompletion()
	handle := newSyncHandle()
	sel := newSelector(handle, SelectorObserver{})
	ctx := &fakeCtx{
		objects:    map[string]any{selectorKey.Key(): promise},
		completion: comp,
	}

	host, ret := sel.ChooseHost(nil, ctx)
	require.Nil(t, host)
	require.Same(t, comp, ret)

	// Schedule ran synchronously inside ChooseHost.
	select {
	case <-fired:
	default:
		t.Fatal("Complete was not called for an already-resolved promise")
	}
	require.NotNil(t, *gotHost)
	require.Empty(t, *gotErr)
}

// TestAsyncHostSelector_laterResolved verifies that completion fires exactly
// once when the promise is resolved after ChooseHost returns.
func TestAsyncHostSelector_laterResolved(t *testing.T) {
	promise := NewStreamPromise[string]()

	comp, gotHost, gotErr, fired := testCompletion()
	handle := newSyncHandle()
	sel := newSelector(handle, SelectorObserver{})
	ctx := &fakeCtx{
		objects:    map[string]any{selectorKey.Key(): promise},
		completion: comp,
	}

	host, ret := sel.ChooseHost(nil, ctx)
	require.Nil(t, host)
	require.Same(t, comp, ret)

	// Complete must not have fired yet.
	select {
	case <-fired:
		t.Fatal("Complete fired before promise resolved")
	default:
	}

	promise.Resolve("ok")

	select {
	case <-fired:
	default:
		t.Fatal("Complete did not fire after promise resolved")
	}
	require.NotNil(t, *gotHost)
	require.Empty(t, *gotErr)

	// Resolve again must not trigger a second Complete.
	promise.Resolve("ok")
	require.Len(t, fired, 0, "Complete fired twice")
}

// TestAsyncHostSelector_cancelBeforeResolve verifies that Cancel before Resolve
// prevents Complete from firing and notifies the observer.
func TestAsyncHostSelector_cancelBeforeResolve(t *testing.T) {
	promise := NewStreamPromise[string]()

	comp, _, _, fired := testCompletion()
	handle := newSyncHandle()
	var cancelledCalled bool
	sel := newSelector(handle, SelectorObserver{
		OnCancelled: func() { cancelledCalled = true },
	})
	ctx := &fakeCtx{
		objects:    map[string]any{selectorKey.Key(): promise},
		completion: comp,
	}

	_, ret := sel.ChooseHost(nil, ctx)
	require.Same(t, comp, ret)

	sel.Cancel(comp)
	require.True(t, cancelledCalled)

	// Now resolve; complete must NOT fire.
	promise.Resolve("ok")

	select {
	case <-fired:
		t.Fatal("Complete fired after Cancel")
	default:
	}
}

// TestAsyncHostSelector_cancelAfterResolve verifies that Cancel after Complete
// is a harmless no-op (no double Complete).
func TestAsyncHostSelector_cancelAfterResolve(t *testing.T) {
	promise := NewStreamPromise[string]()

	comp, _, _, fired := testCompletion()
	handle := newSyncHandle()
	var cancelledCalled bool
	sel := newSelector(handle, SelectorObserver{
		OnCancelled: func() { cancelledCalled = true },
	})
	ctx := &fakeCtx{
		objects:    map[string]any{selectorKey.Key(): promise},
		completion: comp,
	}

	_, ret := sel.ChooseHost(nil, ctx)
	require.Same(t, comp, ret)

	// Resolve first — Complete fires.
	promise.Resolve("ok")
	select {
	case <-fired:
	default:
		t.Fatal("Complete did not fire after Resolve")
	}

	// Cancel afterwards — must not panic or double-fire.
	sel.Cancel(comp)
	require.True(t, cancelledCalled)
	require.Len(t, fired, 0, "Complete fired a second time after Cancel")
}

// TestAsyncHostSelector_missingPromise verifies that (nil, nil) is returned
// and OnMissingPromise is called when the key is absent.
func TestAsyncHostSelector_missingPromise(t *testing.T) {
	comp, _, _, fired := testCompletion()
	handle := newSyncHandle()
	var missingCalled bool
	sel := newSelector(handle, SelectorObserver{
		OnMissingPromise: func() { missingCalled = true },
	})
	ctx := &fakeCtx{
		objects:    map[string]any{}, // key absent
		completion: comp,
	}

	host, ret := sel.ChooseHost(nil, ctx)
	require.Nil(t, host)
	require.Nil(t, ret)
	require.True(t, missingCalled)
	require.Len(t, fired, 0, "Complete should not fire when promise is missing")
}

// TestAsyncHostSelector_lookupError verifies that OnFailed is called and
// completion.Complete(nil, errDetail) is invoked when lookup returns an error.
func TestAsyncHostSelector_lookupError(t *testing.T) {
	promise := NewStreamPromise[string]()

	comp, gotHost, gotErr, fired := testCompletion()
	handle := newSyncHandle()
	var failedDetail string
	sel := newSelector(handle, SelectorObserver{
		OnFailed: func(d string) { failedDetail = d },
	})
	ctx := &fakeCtx{
		objects:    map[string]any{selectorKey.Key(): promise},
		completion: comp,
	}

	_, ret := sel.ChooseHost(nil, ctx)
	require.Same(t, comp, ret)

	// Resolve with the sentinel value that makes lookup return an error.
	promise.Resolve("err")

	select {
	case <-fired:
	default:
		t.Fatal("Complete did not fire after Resolve (error path)")
	}
	require.Equal(t, "lookup.failed", failedDetail)
	require.Nil(t, *gotHost)
	require.Equal(t, "lookup.failed", *gotErr)
}

// TestAsyncHostSelector_lookupSuccess verifies that OnSelected is called and
// completion.Complete(host, "") is invoked on the happy path.
func TestAsyncHostSelector_lookupSuccess(t *testing.T) {
	promise := NewStreamPromise[string]()

	comp, gotHost, gotErr, fired := testCompletion()
	handle := newSyncHandle()
	var selectedHost HostPtr
	sel := newSelector(handle, SelectorObserver{
		OnSelected: func(h HostPtr) { selectedHost = h },
	})
	ctx := &fakeCtx{
		objects:    map[string]any{selectorKey.Key(): promise},
		completion: comp,
	}

	_, ret := sel.ChooseHost(nil, ctx)
	require.Same(t, comp, ret)

	promise.Resolve("ok")

	select {
	case <-fired:
	default:
		t.Fatal("Complete did not fire after Resolve")
	}
	require.NotNil(t, selectedHost)
	require.Equal(t, selectedHost, *gotHost)
	require.Empty(t, *gotErr)
}

// TestAsyncHostSelector_deferredSchedule verifies the deferred (async) Schedule
// path: Complete must not fire until the handle's pending fns are run.
func TestAsyncHostSelector_deferredSchedule(t *testing.T) {
	promise := NewStreamPromise[string]()

	comp, gotHost, _, fired := testCompletion()
	handle := newDeferHandle()
	sel := newSelector(handle, SelectorObserver{})
	ctx := &fakeCtx{
		objects:    map[string]any{selectorKey.Key(): promise},
		completion: comp,
	}

	_, ret := sel.ChooseHost(nil, ctx)
	require.Same(t, comp, ret)

	promise.Resolve("ok")

	// Complete must not have fired yet (Schedule is deferred).
	select {
	case <-fired:
		t.Fatal("Complete fired before scheduled fns ran")
	default:
	}

	handle.runAll()

	select {
	case <-fired:
	default:
		t.Fatal("Complete did not fire after scheduled fns ran")
	}
	require.NotNil(t, *gotHost)
}
