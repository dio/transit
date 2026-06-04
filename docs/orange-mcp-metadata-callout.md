# orange-mcp: Dynamic Metadata via Async Callout

## MCP context

Orange proxies the [MCP streamable HTTP transport](https://spec.modelcontextprotocol.io/specification/2025-06-18/basic/transports/#streamable-http).
Every client interaction is a JSON-RPC 2.0 `POST` to `/mcp/<profile>`.  The
HTTP method is always `POST`; the MCP operation is in the JSON-RPC `method`
field.

Key methods that appear in production:

| JSON-RPC method | Meaning | Fan-out? |
|---|---|---|
| `initialize` | Start a session; negotiate capabilities | yes — one leg per backend in the profile |
| `tools/list` | Enumerate available tools | yes — merged across backends |
| `prompts/list` | Enumerate prompts | yes |
| `resources/list` | Enumerate resources | yes |
| `tools/call` | Invoke a single tool | no — routed to exactly one backend |
| `notifications/*` | Client notifications | broadcast — one leg per backend |

For `tools/call` the tool name in `params.name` is prefixed with the backend
name: `kiwi__search-flight`. Orange strips the prefix and routes the call to
the `kiwi` backend only.

Without MCP-level fields in the access log, every request looks identical:
`POST /mcp/default 200`. With `method` and `tool` the log differentiates
initialize latency from list latency from tool-call latency per backend,
enabling per-operation SLOs and anomaly detection.

---

## Problem

The `orange-mcp` inbound filter is a no-op pass-through. The MCP sidecar runs
as a separate HTTP server (not as an Envoy filter callback), so it cannot call
`w.SetMetadata` directly. Without dynamic metadata, MCP-method-level fields
(e.g. `tools/list`, `tools/call`, tool name) cannot appear in Envoy's access
log via `%DYNAMIC_METADATA(...)%`.

**Current workaround:** the sidecar sets `x-orange-mcp-method` and
`x-orange-mcp-tool` on the inbound response; the access log reads them via
`%RESP(x-orange-mcp-method)%`. These headers are visible to the client, which
is a minor leak of internal routing state.

---

## Planned approach

Replace the no-op `orange-mcp` filter with one that makes a single async
callout to the sidecar's `/classify` endpoint, extracts the MCP method and
tool name from the response, and sets dynamic metadata before forwarding the
request.

```
client → Envoy inbound ──┐
                          │  OnRequestHeaders:
                          │    w.HTTPCallout → sidecar /classify
                          │    callback: w.SetMetadata("orange_mcp", "method", method)
                          │              w.SetMetadata("orange_mcp", "tool",   tool)
                          │    flush + ContinueRequest
                          ▼
              orange-mcp-loopback cluster → sidecar (full request handling)
```

The classify call reads only enough of the body to identify method and tool.
The actual request forwarding still goes through the normal sidecar path.

`w.HTTPCalloutAllSettled` is available for fan-out to N backends in parallel
(used by `examples/mcp-profile-gateway`), but the classify probe is a single
call so the simpler `w.HTTPCallout` form is correct here. See
[async-http-callouts.md](async-http-callouts.md) for the full API.

A deeper alternative — moving the MCP fan-out itself into the filter using
`HTTPCalloutAllSettled` — would give the filter direct access to each backend
response and eliminate the classify probe entirely. That is a larger
architectural change (the sidecar becomes a thin session manager rather than
the fan-out engine) and is deferred for now.

**Hybrid split.** MCP has two fundamentally different transport shapes:

| Path | Shape | Callout viable? |
|---|---|---|
| `POST /mcp/<profile>` (initialize, tools/list, tools/call, …) | JSON-RPC request / response | Yes — response body closes; `HTTPCalloutAllSettled` can settle all backend legs |
| `GET /mcp/<profile>` (server-push event stream) | Long-lived SSE stream | No — stream never closes; callout never settles |

The natural split is therefore:

- **JSON-RPC POST path** → handled entirely in the filter via
  `HTTPCalloutAllSettled`, one callout per backend. The filter fans out,
  merges responses, and sets dynamic metadata. SSE framing from backends that
  respond with `text/event-stream` (e.g. Kiwi — see `bodyJSON` in
  `jsonrpc.go`) must be stripped inside the callout callback before parsing.
- **SSE GET path** → stays on the current sidecar path. The sidecar fans out
  GET connections to each backend and multiplexes events back to the client.

Under this split the sidecar shrinks to session management and SSE
multiplexing; the fan-out logic for all request/response methods moves into
the filter where `w.SetMetadata` is available natively.

---

## Implementation sketch

### Filter function

```go
up.Register(FilterName, func(w *up.Writer, r *up.Request) {
    body := r.Body() // request body bytes; copy before callout
    _, err := w.HTTPCallout(up.HTTPCalloutRequest{
        Cluster:       "orange-mcp-classify",
        Headers:       [][2]string{
            {":method", "POST"},
            {":path",   "/classify"},
            {"host",    "orange-mcp-sidecar.local"},
        },
        Body:          body,
        TimeoutMillis: 50, // classify is cheap; tight timeout
    }, func(result up.HTTPCalloutResult, _ [][2]shared.UnsafeEnvoyBuffer, body []shared.UnsafeEnvoyBuffer) {
        if result != up.HTTPCalloutSuccess || len(body) == 0 {
            return // fall through; access log fields will be absent
        }
        var info struct {
            Method string `json:"method"`
            Tool   string `json:"tool,omitempty"`
        }
        if err := json.Unmarshal([]byte(body[0].ToString()), &info); err != nil {
            return
        }
        w.SetMetadata("orange_mcp", "method", info.Method)
        if info.Tool != "" {
            w.SetMetadata("orange_mcp", "tool", info.Tool)
        }
    })
    if err != nil {
        return // callout init failed; request still forwards
    }
}, up.WithGroup(g))
```

`body[0].ToString()` is only valid during the callback. `json.Unmarshal` copies
the string internally, so no explicit copy is needed here.

### Sidecar `/classify` endpoint

```go
http.HandleFunc("/classify", func(w http.ResponseWriter, r *http.Request) {
    req, _, err := readRPCRequest(r.Body)
    if err != nil {
        w.WriteHeader(http.StatusBadRequest)
        return
    }
    info := struct {
        Method string `json:"method"`
        Tool   string `json:"tool,omitempty"`
    }{Method: req.Method}
    if req.Method == methodToolsCall {
        backend, stripped, _, err := rewriteToolCall(req)
        if err == nil {
            info.Tool = backend + toolSeparator + stripped
        }
    }
    json.NewEncoder(w).Encode(info)
})
```

### Envoy cluster

Add a static cluster pointing at the sidecar's classify port (can reuse
`orange-mcp-loopback` if the path `/classify` is registered, or add a
dedicated port):

```yaml
- name: orange-mcp-classify
  type: STATIC
  connect_timeout: 0.05s
  load_assignment:
    cluster_name: orange-mcp-classify
    endpoints:
      - lb_endpoints:
          - endpoint:
              address:
                socket_address: { address: 127.0.0.1, port_value: 10004 }
```

### Access log

Switch from `%RESP(...)%` to `%DYNAMIC_METADATA(...)%`:

```yaml
mcp_method: "%DYNAMIC_METADATA(orange_mcp:method)%"
mcp_tool:   "%DYNAMIC_METADATA(orange_mcp:tool)%"
```

---

## Constraints

- `w.HTTPCallout` issues a single callout and cannot be combined with `w.Go`.
  `w.HTTPCalloutAllSettled` fans out to N callouts and is the right primitive
  if the filter ever needs to contact multiple backends directly (e.g. the
  deeper migration noted above). The classify probe needs only one call, so
  `w.HTTPCallout` is the correct choice here.
- Body buffers in the callout callback are Envoy-owned and valid only for the
  duration of the callback. Call `.ToString()` / `.ToBytes()` before retaining
  any value.
- If the classify callout times out or fails, `SetMetadata` is simply not
  called. The request still forwards normally; the access log fields are absent
  rather than incorrect.
- `SendLocalResponse` is safe from the callout callback if classification
  reveals a malformed request — but orange-mcp does not validate, so this is
  not needed here.
- The timeout budget (50 ms suggested above) is an intra-host loopback call and
  should complete in < 1 ms under normal conditions. Tune with `wrk` against the
  `/classify` endpoint directly.

---

## Why not reuse orange-mcp-loopback?

`orange-mcp-loopback` routes the full `/mcp` request to the sidecar for
processing. The classify callout is a separate, cheap probe that only parses
the JSON-RPC envelope. Reusing the same cluster is possible (both point at the
same address) but mixing the classify probe and the real MCP processing on the
same cluster makes timeout and retry policy harder to tune independently.
