package pending

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestOnResolve_firesOnFirstResolve(t *testing.T) {
	p := New()
	var got Result
	var calls atomic.Int32

	p.OnResolve(func(r Result) {
		calls.Add(1)
		got = r
	})

	if !p.Resolve(Result{Provider: "openai_direct", Model: "gpt"}) {
		t.Fatal("first Resolve must win")
	}
	if calls.Load() != 1 {
		t.Errorf("callback fired %d times, want 1", calls.Load())
	}
	if got.Provider != "openai_direct" {
		t.Errorf("got.Provider = %q", got.Provider)
	}
}

func TestOnResolve_firesInlineIfAlreadyResolved(t *testing.T) {
	p := New()
	p.Resolve(Result{Provider: "openai_direct"})

	var got Result
	var calls atomic.Int32
	p.OnResolve(func(r Result) {
		calls.Add(1)
		got = r
	})

	if calls.Load() != 1 {
		t.Errorf("callback fired %d times, want 1", calls.Load())
	}
	if got.Provider != "openai_direct" {
		t.Errorf("got.Provider = %q", got.Provider)
	}
}

func TestOnResolve_secondResolveDoesNotRefire(t *testing.T) {
	p := New()
	var calls atomic.Int32
	p.OnResolve(func(r Result) { calls.Add(1) })

	p.Resolve(Result{Provider: "a"})
	if got := p.Resolve(Result{Provider: "b"}); got {
		t.Error("second Resolve returned true; want false")
	}
	if calls.Load() != 1 {
		t.Errorf("callback fired %d times, want 1", calls.Load())
	}
}

func TestOnResolve_secondRegistrationOnFiredPendingIsNoop(t *testing.T) {
	p := New()
	first := atomic.Int32{}
	p.OnResolve(func(r Result) { first.Add(1) })
	p.Resolve(Result{Provider: "x"})

	second := atomic.Int32{}
	p.OnResolve(func(r Result) { second.Add(1) })

	if first.Load() != 1 {
		t.Errorf("first callback fired %d times, want 1", first.Load())
	}
	if second.Load() != 0 {
		t.Errorf("second callback fired %d times, want 0 (already-fired Pending)", second.Load())
	}
}

func TestOnResolve_replaceBeforeResolve(t *testing.T) {
	// Last-writer-wins for OnResolve when called multiple times on an
	// unresolved Pending. Documents the intentional behavior.
	p := New()
	first := atomic.Int32{}
	second := atomic.Int32{}
	p.OnResolve(func(r Result) { first.Add(1) })
	p.OnResolve(func(r Result) { second.Add(1) })

	p.Resolve(Result{Provider: "x"})

	if first.Load() != 0 {
		t.Errorf("first callback fired %d times, want 0 (replaced)", first.Load())
	}
	if second.Load() != 1 {
		t.Errorf("second callback fired %d times, want 1", second.Load())
	}
}

func TestPending_concurrentResolveOnlyOneWins(t *testing.T) {
	p := New()
	const N = 32
	var calls atomic.Int32
	p.OnResolve(func(r Result) { calls.Add(1) })

	var wins atomic.Int32
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			if p.Resolve(Result{Provider: "x"}) {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()

	if wins.Load() != 1 {
		t.Errorf("Resolve returned true %d times, want 1", wins.Load())
	}
	if calls.Load() != 1 {
		t.Errorf("callback fired %d times, want 1", calls.Load())
	}
}
