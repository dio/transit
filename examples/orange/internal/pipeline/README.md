# pipeline

Four Envoy extension packages that together route and translate an LLM inference request from a client to the correct upstream provider.

## Packages

| Package | Extension type | Role |
|---------|---------------|------|
| `match` | Downstream HTTP filter | Parses the request body for `model`, resolves the upstream |
| `pick`  | Cluster LB extension  | Waits for `match`'s decision, selects the upstream host |
| `adapt` | Upstream HTTP filter  | Translates request/response headers and body, injects auth |
| `meter` | Response observer     | Extracts LLM token counts, emits Envoy counters + metadata |

## Request flow

```
Client POST /v1/chat/completions
        │
        ▼ match — headers phase
  Creates StreamPromise[Decision], stores in per-stream object bag (DecisionKey)
        │
        ▼ pick — ChooseHost
  Retrieves the promise via DecisionKey.GetFromCtx
  Suspends — waiting for match to resolve it
        │
        ▼ match — body phase
  Parses "model" from JSON body
  Looks up upstream in orange.yaml providers[]
  Rewrites :authority to provider host
  Writes filter state + dynamic metadata (upstream, provider, model)
  Resolves the promise with Decision{Provider, Kind, Model}
        │
        ▼ pick — wakes up
  Maps Decision.Provider → HostPtr (pre-resolved in Init)
  Returns host to Envoy; request proceeds to upstream
        │
        ▼ adapt — request-headers phase
  Reads upstream name from dynamic metadata
  Selects translator (schema rewrite) + auth handler
  Strips client-auth headers, applies translated request headers
        │
        ▼ adapt — request-body phase
  Translates request body (e.g. OpenAI → Bedrock Converse format)
  Body-signed auth (AWS SigV4) signs final body + :path here
        │
        ▼ upstream provider (OpenAI / Anthropic / Bedrock / Vertex …)
        │
        ▼ adapt — response-headers phase
  Translates provider-specific response headers
        │
        ▼ adapt — response-body phase
  Translates response body back to the client's schema
        │
        ▼ meter — response observer (zero latency, chunks forwarded as they arrive)
  Streaming (text/event-stream): head+tail ring buffer → parse SSE usage events
  Non-streaming (application/json): accumulate body → parse top-level usage object
  Emits orange_input_tokens / orange_output_tokens counters
  Writes orange_meter.{input,output}_tokens dynamic metadata
        │
        ▼ Client
```

## Why the promise / async pattern

Envoy's filter chain processes all request headers across every filter *before* any filter sees the body. This means a synchronous write to filter state in a body callback arrives too late to influence `ChooseHost`, which runs during the header phase. `match` therefore publishes a `StreamPromise[Decision]` at headers time and resolves it in the body callback; `pick` suspends `ChooseHost` until the promise resolves, then completes host selection on the cluster main thread.

## Error paths

- **Missing `model` field**: `match` resolves the promise with `ErrModelRequired` and sends a `400` local response. `pick` completes with `ErrDetail` set; the stream closes without touching an upstream.
- **Unknown model**: same flow, error code and HTTP status come from `config.classify.on_miss`.
- **Stream terminated before body**: `onStreamComplete` resolves with `ErrStreamTerminated` (first-wins, so it is a no-op if `bodyHandler` already resolved).
- **Auth failure**: `adapt` falls back to `noAuth{}` and logs the error; the request still reaches the upstream.

---

## pick — DNS reconciliation

`pick` implements a STRICT_DNS-style refresh loop (see `docs/orange-pick-strict-dns.md`). Every TTL expiry triggers `resolveAll`, which reconciles the live cluster host set:

| Condition | Action |
|-----------|--------|
| Resolve succeeds, addr unchanged | Refresh `nextRefresh` from new TTL; keep existing `HostPtr` — no cluster churn |
| Resolve succeeds, addr changed | `RemoveHosts(old)` → `AddHosts(new)` → `UpdateHostHealth(Healthy)` |
| Resolve fails | Preserve existing entry; reset `nextRefresh` to `now + minTTLFloor` (retry soon without hammering resolver) |
| New provider (not in current map) | `AddHosts` → `UpdateHostHealth(Healthy)` |
| Provider deleted from config | `RemoveHosts(old)` |

The `hosts` map is published atomically (`atomic.Pointer`) after each `resolveAll` cycle so `ChooseHost` (the hot path) remains lock-free.

### Constants

| Name | Default | Notes |
|------|---------|-------|
| `defaultResolveTimeout` | 5s | Per-refresh DNS timeout; covers k8s ndots search-domain chaining |
| `defaultDNSRefreshInterval` | 30s | Fallback wake interval when no hosts are registered |
| `minTTLFloor` | 10s | Clamps pathologically short TTLs; also the retry delay after a DNS failure |

### Testability

`cluster.resolveFunc` (nil by default → `resolveUpstream`) can be set in tests to stub DNS without network access. `cluster.lookupHost` is an extracted method (rather than an inline closure in `NewClusterLB`) so it can be called directly in unit tests.

---

## Testing

Each package has a `_test.go` in the same package (white-box).

### match

| Test | What it covers |
|------|---------------|
| `TestHeaders_storesPendingInStreamObjectBag` | Promise stored in stream-object bag at headers phase |
| `TestBody_knownModel_resolvesUpstream` | Full flow: body resolved, metadata written, bag cleaned up |
| `TestBody_missingModel_400` | Missing `model` field → 400, `ErrModelRequired` |
| `TestBody_unknownModel_404` | Unknown model → 404, `ErrUnknownModel` |
| `TestOnStreamComplete_cleansUpWhenBodyNeverRan` | Disconnect before body → `ErrStreamTerminated` |
| `TestOnStreamComplete_nilContextIsNoop` | No panic when context is nil |
| `TestHeaders_nonMatchingRequest_404` | Non-routed path → 404 |
| `TestPick_getStreamObject` | `DecisionKey.GetFromCtx` retrieves promise via filter state |
| `TestBody_authorityRewrite` | `:authority` rewritten to provider host |
| `TestBody_filterStatePopulated` | Filter state keys set correctly |
| `TestBody_partialChunk_skipped` | Non-terminal chunk does not resolve promise |
| `TestBody_anthropicKind` | `anthropic` kind resolved correctly |
| `TestBody_modelWithNameOverride` | `Decision.Model` carries client-facing name, not backend alias |
| `TestBody_compoundModelName` | `groq/llama-3.1-8b-instant` compound name resolves |

### pick

| Test | What it covers |
|------|---------------|
| `TestSplitEndpoint` | URL/host:port parsing, scheme defaults, error cases |
| `TestInit_HostnamePopulated` | Integration: `Init` resolves real DNS and calls `AddHosts` with correct `Hostname` |
| `TestEarliestNextRefresh_nilMap` | Nil map → `now + defaultDNSRefreshInterval` |
| `TestEarliestNextRefresh_emptyMap` | Empty map → same fallback |
| `TestEarliestNextRefresh_singleEntry` | Single entry → its `nextRefresh` |
| `TestEarliestNextRefresh_picksEarliest` | Multiple entries → minimum `nextRefresh` |
| `TestLookupHost_errDecision` | `Decision.Err` set → `ErrDetail` propagated |
| `TestLookupHost_knownProvider` | Known provider → correct `HostPtr` |
| `TestLookupHost_unknownProvider` | Provider absent from map → `orange.unknown_upstream` |
| `TestLookupHost_nilHosts` | Nil hosts map → `orange.unknown_upstream` |
| `TestLookupHost_errTakesPrecedenceOverHosts` | `Err` wins even when host exists in map |
| `TestResolveAll_newProvider` | First pass → `AddHosts` called for every provider |
| `TestResolveAll_addrUnchanged` | Same IP on second pass → no cluster churn, `HostPtr` preserved |
| `TestResolveAll_addrChanged` | IP changes → `RemoveHosts` + `AddHosts` |
| `TestResolveAll_resolveFailKeepsOld` | DNS error → existing host preserved, `nextRefresh` reset to `minTTLFloor` |
| `TestResolveAll_providerDeleted` | Provider removed from config → `RemoveHosts` on next cycle |
| `TestResolveAll_ttlFloor` | Short TTL clamped to `minTTLFloor` |
| `TestShutdown_cancelsRefreshContext` | `Shutdown` cancels refresh context and calls `done()` |
