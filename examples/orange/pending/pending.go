// Package pending is the single-shot async handoff primitive for orange.
//
// It provides [Pending], a CAS-based one-shot async result carrier with an
// [OnResolve] callback. Two extensions on the same stream (classify and
// hostpick) communicate through it: classify resolves the Pending after
// parsing the request body, and hostpick installs an OnResolve callback to
// learn the result and complete host selection.
//
// The cross-extension handoff (passing the *Pending itself from classify to
// hostpick) is done via the SDK's Primitive A typed per-stream object store:
// classify stores the *Pending via Writer.SetStreamObject; hostpick reads it
// via ClusterLBContext.GetStreamObject. There is no process-wide registry.
package pending

import "sync"

// Result is what classify resolves a Pending with.
//
// Err is the orange.* error code (e.g. "orange.model_required",
// "orange.model_not_found"). When Err is set, Provider/Kind/Model are
// undefined; hostpick will Complete the host selection with a nil host and
// that string as errDetail, but classify has already sent a local response so
// the stream is on its way to closing anyway.
type Result struct {
	Provider string // selected provider name, e.g. "openai_direct"
	Kind     string // provider kind, e.g. "openai"
	Model    string
	Err      string
}

// Pending is the single-shot async handoff for one in-flight request.
//
// Resolution fires an optional callback registered via OnResolve. The
// callback runs at most once on whichever thread calls Resolve first; if
// Resolve has already happened by the time OnResolve is called, the callback
// fires inline on the OnResolve caller's thread instead.
type Pending struct {
	mu    sync.Mutex
	res   *Result
	cb    func(Result)
	fired bool
}

// New returns an unresolved Pending.
func New() *Pending { return &Pending{} }

// Resolve publishes r. The first call wins; later calls are no-ops and return
// false. If a callback was registered via OnResolve, it fires exactly once
// inside the winning Resolve, on this goroutine, after the result is
// published.
func (p *Pending) Resolve(r Result) bool {
	p.mu.Lock()
	if p.res != nil {
		p.mu.Unlock()
		return false
	}
	p.res = &r
	cb := p.cb
	if cb != nil {
		p.fired = true
	}
	p.mu.Unlock()
	if cb != nil {
		cb(r)
	}
	return true
}

// OnResolve registers a callback to fire when the Pending resolves. The
// callback fires at most once for the lifetime of the Pending:
//   - If the Pending is unresolved, the callback is stored and will fire
//     inside the next Resolve.
//   - If the Pending is already resolved, the callback fires inline on the
//     current goroutine.
//
// Calling OnResolve again on an already-resolved-and-fired Pending is a
// no-op. Calling OnResolve again before resolution replaces the previous
// callback (last writer wins) — this is intentional so a late ChooseHost
// retry could install a fresh waiter, but in practice each Pending has a
// single hostpick consumer.
func (p *Pending) OnResolve(fn func(Result)) {
	p.mu.Lock()
	if p.fired {
		p.mu.Unlock()
		return
	}
	if p.res != nil {
		r := *p.res
		p.fired = true
		p.mu.Unlock()
		fn(r)
		return
	}
	p.cb = fn
	p.mu.Unlock()
}

// Result returns the resolved Result, or (zero, false) if not yet resolved.
func (p *Pending) Result() (Result, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.res == nil {
		return Result{}, false
	}
	return *p.res, true
}
