package up

import "sync"

// HostResult is returned by the lookup function to resolve a host.
type HostResult struct {
	Host      HostPtr // nil → use ErrDetail
	ErrDetail string  // non-empty → complete with error
}

// SelectorObserver receives events from AsyncHostSelector.
// All nil fields are treated as no-ops.
type SelectorObserver struct {
	// OnSelected is called (on cluster main thread) when a host is chosen.
	OnSelected func(host HostPtr)
	// OnFailed is called (on cluster main thread) when the lookup returns an error.
	OnFailed func(errDetail string)
	// OnCancelled is called (on cluster main thread) when CancelHostSelection fires.
	OnCancelled func()
	// OnMissingPromise is called (on cluster main thread) when no promise is
	// found in the stream-object bag (key absent or wrong type).
	OnMissingPromise func()
}

// AsyncHostSelector[T] implements the ChooseHost + CancelHostSelection pair
// for body-driven async host selection.
//
// Usage: embed or delegate from a ClusterLB implementation.
//   - ChooseHost: call s.ChooseHost(handle, ctx)
//   - CancelHostSelection: call s.Cancel(completion)
//
// T is the decision type resolved by the body filter (e.g. a struct with
// provider name, model, error code). lookup maps a T to a HostResult.
//
// The OnResolve callback may fire from a worker goroutine; AsyncHostSelector
// always marshals completion.Complete back to the main thread via
// handle.Schedule — the canonical example of the main-thread contract.
// See .agents/skills/transit-cluster-main-thread/SKILL.md.
type AsyncHostSelector[T any] struct {
	handle ClusterHandle
	key    StreamKey[*StreamPromise[T]]
	lookup func(T) HostResult
	obs    SelectorObserver

	// cancelled tracks ChooseHost completions Envoy cancelled before they
	// completed. The scheduled completion callback consults this map on the
	// cluster main thread and skips Complete when the entry is present. Empty
	// in the happy path: entries only appear on the cancel path.
	mu        sync.Mutex
	cancelled map[*ClusterLBCompletion]struct{}
}

// NewAsyncHostSelector creates an AsyncHostSelector that reads promises from
// key and resolves hosts via lookup. handle is the ClusterHandle used for
// Schedule. obs may be zero (all nil fields are no-ops).
func NewAsyncHostSelector[T any](
	handle ClusterHandle,
	key StreamKey[*StreamPromise[T]],
	lookup func(T) HostResult,
	obs SelectorObserver,
) *AsyncHostSelector[T] {
	return &AsyncHostSelector[T]{
		handle:    handle,
		key:       key,
		lookup:    lookup,
		obs:       obs,
		cancelled: make(map[*ClusterLBCompletion]struct{}),
	}
}

// ChooseHost implements the ChooseHost half of ClusterLB. Returns (nil,
// completion) for async resolution, or (nil, nil) when no promise is found.
func (s *AsyncHostSelector[T]) ChooseHost(_ ClusterLBHandle, ctx ClusterLBContext) (HostPtr, *ClusterLBCompletion) {
	promise, ok := s.key.GetFromCtx(ctx)
	if !ok || promise == nil {
		if s.obs.OnMissingPromise != nil {
			s.obs.OnMissingPromise()
		}
		return nil, nil
	}

	completion := ctx.NewCompletion()
	promise.OnResolve(func(v T) {
		// May be invoked inline (already resolved) or later from a worker thread.
		// Either way we must run completion.Complete on the cluster main thread.
		s.handle.Schedule(func() { s.complete(completion, v) })
	})
	return nil, completion
}

// Cancel implements the CancelHostSelection half of ClusterLB. Prevents the
// pending OnResolve callback from calling Complete.
func (s *AsyncHostSelector[T]) Cancel(completion *ClusterLBCompletion) {
	s.mu.Lock()
	s.cancelled[completion] = struct{}{}
	s.mu.Unlock()
	if s.obs.OnCancelled != nil {
		s.obs.OnCancelled()
	}
}

// complete finalises a host selection on the cluster main thread. It is the
// single place that calls completion.Complete: kept here so the
// cancelled-check, the lookup and the Complete call cannot race with Cancel.
func (s *AsyncHostSelector[T]) complete(completion *ClusterLBCompletion, v T) {
	s.mu.Lock()
	_, cancelled := s.cancelled[completion]
	if cancelled {
		delete(s.cancelled, completion)
	}
	s.mu.Unlock()
	if cancelled {
		return
	}

	res := s.lookup(v)
	if res.ErrDetail != "" {
		if s.obs.OnFailed != nil {
			s.obs.OnFailed(res.ErrDetail)
		}
		completion.Complete(nil, res.ErrDetail)
		return
	}
	if s.obs.OnSelected != nil {
		s.obs.OnSelected(res.Host)
	}
	completion.Complete(res.Host, "")
}
