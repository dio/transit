package up

import (
	"context"
	"fmt"
	"runtime"
	"sync"
)

// Group manages a set of background goroutines sharing a common lifecycle.
// All goroutines start together and stop together when [Group.Stop] is called.
//
// Register goroutines with [Group.Add] or [Group.AddGoroutine], then call
// [Group.Start] exactly once. Call [Group.Stop] from your filter factory's
// OnDestroy (done automatically when registered via [RegisterWithGroup]).
type Group struct {
	actors []groupActor
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
}

type groupActor struct {
	execute func() error
	stop    func()
}

// NewGroup creates a new Group ready for actor registration.
func NewGroup() *Group {
	ctx, cancel := context.WithCancel(context.Background())
	return &Group{ctx: ctx, cancel: cancel}
}

// Add registers an actor with explicit execute and stop functions.
// execute blocks until the actor finishes; it runs in a background goroutine.
// stop must cause execute to return promptly — it is called from another goroutine.
// Panics inside execute are recovered rather than crashing the process.
func (g *Group) Add(execute func() error, stop func()) {
	g.actors = append(g.actors, groupActor{
		execute: groupWrapPanic(execute),
		stop:    stop,
	})
}

// AddGoroutine registers a context-aware background function.
// The context is cancelled automatically when [Group.Stop] is called.
//
//	g.AddGoroutine(func(ctx context.Context) {
//	    t := time.NewTicker(30 * time.Second)
//	    defer t.Stop()
//	    for {
//	        select {
//	        case <-ctx.Done(): return
//	        case <-t.C: doWork()
//	        }
//	    }
//	})
func (g *Group) AddGoroutine(fn func(ctx context.Context)) {
	ctx := g.ctx
	g.Add(
		func() error { fn(ctx); return nil },
		func() {}, // context cancellation is the stop signal
	)
}

// Start launches all registered goroutines. Call exactly once after all
// actors are registered. If any actor finishes, Stop is called automatically.
func (g *Group) Start() {
	if len(g.actors) == 0 {
		return
	}
	errc := make(chan struct{}, len(g.actors))
	for _, a := range g.actors {
		g.wg.Add(1)
		go func() {
			defer g.wg.Done()
			a.execute() //nolint:errcheck
			errc <- struct{}{}
		}()
	}
	go func() {
		<-errc
		g.Stop()
	}()
}

// Stop cancels the shared context, interrupts all actors, and waits for them
// to finish. Safe to call multiple times; only the first call has effect.
func (g *Group) Stop() {
	g.once.Do(func() {
		g.cancel()
		for _, a := range g.actors {
			a.stop()
		}
		g.wg.Wait()
	})
}

func groupWrapPanic(fn func() error) func() error {
	return func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				buf := make([]byte, 4096)
				n := runtime.Stack(buf, false)
				err = fmt.Errorf("panic: %v\n%s", r, buf[:n])
			}
		}()
		return fn()
	}
}
