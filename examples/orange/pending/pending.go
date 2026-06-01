// Package pending is the process-wide token-keyed handoff between
// classify.bodyHandler (which decides the upstream after parsing the request
// body) and hostpick.ChooseHost (which has to suspend until that decision
// lands).
//
// The pattern is documented in detail in
// docs/envoy-dynamic-module-upstream-selection.md and in
// .agents/skills/transit-body-driven-cluster-routing/SKILL.md.
package pending

import (
	"sync"
	"sync/atomic"
)

// Result is what classify resolves a Pending with.
//
// Err is the orange.* error code (e.g. "orange.model_required",
// "orange.model_not_found"). When Err is set, Upstream/Provider/Model are
// undefined; hostpick will Complete the host selection with a nil host and
// that string as errDetail, but classify has already sent a local response so
// the stream is on its way to closing anyway.
type Result struct {
	Upstream string
	Provider string
	Model    string
	Err      string
}

// Pending is the single-shot async handoff for one in-flight request.
type Pending struct {
	done chan struct{}
	res  atomic.Pointer[Result]
}

// New returns an unresolved Pending. Callers usually go through Register so
// the Pending is discoverable by token.
func New() *Pending {
	return &Pending{done: make(chan struct{})}
}

// Resolve publishes r. The first call wins; later calls are no-ops and return
// false.
func (p *Pending) Resolve(r Result) bool {
	if !p.res.CompareAndSwap(nil, &r) {
		return false
	}
	close(p.done)
	return true
}

// Done returns a channel that closes when the Pending is resolved.
func (p *Pending) Done() <-chan struct{} { return p.done }

// Result returns the resolved Result, or (zero, false) if not yet resolved.
func (p *Pending) Result() (Result, bool) {
	r := p.res.Load()
	if r == nil {
		return Result{}, false
	}
	return *r, true
}

var registry sync.Map // token (string) -> *Pending

// Register associates a new Pending with token. If token is already present
// (token collision), the existing Pending is returned — Resolve is single-shot
// so the second classify run would just no-op, which is the safe behavior.
func Register(token string) *Pending {
	p := New()
	actual, loaded := registry.LoadOrStore(token, p)
	if loaded {
		return actual.(*Pending)
	}
	return p
}

// Lookup returns the Pending for token, or nil if absent.
func Lookup(token string) *Pending {
	v, ok := registry.Load(token)
	if !ok {
		return nil
	}
	return v.(*Pending)
}

// Delete removes token from the registry. Safe to call multiple times.
func Delete(token string) { registry.Delete(token) }
