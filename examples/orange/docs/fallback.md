## Fallback chain: investigation log and fix summary

This document records the bugs found and fixes applied while building the
four-provider fallback demo (`make demo-fallback`): three dead primaries
(TEST-NET-1) → Vertex AI.

---

### Bug 1 — chain overrun in pick (index out of bounds → nil host)

**Symptom**: after exhausting the chain, extra Envoy retries returned
`orange.fallback_exhausted` from `lookupHostN`, which made `ChooseHost`
return `nil, completion.Complete(nil, …)`. Envoy then reused the last host
from the previous successful `ClusterLBCompletion`, so every over-count
attempt continued hitting the last fallback with no error, but the chain
selection was wrong.

**Fix**: clamp `idx` to `len(d.Fallbacks) - 1` instead of returning an error
when the attempt number exceeds the chain length. Extra retries beyond the
chain keep hitting the last fallback.

Files: `internal/pipeline/pick/pick.go` (`lookupHostN`).

---

### Bug 2 — chain overrun in adapt (stale `:authority` on over-count retry)

**Symptom**: same condition as bug 1 but in the adapt upstream filter. When
`attempt-1 >= len(fallbacks)` adapt skipped the upstream update, leaving the
stale `:authority` from the primary for all over-count retries.

**Fix**: same clamp — `idx = min(attempt-1, len(fallbacks)-1)`.

Files: `internal/pipeline/adapt/adapt.go` (`handler`).

---

### Bug 3 — `GetHostSelectionRetryCount()` resets to 0 per HTTP attempt (root cause of connect-timeout on fallback)

**Symptom**: with a four-provider chain (3 dead primaries + Vertex AI), all
eight Envoy retry attempts hit connect-timeout (`UF` flag, 5 s each, total
≈40 s). The adapt filter logs correctly showed `provider=vertex_anthropic
authority=us-east5-aiplatform.googleapis.com` for attempt 3+, yet the
upstream TCP connection was still going to `192.0.2.x`.

**Root cause**: `ChooseHost` used `ctx.GetHostSelectionRetryCount()` as the
fallback index. That counter counts *within-attempt* host-selection retries
(i.e. how many times `ShouldSelectAnotherHost` fired within one HTTP
attempt). It resets to 0 at the start of every new HTTP retry attempt.

Consequence: on every HTTP retry, pick saw `attempt = 0` and selected the
primary's `HostPtr` (a `192.0.2.x` IP). Envoy opened a TCP connection to
that IP. Meanwhile adapt correctly rewrote `:authority` and the auth headers
for Vertex AI — but the TCP socket was already pointing at 192.0.2.x, so
the connection timed out.

The two-provider chain (primary + vertex) appeared to work only because in
early testing the chain had not yet been exercised past attempt 0; once
exhausted the mis-selection caused the same timeout.

**How adapt tracks HTTP attempts**: adapt writes the 1-based attempt number
to the `orange.adapt.attempt` filter state after each HTTP attempt header
phase (`handler`). On HTTP attempt N, adapt has already written `N-1` (from
the previous attempt) before `ChooseHost` is called for attempt N.

| HTTP attempt | `StateAttempt` value when `ChooseHost` called | pick selects |
|:---:|:---:|:---|
| 1 | (absent / 0) | primary |
| 2 | 1 | fallbacks[0] |
| 3 | 2 | fallbacks[1] |
| 4 | 3 | fallbacks[2] (clamped) |
| 5–8 | 4–7 | fallbacks[2] (clamped, last fallback) |

**Fix**: replace `ctx.GetHostSelectionRetryCount()` with a read of
`match.StateAttempt` filter state in pick's `ChooseHost` fast path:

```go
var attempt uint32
if v, ok := ctx.GetFilterState(match.StateAttempt); ok && v != "" {
    if n, err := strconv.Atoi(v); err == nil && n > 0 {
        attempt = uint32(n)
    }
}
```

`StateAttempt` (`"orange.adapt.attempt"`) is now a named constant in the
`match` package (shared by adapt and pick); the local `stateAttempt` const
in adapt now aliases it.

Files:
- `internal/pipeline/match/match.go` — added `StateAttempt` constant
- `internal/pipeline/pick/pick.go` — `ChooseHost` reads `StateAttempt`
- `internal/pipeline/adapt/adapt.go` — `stateAttempt` aliases `match.StateAttempt`

---

### Supporting change — `ORANGE_DNS_SERVERS` env var

**Problem**: on macOS with Tailscale active, `/etc/resolv.conf` is set to
`100.100.100.100` (Tailscale MagicDNS). While Tailscale returns the same
public IPs as `8.8.8.8` for LLM provider endpoints, the private split-DNS
can theoretically return different results or fail for unknown hostnames.

**Fix**: `ORANGE_DNS_SERVERS=8.8.8.8,1.1.1.1` (or any comma-separated
`ip[:port]` list) bypasses `/etc/resolv.conf` for all lookups in pick.
The value is parsed once at startup into `staticDNSConfig` (a package-level
var); `lookupWithTTL` uses it when non-nil, reads `/etc/resolv.conf` freshly
otherwise (so VPN connect/disconnect is still picked up when no override is
set).

`make demo-fallback` passes `ORANGE_DNS_SERVERS=8.8.8.8,1.1.1.1`
automatically.

Files: `internal/pipeline/pick/pick.go` (`staticDNSConfig`, `lookupWithTTL`).

---

### Supporting change — TEST-NET-1 primaries

Dead primaries were originally `127.0.0.1:19091–19093` (loopback, instant
ECONNREFUSED). ECONNREFUSED is not in the default `retry_on` list, so Envoy
did not retry those attempts. Switched to `192.0.2.1–3` (RFC 5737
TEST-NET-1): routed nowhere, TCP SYN never answered → Envoy hits
`connect_timeout: 5s` → flags `UF,UC` → retries as `connect-failure`.

Files: `demos/fallback/orange.yaml`, `Makefile` (demo-fallback echo lines).

---

### TLS / SNI notes

The Vertex AI endpoint `us-east5-aiplatform.googleapis.com` resolves to
`216.239.x.x` IPs (anycast). Those IPs serve a cert with SAN
`DNS:*.googleapis.com` which covers the hostname. `auto_sni_san_validation`
validates the SAN against the `:authority` header (rewritten by adapt to
`us-east5-aiplatform.googleapis.com`), so TLS succeeds once the TCP
connection actually reaches Google's network.

Both Tailscale MagicDNS and `8.8.8.8` return the same `216.239.x.x` IPs,
so the DNS override does not change which IPs are used — it only ensures the
resolution path is predictable and does not fail if Tailscale's split-DNS
blocks or rewrites the query.

---

### Bug 4 — adapt used `prov.Host()` instead of `prov.BindingHost(binding)` (HTTP 400 for multi-binding providers)

**Symptom**: `TestSNI_twoBindings` returned HTTP 400. Adapt logs showed
`authority=` (empty). The `auto_host_sni` patch is already landed; the TLS
path was fine. The request was rejected before reaching the stub because the
upstream `:authority` header was empty.

**Root cause**: adapt called `prov.Host()` and `prov.Endpoint` for the
`:authority` header and translator endpoint. For providers that have named
bindings but no top-level `endpoint:` field, both return empty string.

Match correctly called `provider.BindingHost(binding)` to set `:authority`
in the body phase, but adapt's headers phase ran after match and overwrote it
with the empty value from `prov.Host()`.

**Fix**:
- Added `Provider.BindingEndpoint(binding string) string` to config — returns
  the named binding's endpoint URL, falling back to `Provider.Endpoint`.
- In adapt's `handler`, read `binding` from `match.StateBinding` filter state
  when `attempt == 0` (primary). Fallback targets use `""` binding so they
  naturally fall through to `Provider.Endpoint` / `Provider.Host()`.
- Use `prov.BindingHost(binding)` for `:authority` and `sc.upstreamHost`.
- Use `prov.BindingEndpoint(binding)` for the translator endpoint.

Files:
- `internal/config/config.go` — added `BindingEndpoint`
- `internal/pipeline/adapt/adapt.go` — binding-aware authority + endpoint

---

## Retry policy: orange.yaml as the source of truth

### Motivation

In ai-gateway, retry policy lives in a separate `BackendTrafficPolicy` CRD that
the user must apply alongside the `AIGatewayRoute`. The gateway controller
translates priority annotations into xDS `LoadAssignment` priorities, and the
retry knobs (`numRetries`, `numAttemptsPerPriority`, `retryOn`) must be
configured to match.

Orange takes a different approach: the chain in `orange.yaml` *is* the priority
ordering — chain position `i` maps directly to attempt `i`. There is no CRD.
The retry policy is declared inline with the chain:

```yaml
routing:
  chain:
    retry:
      retry_on: "connect-failure,reset,5xx,retriable-status-codes"
      per_try_timeout_ms: 10000
    children:
      - target: { provider: primary }
      - target: { provider: fallback }
```

Orange auto-derives `x-envoy-max-retries = len(children) - 1` so Envoy always
allows exactly as many attempts as providers in the chain.

### Injection point: match's headers phase

Envoy's `RetryStateImpl` is created during the router filter's `decodeHeaders`
call, which runs **after** orange-match's headers handler returns
`HeadersStatusContinue`. Headers injected by match via `w.SetRequestHeader` in
`tagRequestForEndpoint` are flushed before the filter returns, so the router
sees them.

The body phase (where the model and chain are known) runs **after** the router's
`decodeHeaders`, so retry headers injected there would be silently ignored by
`RetryStateImpl`.

Because of this ordering, orange uses the config snapshot taken at headers phase
to compute global values across all chains:

| orange.yaml field | Envoy header | Computed as |
|---|---|---|
| `len(children) - 1` | `x-envoy-max-retries` | max across all chains |
| `chain.retry.retry_on` | `x-envoy-retry-on` | union across all chains |
| `chain.retry.per_try_timeout_ms` | `x-envoy-upstream-rq-per-try-timeout-ms` | max across all chains |

Per-chain specificity is not achievable for timeout at headers phase (the model
is in the body). Conservative maximums are safe: extra retry budget for
single-provider models is harmless (pick always returns the same host), and a
generous timeout ceiling gives every provider enough headroom.

### Route floor in envoy.tmpl.yaml

The route must have `retry_on` set to create a `RetryStateImpl`. Without it,
`x-envoy-retry-on` (which is additive OR, not a replacement) has no bitmask to
add to and all retry headers are ignored. The minimal route config is:

```yaml
retry_policy:
  retry_on: "connect-failure,reset,5xx,gateway-error,retriable-status-codes"
  retriable_status_codes: [429]
  num_retries: 1          # overridden per-request by x-envoy-max-retries
  retry_back_off:
    base_interval: 0.1s
    max_interval: 1s
```

`num_retries: 1` in the route is just a sentinel; the real ceiling comes from
`x-envoy-max-retries` injected by orange. When no chain is configured,
`MaxChainRetries()` returns 0 and no header is injected — the route's
`num_retries` applies directly for single-provider error-handling retries.

### Route floor on Envoy Gateway (Kubernetes)

When orange runs behind [Envoy Gateway](https://gateway.envoyproxy.io) on
Kubernetes, `envoy.tmpl.yaml` is replaced by EG CRDs. The enabling floor that
was a static `retry_policy` block becomes a `BackendTrafficPolicy` targeting the
`HTTPRoute` that points at orange's service. Orange's per-request
`x-envoy-max-retries` injection continues to work identically — the match filter
still runs before EG's router filter.

**HTTPRoute** (the route pointing at orange):

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: orange
  namespace: orange
spec:
  parentRefs:
    - name: orange-gateway
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /
      backendRefs:
        - name: orange
          port: 8080
```

**BackendTrafficPolicy** (the retry floor — equivalent to `retry_policy` in
`envoy.tmpl.yaml`):

```yaml
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: BackendTrafficPolicy
metadata:
  name: orange-retry-floor
  namespace: orange
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: orange
  retry:
    retryOn:
      triggers:
        - connect-failure
        - reset
        - retriable-status-codes
      httpStatusCodes: [429, 500, 502, 503, 504]
    numRetries: 1          # floor; x-envoy-max-retries from orange overrides per-request
    perRetry:
      backOff:
        baseInterval: 100ms
        maxInterval: 1s
```

`numRetries: 1` is the same sentinel as in the static config — it just keeps
`RetryStateImpl` alive so that orange's `x-envoy-max-retries` header has
something to override. Orange's `MaxChainRetries()` sets the real ceiling.

The `perRetry.timeout` field is intentionally omitted here; orange's
`x-envoy-upstream-rq-per-try-timeout-ms` (derived from `chain.retry.per_try_timeout_ms`)
sets it per-request when a chain declares a value.

**Contrast with ai-gateway's BackendTrafficPolicy**: ai-gateway requires the
user to set `numRetries` equal to the number of priority tiers and
`numAttemptsPerPriority: 1` explicitly, because the retry count and priority
count are decoupled in its xDS-based model. With orange, `numRetries` in the
BackendTrafficPolicy is always `1` regardless of chain depth — orange's
auto-derived `x-envoy-max-retries` takes care of it.

### Testing (resume here)

Run the demo:

```bash
cd examples/orange
make demo-fallback   # requires GCP_PROJECT and GCP_SERVICE_ACCOUNT_JSON in .env
```

From another shell:

```bash
curl -s localhost:8080/v1/messages \
  -H 'content-type: application/json' \
  -H 'authorization: Bearer demo/maya/sk-fallback' \
  -d '{"model":"claude-haiku-4-5","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}'
```

**What to verify:**

1. Envoy access log shows `response_code: 200` and `response_flags: "-"` (no
   error flag), confirming the chain resolved to Vertex AI.

2. `response_flags` on the retried attempts should include `UF` (upstream
   connection failure) for the three TEST-NET-1 primaries — look for earlier
   log lines with `response_code: 0`.

3. `x-envoy-max-retries` injection: the demo chain has 4 providers → orange
   injects `x-envoy-max-retries: 3`. Confirm by checking that Envoy does not
   emit more than 3 retry log lines before the 200.

4. `per_try_timeout_ms: 10000` from `chain.retry` → confirm each failing attempt
   times out in ~10 s (not the old 30 s). Check `duration_ms` in the access log
   per retry attempt.

5. With no chain configured (test by pointing `ORANGE_CONFIG` at
   `e2e/testdata/orange.yaml` which uses sugar-path routing), confirm orange does
   **not** inject `x-envoy-max-retries` and the route's `num_retries: 1` floor
   applies.

**If retries aren't firing:** check that the route `retry_policy.retry_on`
includes `connect-failure` — without a non-empty `retry_on` in the route,
Envoy never creates `RetryStateImpl` and all `x-envoy-*` headers are silently
ignored.

### Files changed

- `internal/config/config.go` — added `ChainRetryPolicy`, `ChainNode.Retry`,
  `MaxChainRetries()`, `ChainRetryPolicy()`
- `internal/config/config.schema.json` — added `ChainRetryPolicy` def and
  `retry` to `ChainNode`
- `internal/pipeline/match/match.go` — inject retry headers in headers phase
- `demos/fallback/orange.yaml` — added `chain.retry` block
- `demos/fallback/envoy.tmpl.yaml` — simplified to minimal floor
