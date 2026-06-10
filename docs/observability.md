# Observability and Per-Request State in Transit

Transit provides four mechanisms for emitting signals and passing data around.
They are not interchangeable — each has a distinct scope, lifetime, and set of
readers. Picking the wrong one results in silent no-ops (metrics that never
appear) or missing data (routing decisions made with stale values).

---

## Quick-reference table

| Mechanism | Written by | Read by | Lifetime | Structured? |
|---|---|---|---|---|
| `w.Log` | HTTP filter (any phase) | Envoy log file | none | no |
| `w.IncrementCounter` / `w.IncrementGauge` / `w.RecordHistogram` | HTTP filter (any phase) | Envoy stats sink | config-defined | labels only |
| `w.SetFilterState` | HTTP filter (request phase only) | Cluster Extension (`ctx.GetFilterState`) | per-stream | no (bytes) |
| `w.SetMetadata` / `w.GetMetadataString` etc. | HTTP filter (any phase) | Same/downstream filters, Envoy access log (`%DYNAMIC_METADATA%`) | per-stream | yes (typed) — see `docs/metadata.md` |

---

## Logging: `w.Log`

```go
w.Log(up.LogInfo, "routing request to model=%s latency_ms=%d", model, ms)
```

Writes to Envoy's application log (stdout / log file depending on Envoy config).
Use for debugging and operational insight. Not per-request structured data —
log lines are free-form strings and are not queryable as fields in an access
log pipeline.

**When to use:** tracing execution flow, debugging unexpected routing, emitting
one-off warnings. Not a substitute for metrics.

---

## Metrics: counters, gauges, and histograms

All metrics must be defined once at config time via `ConfigHandle`, stored in
a package variable, and used from any request phase via `*Writer`:

```go
var (
    reqCounter  up.MetricID
    activeGauge up.MetricID
    latencyHist up.MetricID
)

func init() {
    up.RegisterWithConfig("my-filter",
        func(h up.ConfigHandle) error {
            var err error
            reqCounter, err = h.DefineCounter("my_filter_requests_total")
            if err != nil { return err }
            activeGauge, err = h.DefineGauge("my_filter_active_requests")
            if err != nil { return err }
            latencyHist, err = h.DefineHistogram("my_filter_latency_ms")
            return err
        },
        onRequest, onResponse,
    )
}

func onRequest(w *up.Writer, r *up.Request) {
    w.IncrementCounter(reqCounter, 1)
    w.IncrementGauge(activeGauge, 1)
}

func onResponse(w *up.Writer, chunk *up.ResponseChunk) {
    if chunk.EndStream {
        w.DecrementGauge(activeGauge, 1)
        w.RecordHistogram(latencyHist, computeLatency())
    }
}
```

For labeled metrics, define tag keys at config time and pass values in the
same order when recording:

```go
var tokenUsage up.MetricID

func init() {
    up.RegisterWithConfig("my-filter",
        func(h up.ConfigHandle) error {
            var err error
            tokenUsage, err = h.DefineHistogram("gen_ai.client.token.usage",
                "gen_ai.operation.name",
                "gen_ai.provider.name",
                "gen_ai.token.type",
            )
            return err
        },
        onRequest, onResponse,
    )
}

func onResponse(w *up.Writer, chunk *up.ResponseChunk) {
    if chunk.EndStream {
        w.RecordHistogramLabels(tokenUsage, 42, "chat", "openai", "input")
    }
}
```

The label values must match the metric's tag-key order and count. Keep labels
low-cardinality; do not use request IDs, user IDs, raw paths, or API keys.

**Counter** — monotonically increasing, never decreases, reset on Envoy restart.
Good for total requests, errors, retries.

**Gauge** — current value, can go up and down. Good for in-flight request count,
connection pool depth, cache size.

**Histogram** — records a distribution of values. Good for latency, token counts,
body size. Envoy computes percentiles automatically.

### Request-phase vs response-phase behaviour

On the request path (queued mode), metric mutations are applied by `flush()`
before `ContinueRequest`. On the response path (direct mode), they apply
immediately inline. Both work correctly — Transit sets `directWrite=true` for
response-path Writers so metrics are never silently dropped.

See `docs/filter.md` for the response-path trap that plagued early versions.

---

## Filter state: passing data between filter and cluster extension

Filter state is Envoy's per-stream key-value store. An HTTP filter writes a
value; downstream extensions read it:

```go
// HTTP filter — request phase
func onRequest(w *up.Writer, r *up.Request) {
    target := selectTarget(r.Header("x-model"))
    w.SetFilterState("llm.target", target)
}
```

```go
// Cluster Extension — ChooseHost
func (lb *myLB) ChooseHost(h up.ClusterLBHandle, ctx up.ClusterLBContext) (up.HostPtr, *up.ClusterLBCompletion) {
    target, ok := ctx.GetFilterState("llm.target")
    if !ok { return nil, nil }
    // ... select host by target
}
```

**Key constraints:**

- Filter state can only be written from the **request phase** (`OnRequestHeaders`,
  `OnRequestBody`, or an async callout callback). Writing it from a response
  handler has no effect — the upstream routing decision was already made.
- Filter state is readable from **Cluster Extension** (`ClusterLBContext.GetFilterState`).
  It is NOT available from LB Policy (`LBContext` has no `GetFilterState`). If
  you need arbitrary per-request state to influence host selection, use a Cluster
  Extension.
- Values are raw bytes (`string` in the Go API). No schema, no types.
- Filter state persists for the lifetime of the HTTP stream; it is not visible
  to subsequent, unrelated requests.

**When to use filter state:** routing intent — write a model name, tenant ID,
or backend endpoint in the HTTP filter and read it in the Cluster Extension's
`ChooseHost`. It is the right mechanism when the HTTP filter has already done
the decision work (auth, header inspection, business logic) and the cluster
extension just needs the result.

**When NOT to use filter state:** if you only need to influence routing via a
single host address, use `w.SetUpstreamOverrideHost` instead — it skips the
filter-state round-trip entirely and works with both LB Policy and Cluster
Extension. Use filter state when the routing key is too complex to express as a
single host address string.

---

## Decision guide

**"I want to debug a specific request."**
→ `w.Log` with `LogDebug`. Ship to Envoy stdout.

**"I want to count how many requests hit my filter."**
→ `h.DefineCounter` in `ConfigFunc`, `w.IncrementCounter` in the handler.

**"I want to know how many requests are in flight right now."**
→ `h.DefineGauge`; `w.IncrementGauge` on request, `w.DecrementGauge` on
response `EndStream`.

**"I want latency or token-count percentiles in my stats sink."**
→ `h.DefineHistogram`, `w.RecordHistogram` on response `EndStream`.

**"My HTTP filter decided which backend to call; I need the cluster extension to act on it."**
→ `w.SetFilterState("key", value)` in the HTTP filter; `ctx.GetFilterState("key")`
in `ChooseHost`.

**"I want routing to a specific host without writing a cluster extension."**
→ `w.SetUpstreamOverrideHost(host, strict)` — readable by both LB Policy
(`ctx.GetOverrideHost`) and Cluster Extension.

**"I want per-request fields in Envoy access logs."**
→ `w.SetMetadata(ns, key, value)` — Envoy access log formatter reads it via
`%DYNAMIC_METADATA(namespace:key)%`. See `docs/structured-log-fields.md` for
the full two-track guide (process log vs access log, YAML config, pitfalls).
