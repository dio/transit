# up

Package `up` is the user-facing API for Transit HTTP filters. Import it from
handler packages. The shared library entrypoint additionally blank-imports
`down/abi_impl` to link the Envoy ABI exports — but that import belongs only
there, not in reusable handler packages.

## Registration

Register a filter from your shared library entrypoint:

```go
package main

import (
	_ "github.com/dio/transit/down/abi_impl"
	_ "github.com/dio/transit/examples/hello"
)

func main() {}
```

And in your handler package:

```go
func init() {
	up.Register("hello", Handler)
}
```

The registered name must match `filter_name` in the Envoy dynamic module filter
config.

## Registration forms

| Use case | Register with |
| --- | --- |
| Request headers only | `up.Register(name, onReq)` |
| Request and response headers | `up.RegisterWithResponse(name, onReq, onResp)` |
| Streaming request body chunks | `up.RegisterWithBody(name, onReq, onBody, onResp)` |
| Buffered body reads or replacement | `up.RegisterWithMutableBody(name, onReq, onBody, onResp)` |
| Per-config metrics setup | `up.RegisterWithConfig(name, configure, onReq, onResp)` |
| Background goroutines tied to filter config | `up.RegisterWithGroup(name, group, onReq)` |
| Access logs after the stream is complete | `up.RegisterAccessLogger(name, factory)` |
| Cluster Extension modules | `up.RegisterCluster(name, factory)` |
| LB Policy modules | `up.RegisterLBPolicy(name, factory)` |

Start with `up.Register`. Move to a larger form only when the filter needs
another stream phase or lifecycle hook.

## Handler shape

Request handlers always have this signature:

```go
func(w *up.Writer, r *up.Request)
```

`r` gives you the parsed request: `Method`, `Path`, `Host`, `FilterName`,
and individual headers via `r.Header(name)` or all headers via `r.AllHeaders()`.

`w` gives you the actions available from a filter.

## Writer actions

- `w.Log(level, format, args...)` — log through Envoy at a given severity
- `w.SendLocalResponse(status, body, headers...)` — send an immediate response and stop the filter chain
- `w.SetRequestHeader(key, value)` — add or replace a request header
- `w.RemoveRequestHeader(key)` — remove a request header
- `w.SetResponseHeader(key, value)` — add or replace a response header
- `w.SetFilterState(key, value)` — write Envoy filter state
- `w.SetUpstreamOverrideHost(host)` — override the upstream host for this request
- `w.AddSpanTag(key, value)` — annotate the active tracing span
- `w.IncrementCounter(id, delta)` — increment an Envoy counter defined at config time
- `w.IncrementGauge(id, delta)` — increment an Envoy gauge defined at config time
- `w.DecrementGauge(id, delta)` — decrement an Envoy gauge defined at config time
- `w.SetGauge(id, value)` — set an Envoy gauge to an absolute value
- `w.RecordHistogram(id, value)` — record a histogram observation
- `w.GetBufferedBody()` — read the buffered request body (requires `RegisterWithMutableBody`)
- `w.SetBufferedBody(body)` — replace the buffered request body
- `w.HTTPCallout(req, callback)` — make an async HTTP callout to an Envoy cluster

## Async HTTP callouts

Use `w.HTTPCallout` when the handler needs to call an upstream before deciding
the response. The callout pauses the request stream. The callback receives the
callout result and response body as borrowed `shared.UnsafeEnvoyBuffer` values;
copy them with `ToString()` or `ToBytes()` before the callback returns.

For fire-and-forget work that does not need to send a local response, use
`w.Go` plus `w.Do` to queue mutations and return `Continue` directly. See
`docs/async-http-callouts.md` for the detailed decision guide.

## Middleware

`up.Chain` composes `HandlerFunc` values with middleware:

```go
up.Register("myfilter", up.Chain(handler, authMiddleware, loggingMiddleware))
```

Middleware runs left-to-right; the first entry is outermost.

## Upstream HTTP filters

Filters registered with `up.Register` run on the listener (downstream) side by
default. To run a filter on the upstream side, configure the dynamic module
filter under `HttpProtocolOptions.http_filters` on a cluster.

### Body buffering difference vs downstream

Downstream filters using `WithMutableBody` (buffered mode) rely on
`BufferedRequestBody()` to read the accumulated request body. Envoy pre-fills
this buffer across `StopAndBuffer` calls, so by the time the handler sees
`endOfStream=true`, `BufferedRequestBody()` holds all data.

**Upstream filter chains behave differently:** Envoy does not pre-fill the
buffer when `endOfStream=true` arrives on the first body call (i.e. when no
prior `StopAndBuffer` has occurred — common for small single-frame requests).
In that case the body is only available in the `body` argument passed to
`OnRequestBody`, not in `BufferedRequestBody()`.

The `up` framework handles this transparently: when `WithMutableBody` is used,
`OnRequestBody` falls back to the `body` argument when `BufferedRequestBody()`
returns an empty buffer, so `BodyChunk.Data` always contains the correct body
regardless of which filter chain the filter runs in.

## Log levels

`up.LogTrace`, `up.LogDebug`, `up.LogInfo`, `up.LogWarn`, `up.LogError`,
`up.LogCritical` map directly to Envoy log severity levels.
