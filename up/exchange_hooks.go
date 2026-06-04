package up

import "sync"

// ExchangeHooks[T] carries typed callbacks for the three phases of an exchange.
type ExchangeHooks[T any] struct {
	// OnRequest initializes and returns the per-stream accumulator.
	// Called at request headers phase.
	OnRequest func(w *Writer, r *Request) T

	// OnResponse is called at response headers (chunk.StatusCode != 0, chunk.Data == nil)
	// and optionally response body (chunk.Data != nil). If nil, response phase is skipped.
	OnResponse func(st T, w *Writer, chunk *ResponseChunk)

	// OnFinalized receives the accumulated state and Envoy's FinalizedInfo.
	// Delivery semantics match WithOnStreamFinalized.
	OnFinalized func(st T, info FinalizedInfo)
}

// TruncateBody returns data[:max] if len(data) > max, otherwise data unchanged.
func TruncateBody(data []byte, max int) []byte {
	if len(data) > max {
		return data[:max]
	}
	return data
}

// WithExchangeObserver returns a set of FilterOptions that wire ExchangeHooks[T]
// into a filter registration. The returned options are passed directly to up.Register.
// The SDK owns the context slot and pool lifecycle; callers never touch *any.
func WithExchangeObserver[T any](hooks ExchangeHooks[T]) []FilterOption {
	pool := &sync.Pool{New: func() any { return new(T) }}

	handlerOpt := FilterOption(func(cf *configFactory) {
		cf.handler = func(w *Writer, r *Request) {
			slot, ok := pool.Get().(*T)
			if !ok {
				slot = new(T)
			}
			*slot = hooks.OnRequest(w, r)
			*r.Context = slot
		}
	})

	opts := []FilterOption{handlerOpt}

	if hooks.OnResponse != nil {
		opts = append(opts, WithResponse(func(w *Writer, chunk *ResponseChunk) {
			if *chunk.Context == nil {
				return
			}
			slot, ok := (*chunk.Context).(*T)
			if !ok || slot == nil {
				return
			}
			hooks.OnResponse(*slot, w, chunk)
		}))
	}

	opts = append(opts, WithOnStreamFinalized(func(ctx *any, info FinalizedInfo) {
		if ctx == nil || *ctx == nil {
			return
		}
		slot, ok := (*ctx).(*T)
		if !ok || slot == nil {
			return
		}
		if hooks.OnFinalized != nil {
			hooks.OnFinalized(*slot, info)
		}
		var zero T
		*slot = zero
		pool.Put(slot)
		*ctx = nil
	}))

	return opts
}
