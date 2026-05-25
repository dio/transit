# Metadata and Filter State in Transit

Transit exposes three distinct per-stream data stores. They are not
interchangeable — each has a different scope, mutability, type system, and set
of readers.

---

## Quick-reference table

| Store | Written by | Read by | Lifetime | Typed? |
|---|---|---|---|---|
| Filter state | HTTP filter (request phase only) | Cluster / LB Extension | per-stream | no (bytes) |
| Dynamic metadata | HTTP filter (any phase) | Same filter, other filters, access log | per-stream | yes |
| Static metadata | xDS config (route / cluster / host / listener) | HTTP filter (read-only) | config | yes |

---

## Filter state

Filter state is the mechanism for passing a routing hint from an HTTP filter
into a Cluster Extension or LB Policy. It is a flat per-stream byte store
keyed by a string.

```go
// HTTP filter — request headers phase
w.SetFilterState("x-selected-model", "gpt-fast")
```

```go
// Cluster Extension — host selection
model, _ := ctx.GetFilterState("x-selected-model")
```

**Constraints:**

- `SetFilterState` is only meaningful on the **request path**. On the response
  path it is a no-op because Envoy has already selected the upstream host.
- Values are raw `[]byte`. No schema. Parse on the Cluster Extension side.
- On the request path with `directWrite=false` (queued mode), `SetFilterState`
  is batched with header mutations and flushed before `ContinueRequest`. On the
  response path or inside a callout callback (`directWrite=true`) it writes
  immediately.
- Filter state is not visible to Envoy's access log formatter; use dynamic
  metadata for that.

---

## Dynamic metadata

Dynamic metadata is a namespaced, typed key-value store attached to the
current stream. Filters write to it; other filters in the same chain, the
Envoy access log formatter, and rate-limit services can read it.

### Writing

```go
// value may be string, float64, int, int64, or bool.
w.SetMetadata("com.example.my-filter", "model", "gpt-fast")
w.SetMetadata("com.example.my-filter", "input_tokens", int64(420))
```

Use a reverse-DNS namespace that identifies your filter to avoid collisions
with other filters or Envoy's built-in metadata (e.g. `envoy.filters.http.jwt_authn`).

### Reading

```go
// Read back from the same or a downstream filter.
if buf, ok := w.GetMetadataString(up.MetadataSourceTypeDynamic, "com.example.my-filter", "model"); ok {
    model := buf.String()
}

tokens, ok := w.GetMetadataNumber(up.MetadataSourceTypeDynamic, "com.example.my-filter", "input_tokens")
enabled, ok := w.GetMetadataBool(up.MetadataSourceTypeDynamic, "com.example.my-filter", "flag")
```

### Access log

Dynamic metadata written by a filter is available in Envoy's access log
format string via `%DYNAMIC_METADATA(namespace:key)%`, enabling per-request
structured fields without a separate logging filter.

### Available `MetadataSourceType` values

| Constant | Store read |
|---|---|
| `MetadataSourceTypeDynamic` | Dynamic (mutable, per-stream) |
| `MetadataSourceTypeRoute` | Static metadata on the matched HTTPRoute |
| `MetadataSourceTypeCluster` | Static metadata on the upstream cluster |
| `MetadataSourceTypeHost` | Static metadata on the selected upstream host |
| `MetadataSourceTypeHostLocality` | Locality metadata on the selected host |

---

## Static metadata

Static metadata is set once in xDS configuration and is **read-only** at
request time. It is the right place for per-route or per-cluster policy
configuration (rate-limit tiers, feature flags, auth namespaces) that must
not change mid-stream.

### Reading via `GetMetadataString/Number/Bool`

Use `MetadataSourceTypeRoute`, `MetadataSourceTypeCluster`, or
`MetadataSourceTypeHost` to read typed proto fields set in the Envoy
configuration:

```go
// Read a string field from the matched route's metadata.
if buf, ok := w.GetMetadataString(up.MetadataSourceTypeRoute, "com.example.routing", "tier"); ok {
    tier := buf.String()
}

// Read a flag from the upstream cluster's metadata.
if enabled, ok := w.GetMetadataBool(up.MetadataSourceTypeCluster, "com.example.features", "shadow"); ok && enabled {
    // ...
}
```

### Reading via the attribute API

The attribute API exposes static metadata as a JSON blob via a single
`GetAttributeString` call. Use this when you need the raw metadata struct or
when the typed-field API does not cover your use case:

```go
// AttributeIDXdsRouteMetadata, AttributeIDXdsClusterMetadata,
// AttributeIDXdsListenerMetadata, AttributeIDXdsVirtualHostMetadata,
// AttributeIDXdsUpstreamHostMetadata
if buf, ok := w.GetAttributeString(up.AttributeIDXdsRouteMetadata); ok {
    raw := buf.String() // JSON-encoded Struct proto
}
```

### Setting static metadata in xDS config

In an Envoy Gateway `EnvoyProxy` / `EnvoyPatchPolicy`, add a `metadata` field
under the relevant route, cluster, or virtual host:

```yaml
metadata:
  filter_metadata:
    com.example.routing:
      tier: "premium"
      shadow: true
```

For host-level metadata (used with `MetadataSourceTypeHost` or
`AttributeIDXdsUpstreamHostMetadata`), set `metadata` on the endpoint in the
`ClusterLoadAssignment`.

---

## Pitfalls

| Pitfall | Symptom | Fix |
|---|---|---|
| `SetFilterState` on the response path | Cluster Extension receives empty string | Move the call to `OnRequestHeaders` |
| `SetMetadata` with an unrecognised value type | Panic at runtime | Use `string`, `float64`, `int`, `int64`, or `bool` |
| Namespace collision with Envoy built-ins | Unexpected overwrites or missing values | Prefix namespace with a reverse-DNS string you own |
| Reading dynamic metadata before it is written | `ok=false` | Ensure the writing filter runs earlier in the filter chain |
| Using `GetAttributeString(AttributeIDXdsRouteMetadata)` before route matching | Empty result | Read only after route matching (not in pre-route `OnRequestHeaders`) |
| Expecting `GetMetadataString(MetadataSourceTypeCluster, …)` to return host-level metadata | Wrong source | Use `MetadataSourceTypeHost` for per-endpoint metadata |
