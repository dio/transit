package up

import (
	"sync"
	"testing"

	"github.com/dio/transit/down"
	"github.com/dio/transit/up/testutil"
)

// newTestWriter returns a Writer and its backing FakeFilterHandle.
// directWrite=true (NewWriter) means mutations are applied to the handle
// immediately — no flush() cycle needed in unit tests.
func newTestWriter(t *testing.T) (*Writer, *testutil.FakeFilterHandle) {
	t.Helper()
	h := testutil.NewFilterHandle()
	w := NewWriter(h)
	return w, h
}

// TestStreamObject_writerSetGet: Writer.SetStreamObject followed by
// Writer.GetStreamObject on the same stream returns the stored value.
func TestStreamObject_writerSetGet(t *testing.T) {
	w, _ := newTestWriter(t)
	defer func() { dropBag(w.f.streamObjectNonce) }()

	type payload struct{ x int }
	w.SetStreamObject("k", &payload{x: 42})

	v, ok := w.GetStreamObject("k")
	if !ok {
		t.Fatal("GetStreamObject returned false for a key that was Set")
	}
	got, ok := v.(*payload)
	if !ok {
		t.Fatalf("type assertion failed: %T", v)
	}
	if got.x != 42 {
		t.Errorf("x = %d, want 42", got.x)
	}
}

// TestStreamObject_writerToClusterLBContext: a value Set via Writer is
// readable through FakeClusterLBContext.GetStreamObject on the same stream.
func TestStreamObject_writerToClusterLBContext(t *testing.T) {
	w, h := newTestWriter(t)
	defer func() { dropBag(w.f.streamObjectNonce) }()

	type payload struct{ model string }
	w.SetStreamObject("pending", &payload{model: "gpt-4o"})

	// The nonce was written to h's filter state by getOrCreateBag via the
	// directWrite path (NewWriter flushes filter-state to the handle).
	ctx := testutil.NewFakeClusterLBContext(h)

	v, ok := ctx.GetStreamObject("pending")
	if !ok {
		t.Fatal("ClusterLBContext.GetStreamObject returned false")
	}
	got := v.(*payload)
	if got.model != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o", got.model)
	}
}

// TestStreamObject_onStreamFinalizedCanRead: after Set, a value stored via
// Writer is still readable when an OnStreamFinalized callback fires, proving
// the drain order: user OnStreamComplete → user OnStreamFinalized → dropBag.
//
// This test simulates the ordering by directly calling the stream lifecycle:
//  1. requestHandler calls SetStreamObject.
//  2. onStreamComplete user callback runs (reads the bag — must succeed).
//  3. finalizedLogger.OnLog fires (simulated: we call dropBag after) — bag
//     must still be present when the B callback reads it.
//  4. dropBag fires — bag must be gone.
func TestStreamObject_onStreamFinalizedCanRead(t *testing.T) {
	h := testutil.NewFilterHandle()

	type payload struct{ val string }
	var readDuringComplete any
	var readDuringFinalized any

	// Build the filter directly so we can drive OnStreamComplete.
	// Use a filter with both onStreamComplete and onStreamFinalized so the
	// drain is deferred to the finalized path.
	f := &filter{
		handle: h,
		onStreamComplete: func(ctx *any) {
			// User OnStreamComplete: bag must still be live.
			if ctx != nil {
				if wInner, ok := (*ctx).(*Writer); ok {
					readDuringComplete, _ = wInner.GetStreamObject("obj")
				}
			}
		},
		// onStreamFinalized set below via the entry path.
	}
	w := &Writer{f: f}

	// Simulate the filter storing an object.
	w.SetStreamObject("obj", &payload{val: "hello"})
	nonce := f.streamObjectNonce
	if nonce == "" {
		t.Fatal("nonce not set after SetStreamObject")
	}

	// Set a context slot so onStreamComplete can access the Writer.
	f.context = w

	// Simulate stream tear-down with B callback.
	// We drive OnStreamComplete directly; onStreamFinalized fires after.
	var finalizedBagAlive bool
	manualFinalizedFn := OnStreamFinalizedFunc(func(_ *any, _ FinalizedInfo) {
		// Must be able to read bag here (before dropBag).
		_, alive := lookupBag(nonce)
		finalizedBagAlive = alive
		readDuringFinalized, _ = lookupBag(nonce)
	})

	// Wire a streamFinalizedEntry so that OnLog path drains the bag.
	f.onStreamFinalized = manualFinalizedFn
	fakeKey := "test\x00stream1"
	putFinalizedEntry(fakeKey, &streamFinalizedEntry{
		fn:                manualFinalizedFn,
		ctx:               &f.context,
		streamObjectNonce: &f.streamObjectNonce,
	})
	f.finalizedKey = fakeKey

	// Fire OnStreamComplete.
	f.onStreamComplete = func(ctx *any) {
		// Bag must still be live at this point.
		_, ok := lookupBag(nonce)
		if !ok {
			t.Error("bag already drained before onStreamComplete ran")
		}
	}
	f.OnStreamComplete()

	// After OnStreamComplete, bag must still be alive (drain is deferred to OnLog).
	if _, ok := lookupBag(nonce); !ok {
		t.Error("bag was drained in OnStreamComplete but onStreamFinalized configured — should wait for OnLog")
	}

	// Simulate finalizedLogger.OnLog firing: take entry and call fn, then drop bag.
	entry, ok := takeFinalizedEntry(fakeKey)
	if !ok {
		t.Fatal("finalized entry was already consumed before simulated OnLog")
	}
	entry.fn(entry.ctx, FinalizedInfo{})
	if entry.streamObjectNonce != nil {
		dropBag(*entry.streamObjectNonce)
	}

	// Now the bag must be gone.
	if _, ok := lookupBag(nonce); ok {
		t.Error("bag still alive after OnLog ran — dropBag should have cleaned it up")
	}
	if !finalizedBagAlive {
		t.Error("bag was not alive when OnStreamFinalized callback ran")
	}
	_ = readDuringComplete
	_ = readDuringFinalized
}

// TestStreamObject_twoPhasesSingleNonce: SetStreamObject called from two
// different handler phases (simulating header + body) produces ONE nonce; both
// values end up in the same bag.
func TestStreamObject_twoPhasesSingleNonce(t *testing.T) {
	w, _ := newTestWriter(t)
	defer func() { dropBag(w.f.streamObjectNonce) }()

	// Phase 1 (header handler).
	w.SetStreamObject("phase1", "header-value")
	nonce1 := w.f.streamObjectNonce
	if nonce1 == "" {
		t.Fatal("nonce not set after first SetStreamObject")
	}

	// Phase 2 (body handler — same Writer/filter pair). No manual flush needed:
	// getOrCreateBag fast-paths on f.streamObjectNonce != "".
	w.SetStreamObject("phase2", "body-value")
	nonce2 := w.f.streamObjectNonce

	if nonce1 != nonce2 {
		t.Errorf("two SetStreamObject calls produced different nonces: %q vs %q", nonce1, nonce2)
	}

	v1, ok1 := w.GetStreamObject("phase1")
	v2, ok2 := w.GetStreamObject("phase2")
	if !ok1 || v1 != "header-value" {
		t.Errorf("phase1 = (%v, %v)", v1, ok1)
	}
	if !ok2 || v2 != "body-value" {
		t.Errorf("phase2 = (%v, %v)", v2, ok2)
	}
}

// TestStreamObject_getOnNeverSetReturnsNil: GetStreamObject on a stream that
// never called SetStreamObject returns (nil, false) with no allocation.
func TestStreamObject_getOnNeverSetReturnsNil(t *testing.T) {
	w, _ := newTestWriter(t)

	v, ok := w.GetStreamObject("anything")
	if ok {
		t.Error("expected false, got true")
	}
	if v != nil {
		t.Errorf("expected nil, got %v", v)
	}
	if w.f.streamObjectNonce != "" {
		t.Error("GetStreamObject must not mint a nonce")
	}
}

// TestStreamObject_drainAfterTeardown: after the stream tears down, the bag
// entry is gone from the process-wide map. Mirrors TestRegistry_baseline*
// teardown classes: normal end, local reply (foreign), and concurrent load.
func TestStreamObject_drainAfterTeardown(t *testing.T) {
	t.Run("normal_end", func(t *testing.T) {
		w, _ := newTestWriter(t)
		w.SetStreamObject("k", "v")
		nonce := w.f.streamObjectNonce

		// Verify bag exists before teardown.
		if _, ok := down.LookupStreamObjectBag(nonce); !ok {
			t.Fatal("bag not found before teardown")
		}

		// Simulate OnStreamComplete (filter without onStreamFinalized).
		f := w.f
		f.OnStreamComplete()

		if _, ok := down.LookupStreamObjectBag(nonce); ok {
			t.Error("bag still present after OnStreamComplete — should have been drained")
		}
	})

	t.Run("local_reply_foreign", func(t *testing.T) {
		// Simulates a foreign local reply: onStreamComplete runs with no body.
		w, _ := newTestWriter(t)
		w.SetStreamObject("key", 123)
		nonce := w.f.streamObjectNonce

		// Verify bag exists.
		if _, ok := down.LookupStreamObjectBag(nonce); !ok {
			t.Fatal("bag not found")
		}

		f := w.f
		// Attach a no-op onStreamComplete to prove it runs before dropBag.
		completedCalled := false
		f.onStreamComplete = func(_ *any) { completedCalled = true }
		f.OnStreamComplete()

		if !completedCalled {
			t.Error("onStreamComplete not called")
		}
		if _, ok := down.LookupStreamObjectBag(nonce); ok {
			t.Error("bag still present after OnStreamComplete with local reply")
		}
	})

	t.Run("concurrent_load", func(t *testing.T) {
		const N = 20
		var wg sync.WaitGroup
		nonces := make([]string, N)
		wg.Add(N)
		for i := 0; i < N; i++ {
			i := i
			go func() {
				defer wg.Done()
				w, _ := newTestWriter(t)
				w.SetStreamObject("i", i)
				nonces[i] = w.f.streamObjectNonce
				w.f.OnStreamComplete()
			}()
		}
		wg.Wait()

		for i, nonce := range nonces {
			if nonce == "" {
				t.Errorf("goroutine %d: nonce is empty", i)
				continue
			}
			if _, ok := down.LookupStreamObjectBag(nonce); ok {
				t.Errorf("goroutine %d: bag still present after OnStreamComplete", i)
			}
		}
	})

	t.Run("client_abort_before_body", func(t *testing.T) {
		// Simulate: SetStreamObject called in header handler; stream torn down
		// before body handler runs. OnStreamComplete must drain the bag.
		w, _ := newTestWriter(t)
		w.SetStreamObject("header-phase", true)
		nonce := w.f.streamObjectNonce

		// Bag must exist.
		if _, ok := down.LookupStreamObjectBag(nonce); !ok {
			t.Fatal("bag not found after header-phase Set")
		}

		// Stream tears down — body handler never runs.
		w.f.OnStreamComplete()

		if _, ok := down.LookupStreamObjectBag(nonce); ok {
			t.Error("bag still present after client-abort teardown")
		}
	})
}

// TestStreamObject_bagCountStaysStable: repeated Set+teardown cycles do not
// grow the process-wide bag registry. Validates no structural leaks.
func TestStreamObject_bagCountStaysStable(t *testing.T) {
	baseline := down.StreamObjectBagCount()

	const N = 5
	for i := 0; i < N; i++ {
		w, _ := newTestWriter(t)
		w.SetStreamObject("x", i)
		w.f.OnStreamComplete()
	}

	after := down.StreamObjectBagCount()
	if after != baseline {
		t.Errorf("bag count changed: baseline=%d after=%d", baseline, after)
	}
}
