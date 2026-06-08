# Config System Design — Go

Full pipeline: snapshot bytes from disk, gRPC, HTTP polling, or a similar
updater → typed in-memory structures, with a tiered memory strategy for
millions of user-owned records.

The important distinction: `GlobalConfig` data is logically dynamic, but every
published `GlobalConfig` value is immutable. Providers, models, servers, and
rate limit policies may change through reloads or control-plane updates, but
each successful change publishes a new compiled snapshot. Readers never mutate
or lock the live tree.

---

## 1. Architecture

```text
ConfigSnapshot                       atomically swapped as one unit
  Generation                         monotonically increasing uint64
  GlobalConfig                       admin-owned providers, models, servers, rate limits
  Pools                              deduplicated routing/filter/auth shapes

Snapshot Updater                     SoTW delivery channel
    gRPC stream / HTTP poll / file      receives packed raw bytes
    decode -> compile -> publish        no partial rollout

User Tables                          user-owned, seeded from snapshot
  KeyTable / ProfileTable
  L1 generation-aware LRU -> L2 atomic immutable map (no L3)

InternPool                           shared string deduplication
  string <-> uint32                  append-only, survives reloads
```

The long-lived runtime owner is `AppState`. It is not itself config data; it is
the process-local container that owns the current snapshot pointer, user tables,
the shared intern pool, and the generation counter. A request enters through
`AppState`, performs one atomic snapshot load, then resolves key/profile data
against that snapshot.

`GlobalConfig` is small, admin-owned, fully validated, and always resident in
RAM. It is immutable after publication. To change admin data, compile a new
`GlobalConfig`, bundle it with matching pools in a new `ConfigSnapshot`, then
atomically swap the pointer.

`Pools` hold the small number of distinct routing, filter, and auth shapes that
exist across many user records. Pool entries are compiled against the same
`GlobalConfig` generation as the snapshot that contains them.

`User Tables` hold the user records delivered with each snapshot in an atomic
immutable L2 map, and cache fully resolved records in a generation-aware L1 LRU.
Resolved records carry the snapshot generation that produced them. An evicted L1
entry is always recoverable from L2 with no external call.

`InternPool` maps repeated strings such as workspace names and user IDs to
`uint32` handles. It is append-only and shared across snapshots.

---

## 2. Logical Schema

Five top-level keys:

- `llm`: admin-owned LLM providers and models (including per-model pricing)
- `mcp`: admin-owned MCP servers
- `profiles`: user-owned MCP tool filters
- `keys`: user-owned LLM routing overrides
- `rate_limit`: admin-owned spend and throughput configuration — split into `rules`
  (named tier primitives) and `policies` (per-scope assignments)

Admin-owned sections are small and can change over time. They are loaded,
validated, compiled, and published as immutable snapshots.

User-owned sections may appear in YAML as seeds or examples, but at production
scale they live in a database. The wire format does not have to be YAML; YAML is
only the human-readable expression of the logical shape.

User-owned record IDs use this convention:

```text
{workspace}/{user}/{name}
```

The path is the identity. There are no `workspace` or `user` fields inside the
record body.

`rate_limit.policies` keys use a prefix of the same convention — 1, 2, or 3 segments:

```text
"demo"               workspace scope — applies to all keys under demo
"demo/adi"           user scope      — applies to all keys owned by adi
"demo/adi/sk-vip"    key scope       — applies to one specific key
```

All matching rules across all three scopes accumulate and are enforced
simultaneously. A request passes only when every matching rule is satisfied.

Annotated example covering all five sections:

```yaml
# ── Admin-owned ───────────────────────────────────────────────────────────────

llm:
  providers:
    anthropic:
      kind: anthropic          # wire protocol: anthropic | openai | bedrock
      endpoint: https://api.anthropic.com
      auth:
        type: anthropic        # bearer | anthropic | gcp | aws | gemini
        secret_ref: env://ANTHROPIC_API_KEY   # env:// | file:// | literal://
      extra:
        anthropic_version: "2023-06-01"       # forwarded verbatim to upstream

    vertex_anthropic:
      kind: anthropic
      backend_schema: gcpanthropic   # overrides the wire-format translator;
                                     # defaults to kind when absent
      endpoint: https://us-east5-aiplatform.googleapis.com
      auth:
        type: gcp
        secret_ref: env://GCP_SERVICE_ACCOUNT_JSON
      extra:
        anthropic_version: "vertex-2023-10-16"
        gcp_project: "env://GCP_PROJECT"      # env:// resolved at load time

  models:
    claude-haiku-4-5:
      provider: anthropic            # must match a providers key
      name: claude-haiku-4-5-20251001  # backend model name; defaults to map key
      endpoint_overrides:
        chat_completions: vertex_anthropic  # this operation goes to a different
                                            # provider; others use anthropic
      pricing:
        input_mtok: 0.80             # USD per million input tokens
        output_mtok: 4.00            # USD per million output tokens
        cache_read_mtok: 0.08        # prompt cache read (charged separately)
        cache_write_mtok: 1.00       # prompt cache write (charged separately)

    gpt-4o-mini:
      provider: openai
      pricing:
        input_mtok: 0.15
        output_mtok: 0.60
      metadata:                      # informational; surfaced in GET /v1/models
        context_length: 128000
        tags: [chat, fast, vision]

mcp:
  servers:
    kiwi:
      endpoint: https://mcp.kiwi.com
      namespace: kiwi                # prefixed onto all tool names: "kiwi/search-flight"
      tools_include:                 # server-level allowlist; profiles can only
        - search-flight              # include tools that appear here

    github:
      endpoint: https://api.githubcopilot.com/mcp/
      namespace: github
      auth:
        type: bearer
        secret_ref: env://GITHUB_TOKEN
      tools_include:
        - search_repositories
        - get_file_contents

# ── User-owned ────────────────────────────────────────────────────────────────

profiles:
  demo/adi/default:             # workspace/user/name — path is the identity
    tools:
      kiwi:
        include: [search-flight] # subset of kiwi's tools_include
      github:
        include: [search_repositories]
        optional: true           # session succeeds even if github is unreachable
    auth:
      github:                    # per-profile auth override for this server
        type: bearer
        secret_ref: env://GITHUB_TOKEN

  demo/adi/kiwi-only:
    tools:
      kiwi: {}                   # empty map = expose all allowed tools

keys:
  demo/adi/sk-direct:           # workspace/user/name — no workspace/user body fields
    routing_overrides:
      claude-haiku-4-5:          # client-facing model ID from llm.models
        target:
          provider: anthropic
          name: claude-haiku-4-5-20251001

  demo/adi/sk-fallback:
    routing_overrides:
      claude-haiku-4-5:
        chain:
          retry:
            retry_on: "connect-failure,5xx"
            per_try_timeout_ms: 10000    # ms; maps to x-envoy-upstream-rq-per-try-timeout-ms
          children:
            - target: { provider: fallback_p1, name: claude-haiku-4-5 }
            - target: { provider: vertex_anthropic, name: "claude-opus-4@20250514" }

rate_limit:
  rules:                         # named tier primitives — reusable limit sets
    standard:
      usd_per_day: 200.00
      rpm: 100
    premium:
      usd_per_day: 1_000.00
      rpm: 500
      input_tokens_per_hour: 4_000_000

  policies:
    demo:                        # workspace ceiling — affects every demo key
      - rule: premium            # inherit the premium tier
        models: ["*"]

    demo/adi:                    # user ceiling — stacks on top of workspace rule
      - rule: standard           # inherit the standard tier
        models: ["*"]

    demo/adi/sk-direct:          # key override — stacks on top of both above
      - models: [claude-haiku-4-5, gpt-4o-mini]
        usd_per_hour: 5.00
        input_tokens_per_hour: 800_000
        on_exceed: reject
      - models: ["*"]            # catch-all for any other model this key calls
        rpm: 20
```

---

## 3. Snapshot Updater and Delivery

In production, `AppState` is fed by a snapshot updater. The updater may be a
gRPC stream, an HTTP poller, a file watcher, or another control-plane channel.
The first version should assume State-of-the-World (SoTW) delivery: every update
contains the complete admin snapshot needed to compile a new `ConfigSnapshot`.

Delta delivery can come later, but the internal boundary should already make it
easy to add:

- The updater receives bytes plus metadata such as version, format, checksum,
  and compression.
- The updater tracks the last accepted version, ETag, or resource version so it
  can reject stale SoTW payloads.
- The decoder turns bytes into raw serde structs.
- `compile()` validates the complete raw state and builds owned domain structs.
- `AppState` publishes the result only after the whole snapshot compiles.

```text
gRPC stream / HTTP poll / file
    -> SnapshotEnvelope bytes
    -> decodeRawConfig()
RawConfig
    -> compile()
ConfigSnapshot
    -> AppState.ApplySnapshotEnvelope()
```

SoTW makes correctness simple: a failed update is ignored and the previous
snapshot remains live. The trade-off is payload size, which is why the raw bytes
need a compact packing strategy.

---

## 4. Configuration Packing

The control plane delivers a `SnapshotEnvelope` over gRPC or HTTP. The envelope
is self-describing: it carries a format tag, compression tag, checksum, and a
version so the receiver can reject stale SoTW payloads. The Go struct stays as
written; the wire schema is proto3 (see §4a).

The recommended production path is **proto3 + zstd**. YAML and JSON remain valid
for local dev and seed files. MessagePack is a viable drop-in for JSON with
smaller payload; proto is preferred because it enforces a schema contract between
the control plane and the proxy.

```text
gRPC stream / HTTP poll / file
    -> SnapshotEnvelope (proto-encoded, zstd-compressed payload)
    -> decodeRawConfig()          // format-dispatched; zstd decompressed first
RawConfig
    -> compile()
ConfigSnapshot
    -> AppState.ApplySnapshotEnvelope()
```

SoTW correctness contract: if `Version` in a new envelope is not strictly
greater than the last accepted version, the receiver discards the envelope and
leaves the current snapshot live. This is the only stale-rejection check needed
for SoTW; delta delivery will add resource-version tracking later.

```go
type SnapshotEnvelope struct {
    Version     uint64
    Format      SnapshotFormat
    Compression CompressionKind
    Payload     []byte
    Checksum    []byte
}

type SnapshotFormat string

const (
    SnapshotFormatYAML    SnapshotFormat = "yaml"
    SnapshotFormatJSON    SnapshotFormat = "json"
    SnapshotFormatMsgpack SnapshotFormat = "msgpack"
    SnapshotFormatProto   SnapshotFormat = "proto"
)

type CompressionKind string

const (
    CompressionNone CompressionKind = "none"
    CompressionZstd CompressionKind = "zstd"
)
```

Packed payloads use table encoding:

- A `StringTable` stores every repeated string once; all records reference
  strings by `uint32` index.
- Providers, models, servers, routing shapes, tool filters, and auth overrides
  hold integer indices inside the payload.
- Records are sorted by stable ID index before encoding.
- Optional fields are omitted rather than encoded as empty objects.

The unpacking path produces the same `RawConfig` shape used by the YAML path.
That keeps validation and compilation format-independent.

```text
packed bytes
    -> decompress (zstd)
    -> verify checksum (SHA-256 of decompressed bytes)
    -> proto.Unmarshal -> ConfigPayload
    -> protoToRaw() expands string-table indices
RawConfig
    -> compile()
```

Do not optimize the serving domain model for wire size. Keep the concerns
separate: packed bytes are for transport, raw structs are for serde, domain
structs are for request-time reads.

---

## 4a. Proto Schema

### Design choices

**String table.** Repeated strings — provider IDs, model IDs, server IDs,
namespace names, auth type literals, secret refs, tool names, operation names —
appear in every record. The proto schema pulls them into a top-level
`StringTable` and replaces every repeated string field with a `uint32` index.
The receiver expands indices back to strings during unpacking before handing a
`RawConfig` to `compile()`. This is more explicit and auditable than relying on
proto field compression.

**No cross-references between proto objects.** Proto message fields that
correspond to Go pointer fields (e.g. `model.provider`) use the string-table
index of the provider ID. `compile()` resolves actual pointers; the proto layer
never does.

**Stable ordering.** All repeated record messages (`Provider`, `Model`,
`Server`, `Profile`, `Key`, `RateLimitScope`) are sorted by their string-table
index before encoding. This gives deterministic byte output for a given logical
config, which makes checksums and change detection reliable.

**One checksum.** SHA-256 of the uncompressed, decoded payload bytes. Computed
after decompression, before decoding. The receiver computes and compares before
calling `decodeRawConfig`.

### `snapshot.proto`

```proto
syntax = "proto3";
package config.v1;

// ── Envelope ──────────────────────────────────────────────────────────────────

// SnapshotEnvelope is the outermost frame delivered over gRPC or HTTP.
// Payload contains a compressed (or raw) ConfigPayload proto.
message SnapshotEnvelope {
    uint64        version     = 1;  // monotonically increasing; receiver rejects if <= last accepted
    PayloadFormat format      = 2;
    Compression   compression = 3;
    bytes         payload     = 4;  // compressed ConfigPayload proto bytes
    bytes         checksum    = 5;  // SHA-256 of the decompressed payload bytes
}

enum PayloadFormat {
    PAYLOAD_FORMAT_UNSPECIFIED = 0;
    PAYLOAD_FORMAT_PROTO       = 1;
    PAYLOAD_FORMAT_YAML        = 2;  // local/dev only
    PAYLOAD_FORMAT_JSON        = 3;  // local/dev only
    PAYLOAD_FORMAT_MSGPACK     = 4;  // optional; same logical shape as proto
}

enum Compression {
    COMPRESSION_NONE = 0;
    COMPRESSION_ZSTD = 1;
}

// ── Payload ───────────────────────────────────────────────────────────────────

// ConfigPayload is the decompressed content of SnapshotEnvelope.payload.
// All repeated string fields in sub-messages are indices into strings.
message ConfigPayload {
    StringTable             strings     = 1;
    repeated Provider       providers   = 2;  // sorted by name_idx
    repeated Model          models      = 3;  // sorted by name_idx
    repeated Server         servers     = 4;  // sorted by name_idx
    repeated Profile        profiles    = 5;  // sorted by id_idx
    repeated Key            keys        = 6;  // sorted by id_idx
    repeated RateLimitScope rate_limits = 7;  // sorted by scope_idx
}

// StringTable deduplicates all repeated strings in the payload.
// Index 0 is reserved (empty / unset). First real string is at index 1.
message StringTable {
    repeated string strings = 1;
}

// ── Admin-owned: providers ────────────────────────────────────────────────────

message Provider {
    uint32       name_idx           = 1;  // llm.providers key
    ProviderKind kind               = 2;
    uint32       backend_schema_idx = 3;  // optional; 0 = unset
    uint32       endpoint_idx       = 4;
    Auth         auth               = 5;
    repeated KV  extra              = 6;  // forwarded headers/params
}

enum ProviderKind {
    PROVIDER_KIND_UNSPECIFIED = 0;
    PROVIDER_KIND_ANTHROPIC   = 1;
    PROVIDER_KIND_OPENAI      = 2;
    PROVIDER_KIND_BEDROCK     = 3;
}

// ── Admin-owned: models ───────────────────────────────────────────────────────

message Model {
    uint32                   name_idx           = 1;  // client-facing model ID
    uint32                   provider_idx       = 2;  // must match a Provider.name_idx
    uint32                   api_name_idx       = 3;  // backend name; 0 = use name_idx
    repeated EndpointOverride endpoint_overrides = 4;
    ModelPricing             pricing            = 5;  // zero value = absent
    ModelMetadata            metadata           = 6;  // zero value = absent
}

message EndpointOverride {
    uint32 operation_idx = 1;  // e.g. "chat_completions"
    uint32 provider_idx  = 2;  // override provider for this operation
}

message ModelPricing {
    double input_mtok       = 1;
    double output_mtok      = 2;
    double cache_read_mtok  = 3;
    double cache_write_mtok = 4;
}

message ModelMetadata {
    uint32          description_idx = 1;
    int32           context_length  = 2;
    int32           max_tokens      = 3;
    repeated uint32 tag_idxs        = 4;
}

// ── Admin-owned: MCP servers ──────────────────────────────────────────────────

message Server {
    uint32          name_idx           = 1;  // mcp.servers key
    uint32          endpoint_idx       = 2;
    uint32          namespace_idx      = 3;
    Auth            auth               = 4;  // zero AuthType = absent
    repeated uint32 tools_include_idxs = 5;
}

// ── Auth — shared by Provider, Server, Profile ────────────────────────────────

message Auth {
    AuthType type       = 1;
    uint32   secret_idx = 2;  // env:// | file:// | literal://
}

enum AuthType {
    AUTH_TYPE_UNSPECIFIED = 0;
    AUTH_TYPE_BEARER      = 1;
    AUTH_TYPE_ANTHROPIC   = 2;
    AUTH_TYPE_GCP         = 3;
    AUTH_TYPE_AWS         = 4;
    AUTH_TYPE_GEMINI      = 5;
}

// ── User-owned: profiles ──────────────────────────────────────────────────────

message Profile {
    uint32                id_idx         = 1;  // workspace/user/name
    repeated ToolFilter   tools          = 2;
    repeated AuthOverride auth_overrides = 3;
}

message ToolFilter {
    uint32          server_idx   = 1;
    repeated uint32 include_idxs = 2;  // empty = all allowed tools
    bool            optional     = 3;
}

message AuthOverride {
    uint32 server_idx = 1;
    Auth   auth       = 2;
}

// ── User-owned: keys ──────────────────────────────────────────────────────────

message Key {
    uint32                   id_idx            = 1;  // workspace/user/name
    repeated RoutingOverride routing_overrides = 2;
    repeated RateLimitRule   rate_limit_rules  = 3;  // key-scope; user-managed
}

message RoutingOverride {
    uint32      model_idx = 1;  // client-facing model ID index
    RoutingNode node      = 2;
}

// RoutingNode is recursive. Exactly one of target, chain, split must be set.
message RoutingNode {
    oneof kind {
        RoutingTarget target = 1;
        ChainConfig   chain  = 2;
        SplitConfig   split  = 3;
    }
}

message RoutingTarget {
    uint32 provider_idx = 1;
    uint32 name_idx     = 2;  // backend model name; 0 = use client model ID
}

message ChainConfig {
    RetryPolicy          retry    = 1;  // optional
    repeated RoutingNode children = 2;
}

message RetryPolicy {
    uint32 retry_on_idx       = 1;  // e.g. "connect-failure,reset,5xx"
    int32  per_try_timeout_ms = 2;
}

message SplitConfig {
    repeated SplitChild children = 1;  // weights must sum to 100
}

message SplitChild {
    int32       weight = 1;
    RoutingNode node   = 2;
}

// ── Admin-owned: rate limits (workspace and user scopes only) ─────────────────
// Key-scope (3-segment) rules are encoded inside Key.rate_limit_rules, not here.

message RateLimitScope {
    uint32                 scope_idx = 1;  // 1 or 2 segments (workspace or workspace/user)
    repeated RateLimitRule rules     = 2;
}

message RateLimitRule {
    repeated uint32 model_idxs = 1;  // ["*"] encoded as index to literal "*"

    double usd_per_minute = 2;
    double usd_per_hour   = 3;
    double usd_per_day    = 4;

    int32 rpm = 5;
    int32 rph = 6;
    int32 rpd = 7;

    int32 input_tokens_per_minute  = 8;
    int32 input_tokens_per_hour    = 9;
    int32 input_tokens_per_day     = 10;

    int32 output_tokens_per_minute = 11;
    int32 output_tokens_per_hour   = 12;
    int32 output_tokens_per_day    = 13;

    int32 cache_read_tokens_per_hour  = 14;
    int32 cache_read_tokens_per_day   = 15;

    int32 cache_write_tokens_per_hour = 16;
    int32 cache_write_tokens_per_day  = 17;

    OnExceed on_exceed = 18;
}

enum OnExceed {
    ON_EXCEED_REJECT   = 0;  // default; zero value = reject
    ON_EXCEED_THROTTLE = 1;
    ON_EXCEED_LOG_ONLY = 2;
}

// ── Shared primitives ─────────────────────────────────────────────────────────

message KV {
    uint32 key_idx   = 1;
    uint32 value_idx = 2;
}
```

### Unpacking path

`protoToRaw()` expands string-table indices and produces a `RawConfig`.
`compile()` is unchanged and never sees proto types.

```go
// protoToRaw converts a decoded ConfigPayload proto into the RawConfig
// shape consumed by compile(). String-table indices are expanded here.
func protoToRaw(p *configpb.ConfigPayload) (*RawConfig, error) {
    str := func(idx uint32) string {
        if idx == 0 || int(idx) >= len(p.Strings.Strings) {
            return ""
        }
        return p.Strings.Strings[idx]
    }

    providers := make(map[string]RawProvider, len(p.Providers))
    for _, prov := range p.Providers {
        id := str(prov.NameIdx)
        if id == "" {
            return nil, fmt.Errorf("provider with zero name_idx")
        }
        extra := make(map[string]string, len(prov.Extra))
        for _, kv := range prov.Extra {
            extra[str(kv.KeyIdx)] = str(kv.ValueIdx)
        }
        providers[id] = RawProvider{
            Kind:          prov.Kind.String(),
            BackendSchema: str(prov.BackendSchemaIdx),
            Endpoint:      str(prov.EndpointIdx),
            Auth: RawAuth{
                Type:      prov.Auth.GetType().String(),
                SecretRef: str(prov.Auth.GetSecretIdx()),
            },
            Extra: extra,
        }
    }

    models := make(map[string]RawModel, len(p.Models))
    for _, m := range p.Models {
        id := str(m.NameIdx)
        overrides := make(map[string]string, len(m.EndpointOverrides))
        for _, eo := range m.EndpointOverrides {
            overrides[str(eo.OperationIdx)] = str(eo.ProviderIdx)
        }
        var pricing *RawModelPricing
        if m.Pricing != nil && (m.Pricing.InputMtok != 0 || m.Pricing.OutputMtok != 0) {
            pricing = &RawModelPricing{
                InputMTok:      m.Pricing.InputMtok,
                OutputMTok:     m.Pricing.OutputMtok,
                CacheReadMTok:  m.Pricing.CacheReadMtok,
                CacheWriteMTok: m.Pricing.CacheWriteMtok,
            }
        }
        models[id] = RawModel{
            Provider:          str(m.ProviderIdx),
            Name:              str(m.ApiNameIdx),
            EndpointOverrides: overrides,
            Pricing:           pricing,
        }
    }

    servers := make(map[string]RawServer, len(p.Servers))
    for _, s := range p.Servers {
        id := str(s.NameIdx)
        var auth *RawAuth
        if s.Auth != nil && s.Auth.Type != configpb.AuthType_AUTH_TYPE_UNSPECIFIED {
            v := RawAuth{
                Type:      s.Auth.Type.String(),
                SecretRef: str(s.Auth.SecretIdx),
            }
            auth = &v
        }
        tools := make([]string, len(s.ToolsIncludeIdxs))
        for i, idx := range s.ToolsIncludeIdxs {
            tools[i] = str(idx)
        }
        servers[id] = RawServer{
            Endpoint:     str(s.EndpointIdx),
            Namespace:    str(s.NamespaceIdx),
            Auth:         auth,
            ToolsInclude: tools,
        }
    }

    // profiles, keys, rate_limit.policies follow the same expand-then-map pattern
    // (structure mirrors providers/servers above)

    return &RawConfig{
        LLM: RawLLM{Providers: providers, Models: models},
        MCP: RawMCP{Servers: servers},
        // Profiles, Keys, RateLimit.Policies populated similarly
    }, nil
}
```

### `decodeRawConfig` dispatch

```go
func decodeRawConfig(env SnapshotEnvelope) (*RawConfig, error) {
    payload, err := decompress(env.Compression, env.Payload)
    if err != nil {
        return nil, fmt.Errorf("decompress: %w", err)
    }
    if err := verifyChecksum(payload, env.Checksum); err != nil {
        return nil, fmt.Errorf("checksum mismatch: %w", err)
    }

    switch env.Format {
    case SnapshotFormatProto:
        var pb configpb.ConfigPayload
        if err := proto.Unmarshal(payload, &pb); err != nil {
            return nil, fmt.Errorf("proto unmarshal: %w", err)
        }
        return protoToRaw(&pb)

    case SnapshotFormatYAML:
        var raw RawConfig
        return &raw, yaml.Unmarshal(payload, &raw)

    case SnapshotFormatJSON:
        var raw RawConfig
        return &raw, json.Unmarshal(payload, &raw)

    default:
        return nil, fmt.Errorf("unsupported format %q", env.Format)
    }
}
```

---

## 5. Parse Pipeline

Do not deserialize external bytes directly into domain objects. Use two stages:

1. Decode the external format into raw serde structs.
2. `compile()` raw structs into immutable domain structs.

Raw structs mirror the logical schema. References are strings or decoded string
table values, enums are strings, and there is no validation logic.

`compile()` resolves cross-references, validates enum values and IDs, interns
strings, builds pools, and returns a complete snapshot. If compilation fails,
the old snapshot stays live.

```text
[]byte
    -> decodeRawConfig
RawConfig
    -> compile()
ConfigSnapshot
    -> AppState.ApplySnapshotEnvelope
```

The split is necessary because domain structs contain resolved pointers. For
example, a `ModelRecord` holds a `*ProviderRecord`, and that pointer cannot be
resolved until all providers have been parsed and validated.

---

## 6. Raw Serde Structs

Rules for raw structs:

- Every reference is a plain `string`.
- Every enum is a plain `string`.
- Optional fields use pointer types or `omitempty`.
- Raw types have no methods and no validation logic.
- Raw types stay internal to the parse package.

```go
type RawConfig struct {
    LLM       RawLLM                `yaml:"llm"`
    MCP       RawMCP                `yaml:"mcp"`
    Profiles  map[string]RawProfile `yaml:"profiles,omitempty"`
    Keys      map[string]RawKey     `yaml:"keys,omitempty"`
    RateLimit RawRateLimit          `yaml:"rate_limit,omitempty"`
}

// RawRateLimit is the top-level rate-limiting config section.
// Tiers are named primitives authored in YAML; orange CP expands them into
// policy entries server-side before encoding the snapshot proto, so Tiers is
// always empty on the proto decode path.
type RawRateLimit struct {
    Tiers    map[string]RawRateLimitTier          `yaml:"rules,omitempty"`
    Policies map[string][]RawRateLimitPolicyEntry `yaml:"policies,omitempty"`
}

type RawLLM struct {
    Providers map[string]RawProvider `yaml:"providers"`
    Models    map[string]RawModel    `yaml:"models"`
}

type RawMCP struct {
    Servers map[string]RawServer `yaml:"servers"`
}

type RawProvider struct {
    Kind          string            `yaml:"kind"`
    BackendSchema string            `yaml:"backend_schema,omitempty"`
    Endpoint      string            `yaml:"endpoint"`
    Auth          RawAuth           `yaml:"auth"`
    Extra         map[string]string `yaml:"extra,omitempty"`
}

type RawModel struct {
    Provider          string            `yaml:"provider"`
    Name              string            `yaml:"name,omitempty"`
    EndpointOverrides map[string]string `yaml:"endpoint_overrides,omitempty"`
    Pricing           *RawModelPricing  `yaml:"pricing,omitempty"`
    Metadata          *RawMetadata      `yaml:"metadata,omitempty"`
}

// RawModelPricing holds per-model token prices in USD per million tokens (mtok).
// Required on any model targeted by a USD-based rate limit rule.
// Uses decimal.Decimal (github.com/shopspring/decimal) for exact monetary arithmetic.
type RawModelPricing struct {
    InputMTok      decimal.Decimal `yaml:"input_mtok"`
    OutputMTok     decimal.Decimal `yaml:"output_mtok"`
    CacheReadMTok  decimal.Decimal `yaml:"cache_read_mtok,omitempty"`
    CacheWriteMTok decimal.Decimal `yaml:"cache_write_mtok,omitempty"`
}

type RawMetadata struct {
    Description   string   `yaml:"description,omitempty"`
    ContextLength int      `yaml:"context_length,omitempty"`
    MaxTokens     int      `yaml:"max_tokens,omitempty"`
    Tags          []string `yaml:"tags,omitempty"`
}

type RawServer struct {
    Endpoint     string   `yaml:"endpoint"`
    Namespace    string   `yaml:"namespace"`
    Auth         *RawAuth `yaml:"auth,omitempty"`
    ToolsInclude []string `yaml:"tools_include"`
}

type RawAuth struct {
    Type      string `yaml:"type"`
    SecretRef string `yaml:"secret_ref"`
}

type RawProfile struct {
    Tools map[string]RawToolFilter `yaml:"tools"`
    Auth  map[string]RawAuth       `yaml:"auth,omitempty"`
}

type RawToolFilter struct {
    Include  []string `yaml:"include,omitempty"`
    Optional bool     `yaml:"optional,omitempty"`
}

type RawKey struct {
    RoutingOverrides map[string]RawRoutingNode `yaml:"routing_overrides,omitempty"`
}

// Exactly one of Target, Chain, or Split must be set.
type RawRoutingNode struct {
    Target *RawRoutingTarget `yaml:"target,omitempty"`
    Chain  *RawChain         `yaml:"chain,omitempty"`
    Split  *RawSplit         `yaml:"split,omitempty"`
}

type RawChain struct {
    Retry    *RawRetry        `yaml:"retry,omitempty"`
    Children []RawRoutingNode `yaml:"children"`
}

type RawRetry struct {
    RetryOn         string `yaml:"retry_on,omitempty"`
    PerTryTimeoutMs int    `yaml:"per_try_timeout_ms,omitempty"`
}

type RawSplit struct {
    Children []RawSplitChild `yaml:"children"`
}

type RawSplitChild struct {
    Weight         int `yaml:"weight"`
    RawRoutingNode `yaml:",inline"`
}

type RawRoutingTarget struct {
    Provider string `yaml:"provider"`
    Name     string `yaml:"name,omitempty"`
}

// RawRateLimitTier defines a named rate-limit tier — a reusable set of limit
// values referenced by policy entries via the rule: field. Tiers have no
// Models filter; model applicability is controlled by the policy entry.
// Zero values mean unconstrained for that dimension.
type RawRateLimitTier struct {
    USDPerMinute decimal.Decimal `yaml:"usd_per_minute,omitempty"`
    USDPerHour   decimal.Decimal `yaml:"usd_per_hour,omitempty"`
    USDPerDay    decimal.Decimal `yaml:"usd_per_day,omitempty"`

    RPM int `yaml:"rpm,omitempty"`
    RPH int `yaml:"rph,omitempty"`
    RPD int `yaml:"rpd,omitempty"`

    InputTokensPerMinute int `yaml:"input_tokens_per_minute,omitempty"`
    InputTokensPerHour   int `yaml:"input_tokens_per_hour,omitempty"`
    InputTokensPerDay    int `yaml:"input_tokens_per_day,omitempty"`

    OutputTokensPerMinute int `yaml:"output_tokens_per_minute,omitempty"`
    OutputTokensPerHour   int `yaml:"output_tokens_per_hour,omitempty"`
    OutputTokensPerDay    int `yaml:"output_tokens_per_day,omitempty"`

    CacheReadTokensPerHour int `yaml:"cache_read_tokens_per_hour,omitempty"`
    CacheReadTokensPerDay  int `yaml:"cache_read_tokens_per_day,omitempty"`

    CacheWriteTokensPerHour int `yaml:"cache_write_tokens_per_hour,omitempty"`
    CacheWriteTokensPerDay  int `yaml:"cache_write_tokens_per_day,omitempty"`

    OnExceed string `yaml:"on_exceed,omitempty"`
}

// RawRateLimitPolicyEntry is one entry in a rate_limit.policies scope list.
// Rule names a tier from rate_limit.rules; its fields are used as the base,
// and any non-zero inline fields on this entry override the tier.
// Either Rule or inline fields (or both) must be set.
// Models must be non-empty; use ["*"] as a catch-all.
// All numeric fields default to zero, meaning unconstrained for that dimension.
type RawRateLimitPolicyEntry struct {
    Rule   string   `yaml:"rule,omitempty"` // names a RawRateLimitTier
    Models []string `yaml:"models"`

    USDPerMinute decimal.Decimal `yaml:"usd_per_minute,omitempty"`
    USDPerHour   decimal.Decimal `yaml:"usd_per_hour,omitempty"`
    USDPerDay    decimal.Decimal `yaml:"usd_per_day,omitempty"`

    RPM int `yaml:"rpm,omitempty"`
    RPH int `yaml:"rph,omitempty"`
    RPD int `yaml:"rpd,omitempty"`

    InputTokensPerMinute int `yaml:"input_tokens_per_minute,omitempty"`
    InputTokensPerHour   int `yaml:"input_tokens_per_hour,omitempty"`
    InputTokensPerDay    int `yaml:"input_tokens_per_day,omitempty"`

    OutputTokensPerMinute int `yaml:"output_tokens_per_minute,omitempty"`
    OutputTokensPerHour   int `yaml:"output_tokens_per_hour,omitempty"`
    OutputTokensPerDay    int `yaml:"output_tokens_per_day,omitempty"`

    CacheReadTokensPerHour int `yaml:"cache_read_tokens_per_hour,omitempty"`
    CacheReadTokensPerDay  int `yaml:"cache_read_tokens_per_day,omitempty"`

    CacheWriteTokensPerHour int `yaml:"cache_write_tokens_per_hour,omitempty"`
    CacheWriteTokensPerDay  int `yaml:"cache_write_tokens_per_day,omitempty"`

    OnExceed string `yaml:"on_exceed,omitempty"`
}
```

---

## 7. Routing Composition

Routing is a recursive tree of `target`, `chain`, and `split` nodes.

Rules:

- A routing node must set exactly one of `target`, `chain`, or `split`.
- `target` is a leaf: names a provider and an optional backend model name.
- `chain` tries children in order; stops at the first success. Carries an
  optional retry policy that maps to Envoy's retry semantics.
- `split` samples one child per request by weight (weights must sum to 100).
- `chain` may contain `target` or `split` children.
- `split` arms may contain `target` or `chain` children.
- Nested `split` inside `split` is not supported.
- Chain-of-chain is rejected at compile time unless the runtime gains explicit
  support for it.

### target — single hop

```yaml
routing_overrides:
  claude-haiku-4-5:
    target:
      provider: anthropic
      name: claude-haiku-4-5-20251001
```

### chain — ordered fallback

```yaml
routing_overrides:
  claude-haiku-4-5:
    chain:
      retry:
        retry_on: "connect-failure,reset,5xx"
        per_try_timeout_ms: 10000
      children:
        - target: { provider: fallback_p1, name: claude-haiku-4-5 }
        - target: { provider: fallback_p2, name: claude-haiku-4-5 }
        - target: { provider: vertex_anthropic, name: "claude-opus-4@20250514" }
```

### split — weighted traffic distribution

```yaml
routing_overrides:
  claude-haiku-4-5:
    split:
      children:
        - weight: 34
          target: { provider: split_p1, name: claude-haiku-4-5 }
        - weight: 33
          target: { provider: split_p2, name: claude-haiku-4-5 }
        - weight: 33
          target: { provider: split_p3, name: claude-haiku-4-5 }
```

### chain-of-split — primary split with a hard fallback

```yaml
routing_overrides:
  claude-haiku-4-5:
    chain:
      retry:
        retry_on: "connect-failure,reset,5xx"
        per_try_timeout_ms: 5000
      children:
        - split:
            children:
              - weight: 50
                target: { provider: split_p1 }
              - weight: 50
                target: { provider: split_p2 }
        - target: { provider: fallback_p1 }
```

### split-of-chains — independent chains per traffic arm

```yaml
routing_overrides:
  claude-haiku-4-5:
    split:
      children:
        - weight: 60
          chain:
            retry:
              retry_on: "connect-failure,reset,5xx"
              per_try_timeout_ms: 10000
            children:
              - target: { provider: split_p1 }
              - target: { provider: split_p2 }
        - weight: 40
          chain:
            retry:
              retry_on: "connect-failure,reset,5xx"
              per_try_timeout_ms: 8000
            children:
              - target: { provider: split_p2 }
              - target: { provider: split_p3 }
```

---

## 8. ID Convention

All user-owned records use:

```text
{workspace}/{user}/{name}
```

Rules:

- Exactly three segments separated by `/`.
- No segment may be empty.
- The ID is the only source of workspace and user — no body fields.
- Parsed segments are interned once and stored as `uint32` handles.

```text
Valid:
  demo/adi/sk-001        → workspace=demo  user=adi   name=sk-001
  acme/bot/default       → workspace=acme  user=bot   name=default
  org-a/alice/my-key     → workspace=org-a user=alice name=my-key

Invalid:
  demo/adi               → only 2 segments
  demo//sk-001           → empty user segment
  demo/adi/sk/extra      → 4 segments
  /adi/sk-001            → empty workspace segment
```

`rate_limit.policies` scope keys follow the same segment rules but allow 1, 2, or 3
segments. A 1-segment key is a workspace scope; 2-segment is a user scope;
3-segment is a key scope.

```go
type ParsedID struct {
    Workspace uint32
    User      uint32
    Name      uint32
    Raw       string
}

func parseID(id string, interns *InternPool) (ParsedID, error) {
    parts := strings.SplitN(id, "/", 3)
    if len(parts) != 3 {
        return ParsedID{}, fmt.Errorf(
            "invalid id %q: want workspace/user/name, got %d segment(s)",
            id, len(parts),
        )
    }
    for i, part := range parts {
        if part == "" {
            return ParsedID{}, fmt.Errorf(
                "invalid id %q: segment %d is empty", id, i,
            )
        }
    }
    return ParsedID{
        Workspace: interns.Intern(parts[0]),
        User:      interns.Intern(parts[1]),
        Name:      interns.Intern(parts[2]),
        Raw:       id,
    }, nil
}
```

---

## 9. InternPool

Repeated strings appear in every user record. Storing the raw strings wastes
memory and forces string comparisons on hot lookups.

`InternPool` gives each string a stable `uint32` handle. It is append-only and
never compacted, so user records can safely hold intern IDs across config
reloads.

```go
type InternPool struct {
    mu      sync.RWMutex
    strToID map[string]uint32
    idToStr []string
}

func NewInternPool() *InternPool {
    return &InternPool{strToID: make(map[string]uint32)}
}

func (p *InternPool) Intern(s string) uint32 {
    p.mu.RLock()
    if id, ok := p.strToID[s]; ok {
        p.mu.RUnlock()
        return id
    }
    p.mu.RUnlock()

    p.mu.Lock()
    defer p.mu.Unlock()

    if id, ok := p.strToID[s]; ok {
        return id
    }
    id := uint32(len(p.idToStr))
    p.strToID[s] = id
    p.idToStr = append(p.idToStr, s)
    return id
}

func (p *InternPool) Lookup(id uint32) string {
    p.mu.RLock()
    defer p.mu.RUnlock()
    if int(id) >= len(p.idToStr) {
        return ""
    }
    return p.idToStr[id]
}
```

---

## 10. Domain Structs

Domain structs differ from raw structs in three ways:

- References are resolved pointers, not strings.
- Strings become validated enum values.
- Endpoints and auth shapes are validated before publication.

`GlobalConfig` is the admin-owned tree for one snapshot generation. It may be
replaced often, but once published it must not be mutated.

```go
type ProviderKind string

const (
    ProviderKindAnthropic ProviderKind = "anthropic"
    ProviderKindOpenAI    ProviderKind = "openai"
    ProviderKindBedrock   ProviderKind = "bedrock"
)

type BackendSchema string

const (
    BackendSchemaGCPVertex    BackendSchema = "gcpvertexai"
    BackendSchemaGCPAnthropic BackendSchema = "gcpanthropic"
    BackendSchemaAWSBedrock   BackendSchema = "awsbedrock"
)

// AuthConfig carries an auth type and an opaque secret reference.
// SecretRef (env://, file://, literal://, or any backend scheme) is never
// resolved inside the snapshot. Callers pass it to a SecretResolver at
// request time so secrets rotate without a config reload (see §16).
type AuthConfig struct {
    Type      string
    SecretRef string
}

// ProviderRecord is the compiled, immutable form of one upstream LLM provider.
// SecretRef strings in Auth and Extra are kept as opaque references.
type ProviderRecord struct {
    Kind          ProviderKind
    BackendSchema BackendSchema
    Endpoint      string
    Auth          AuthConfig
    Extra         map[string]string
}

type ModelMetadata struct {
    Description   string
    ContextLength int
    MaxTokens     int
    Tags          []string
}

// ModelPricing holds per-model token prices in USD per million tokens.
// Nil means no pricing is defined; USD rate limit rules cannot target this model.
// Uses decimal.Decimal (github.com/shopspring/decimal) for exact monetary arithmetic.
type ModelPricing struct {
    InputMTok      decimal.Decimal
    OutputMTok     decimal.Decimal
    CacheReadMTok  decimal.Decimal
    CacheWriteMTok decimal.Decimal
}

// Cost returns the USD cost of one response given its token counts.
func (p *ModelPricing) Cost(inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int) decimal.Decimal {
    if p == nil {
        return decimal.Zero
    }
    M := decimal.NewFromInt(1_000_000)
    return decimal.NewFromInt(int64(inputTokens)).Mul(p.InputMTok).Div(M).
        Add(decimal.NewFromInt(int64(outputTokens)).Mul(p.OutputMTok).Div(M)).
        Add(decimal.NewFromInt(int64(cacheReadTokens)).Mul(p.CacheReadMTok).Div(M)).
        Add(decimal.NewFromInt(int64(cacheWriteTokens)).Mul(p.CacheWriteMTok).Div(M))
}

type ModelRecord struct {
    Provider          *ProviderRecord
    APIName           string
    EndpointOverrides map[string]*ProviderRecord
    Pricing           *ModelPricing
    Metadata          *ModelMetadata
}

type ServerRecord struct {
    Endpoint     string
    Namespace    string
    Auth         *AuthConfig
    ToolsInclude []string
}

// RateLimitRule is the compiled form of RawRateLimitPolicyEntry (after tier expansion).
type RateLimitRule struct {
    Models []string

    USDPerMinute decimal.Decimal
    USDPerHour   decimal.Decimal
    USDPerDay    decimal.Decimal

    RPM int
    RPH int
    RPD int

    InputTokensPerMinute  int
    InputTokensPerHour    int
    InputTokensPerDay     int

    OutputTokensPerMinute int
    OutputTokensPerHour   int
    OutputTokensPerDay    int

    CacheReadTokensPerHour  int
    CacheReadTokensPerDay   int

    CacheWriteTokensPerHour int
    CacheWriteTokensPerDay  int

    OnExceed string
}

func (r RateLimitRule) MatchesModel(modelID string) bool {
    for _, m := range r.Models {
        if m == "*" || m == modelID {
            return true
        }
    }
    return false
}

// GlobalConfig is the admin-owned configuration tree for one snapshot generation.
// RateLimit.Policies holds only workspace-scope (1-segment) and user-scope
// (2-segment) entries after tier expansion. Key-scope entries (3-segment) are
// user-managed and live in KeyRecord.RateLimitRules (see §11).
type GlobalConfig struct {
    Providers  map[string]*ProviderRecord
    Models     map[string]*ModelRecord
    Servers    map[string]*ServerRecord
    RateLimit  struct {
        Policies map[string][]RateLimitRule // workspace and user scopes only; tiers already expanded
    }
    Interns    *InternPool
}
```

Implementation rule: clone maps and slices while compiling. Never share mutable
raw maps or slices into a published `GlobalConfig`.

---

## 11. Minimal User Records

With potentially large numbers of records, every byte matters. Most records are
default. A key with no routing override stores only workspace/user/name intern
IDs and nil routing references. A profile pointing to a common tool filter stores
only intern IDs and a compact filter reference.

Minimal records do not store:

- Raw workspace or user strings (intern IDs instead).
- Embedded routing or tool filter configs (pool shape keys instead).
- Pointers into `GlobalConfig`.

In the data plane, user records are compiled from the snapshot payload during
`compile()` and stored in `ConfigSnapshot.Keys` / `ConfigSnapshot.Profiles`.
`AppState.ApplySnapshotEnvelope` seeds L2 from these maps atomically. Pool shape
keys are stable strings that survive snapshot reloads; L1 entries store fully
resolved data and the generation that produced them.

```go
type KeyRecord struct {
    Workspace uint32
    User      uint32
    Name      uint32

    // Per-model shape keys into RoutingPool. Absent model ID = use default
    // routing from GlobalConfig.Models.
    RoutingShapeKeys map[string]string

    // RateLimitRules holds key-scope rate-limit rules set by the key owner.
    // Applied after workspace and user-scope rules from GlobalConfig.RateLimit.Policies;
    // all three scopes accumulate — none short-circuits the others.
    RateLimitRules []RateLimitRule
}

type ProfileRecord struct {
    Workspace uint32
    User      uint32
    Name      uint32

    ToolFilterShapeKey string
    AuthShapeKey       string
}
```

**Rate-limit ownership.** Workspace and user-scope rules are admin-managed and
live in `GlobalConfig.RateLimit.Policies`. Key-scope rules are set by the key owner
(the user who holds the API key) and compile into `KeyRecord.RateLimitRules`.
The full accumulation order is:

```
GlobalConfig.ResolveRateLimitRules(keyID, modelID)   // workspace + user
  + filterRulesByModel(key.RateLimitRules, modelID)   // key scope
```

The admin can restrict what users may set on their own keys (approval flows,
quota caps) at the access-control layer — that enforcement is out of scope for
the compile pipeline itself.

---

## 12. Dedup Pools

Across millions of keys and profiles, the number of distinct routing shapes and
tool filter sets is small. Pools deduplicate them inside a snapshot.

Pool entries may hold pointers into `GlobalConfig`, so `Pools` and `GlobalConfig`
must always be published together in the same `ConfigSnapshot`.

```go
type RoutingKind string

const (
    RoutingKindTarget RoutingKind = "target"
    RoutingKindChain  RoutingKind = "chain"
    RoutingKindSplit  RoutingKind = "split"
)

type RoutingTarget struct {
    Provider  *ProviderRecord
    ModelName string
}

type RetryConfig struct {
    RetryOn         string
    PerTryTimeoutMs int
}

type RoutingConfig struct {
    Kind   RoutingKind
    Target *RoutingTarget
    Chain  *ChainConfig
    Split  *SplitConfig
}

type ChainConfig struct {
    Retry    *RetryConfig
    Children []RoutingConfig
}

type SplitConfig struct {
    Children []WeightedRoutingConfig
}

type WeightedRoutingConfig struct {
    Weight int
    Child  RoutingConfig
}

type ToolFilter struct {
    ServerID string
    Include  []string
    Optional bool
}

type RoutingPool struct {
    entries []RoutingConfig
    index   map[string]uint32
}

func (p *RoutingPool) Intern(shapeKey string, routing RoutingConfig) uint32 {
    if id, ok := p.index[shapeKey]; ok {
        return id
    }
    id := uint32(len(p.entries))
    p.entries = append(p.entries, routing)
    p.index[shapeKey] = id
    return id
}

func (p *RoutingPool) Get(id uint32) *RoutingConfig {
    if int(id) >= len(p.entries) {
        return nil
    }
    return &p.entries[id]
}

type ToolFilterPool struct {
    entries [][]ToolFilter
    index   map[string]uint32
}

type AuthOverride struct {
    ServerID string
    Auth     AuthConfig
}

type AuthPool struct {
    entries [][]AuthOverride
    index   map[string]uint32
}

type Pools struct {
    Routing     *RoutingPool
    ToolFilters *ToolFilterPool
    Auth        *AuthPool
}
```

---

## 13. Compile

Compilation has four phases:

1. Build leaf nodes: providers and servers (no cross-references).
2. Build dependent nodes: models — resolve provider pointers and compile pricing.
3. Compile **admin-scope** rate-limit rules (1-segment workspace and 2-segment
   user scopes only) — validate USD dependencies against compiled models.
   Key-scope entries (3 segments) are skipped here and handled in Phase 4.
4. Compile user records from the snapshot payload: routing shapes and tool-filter
   sets are interned into Pools; key-scope rate-limit rules from `raw.RateLimit.Policies`
   are compiled into `KeyRecord.RateLimitRules`. Publish `ConfigSnapshot` only if
   all validation succeeds. Keys and Profiles are `nil` when absent from the payload.

```go
type ConfigSnapshot struct {
    Generation uint64
    Global     *GlobalConfig
    Pools      *Pools
    Keys       map[string]*KeyRecord     // compiled from snapshot payload; nil if absent
    Profiles   map[string]*ProfileRecord // compiled from snapshot payload; nil if absent
}

func compile(
    raw *RawConfig,
    interns *InternPool,
    generation uint64,
) (*ConfigSnapshot, error) {
    // Phase 1: leaf nodes
    providers := make(map[string]*ProviderRecord, len(raw.LLM.Providers))
    for id, r := range raw.LLM.Providers {
        kind := ProviderKind(r.Kind)
        if kind != ProviderKindAnthropic &&
            kind != ProviderKindOpenAI &&
            kind != ProviderKindBedrock {
            return nil, fmt.Errorf("provider %q: unknown kind %q", id, r.Kind)
        }
        providers[id] = &ProviderRecord{
            Kind:          kind,
            BackendSchema: BackendSchema(r.BackendSchema),
            Endpoint:      r.Endpoint,
            Auth:          AuthConfig(r.Auth),
            Extra:         cloneStringMap(r.Extra),
        }
    }

    servers := make(map[string]*ServerRecord, len(raw.MCP.Servers))
    for id, r := range raw.MCP.Servers {
        var auth *AuthConfig
        if r.Auth != nil {
            v := AuthConfig(*r.Auth)
            auth = &v
        }
        servers[id] = &ServerRecord{
            Endpoint:     r.Endpoint,
            Namespace:    r.Namespace,
            Auth:         auth,
            ToolsInclude: slices.Clone(r.ToolsInclude),
        }
    }

    // Phase 2: models
    models := make(map[string]*ModelRecord, len(raw.LLM.Models))
    for id, r := range raw.LLM.Models {
        provider, ok := providers[r.Provider]
        if !ok {
            return nil, fmt.Errorf("model %q: unknown provider %q", id, r.Provider)
        }
        overrides := make(map[string]*ProviderRecord, len(r.EndpointOverrides))
        for op, provID := range r.EndpointOverrides {
            ep, ok := providers[provID]
            if !ok {
                return nil, fmt.Errorf(
                    "model %q endpoint_override %q: unknown provider %q", id, op, provID,
                )
            }
            overrides[op] = ep
        }
        apiName := r.Name
        if apiName == "" {
            apiName = id
        }
        models[id] = &ModelRecord{
            Provider:          provider,
            APIName:           apiName,
            EndpointOverrides: overrides,
            Pricing:           compilePricing(r.Pricing),
            Metadata:          compileMetadata(r.Metadata),
        }
    }

    // Phase 3: rate limits — expand tier references, then compile policy entries.
    rateLimits := make(map[string][]RateLimitRule, len(raw.RateLimit.Policies))
    for scope, rawRules := range raw.RateLimit.Policies {
        if err := validateScopeKey(scope); err != nil {
            return nil, fmt.Errorf("rate_limit.policies[%q]: %w", scope, err)
        }
        rules := make([]RateLimitRule, len(rawRules))
        for i, r := range rawRules {
            if r.Rule != "" {
                if t, ok := raw.RateLimit.Tiers[r.Rule]; ok {
                    r = applyTier(r, t)
                }
            }
            if len(r.Models) == 0 {
                return nil, fmt.Errorf("rate_limit.policies[%q][%d]: models must not be empty", scope, i)
            }
            if err := validateUSDDependency(r, models, scope, i); err != nil {
                return nil, err
            }
            rules[i] = compileRateLimitRule(r)
        }
        rateLimits[scope] = rules
    }

    global := &GlobalConfig{
        Providers:  providers,
        Models:     models,
        Servers:    servers,
        RateLimits: rateLimits,
        Interns:    interns,
    }
    pools := &Pools{
        Routing:     &RoutingPool{index: make(map[string]uint32)},
        ToolFilters: &ToolFilterPool{index: make(map[string]uint32)},
        Auth:        &AuthPool{index: make(map[string]uint32)},
    }

    // Phase 4: compile user records from snapshot payload.
    // Routing shapes and tool filter sets are interned into pools here so that
    // L1 resolution is a cheap pool lookup. Records absent from the payload
    // (production: delivered separately) result in nil maps.
    compiledKeys := make(map[string]*KeyRecord, len(raw.Keys))
    for id, rawKey := range raw.Keys {
        pid, err := parseID(id, interns)
        if err != nil {
            return nil, fmt.Errorf("keys[%q]: %w", id, err)
        }
        shapeKeys := make(map[string]string, len(rawKey.RoutingOverrides))
        for modelID, rawNode := range rawKey.RoutingOverrides {
            routing, shapeKey, err := compileRoutingNode(rawNode, providers)
            if err != nil {
                return nil, fmt.Errorf("keys[%q].routing_overrides[%q]: %w", id, modelID, err)
            }
            pools.Routing.Intern(shapeKey, routing)
            shapeKeys[modelID] = shapeKey
        }
        compiledKeys[id] = &KeyRecord{
            Workspace:        pid.Workspace,
            User:             pid.User,
            Name:             pid.Name,
            RoutingShapeKeys: shapeKeys,
        }
    }

    compiledProfiles := make(map[string]*ProfileRecord, len(raw.Profiles))
    for id, rawProfile := range raw.Profiles {
        pid, err := parseID(id, interns)
        if err != nil {
            return nil, fmt.Errorf("profiles[%q]: %w", id, err)
        }
        toolShapeKey, authShapeKey, err := compileProfileShapes(rawProfile, servers, pools)
        if err != nil {
            return nil, fmt.Errorf("profiles[%q]: %w", id, err)
        }
        compiledProfiles[id] = &ProfileRecord{
            Workspace:          pid.Workspace,
            User:               pid.User,
            Name:               pid.Name,
            ToolFilterShapeKey: toolShapeKey,
            AuthShapeKey:       authShapeKey,
        }
    }

    return &ConfigSnapshot{
        Generation: generation,
        Global:     global,
        Pools:      pools,
        Keys:       compiledKeys,
        Profiles:   compiledProfiles,
    }, nil
}

func validateUSDDependency(r RawRateLimitPolicyEntry, models map[string]*ModelRecord, scope string, i int) error {
    if r.USDPerMinute.IsZero() && r.USDPerHour.IsZero() && r.USDPerDay.IsZero() {
        return nil
    }
    check := func(modelID string) error {
        m, ok := models[modelID]
        if !ok {
            return fmt.Errorf("rate_limit.policies[%q][%d]: model %q not found", scope, i, modelID)
        }
        if m.Pricing == nil {
            return fmt.Errorf(
                "rate_limit.policies[%q][%d]: usd limit requires pricing block on model %q",
                scope, i, modelID,
            )
        }
        return nil
    }
    if len(r.Models) == 1 && r.Models[0] == "*" {
        for modelID := range models {
            if err := check(modelID); err != nil {
                return err
            }
        }
        return nil
    }
    for _, modelID := range r.Models {
        if modelID == "*" {
            continue
        }
        if err := check(modelID); err != nil {
            return err
        }
    }
    return nil
}

func validateScopeKey(scope string) error {
    parts := strings.SplitN(scope, "/", 4)
    if len(parts) > 3 {
        return fmt.Errorf("too many segments (max 3: workspace/user/name)")
    }
    for i, p := range parts {
        if p == "" {
            return fmt.Errorf("segment %d is empty", i)
        }
    }
    return nil
}
```

---

## 14. AppState and Hot Reload

`AppState` is the boundary between dynamic control-plane changes and lock-free
request handling. Writers build complete snapshots off to the side and publish
them through `AppState`; readers only load the current snapshot and never observe
partially compiled state.

One atomic pointer covers the full snapshot. This prevents a reader from ever
seeing a new `GlobalConfig` paired with old pools, or vice versa.

Reload sequence:

1. Decode and compile the new snapshot on a background goroutine.
2. If compile fails, return an error and leave the old snapshot live.
3. Seed user tables from the new snapshot (atomically replaces L2, purges L1).
4. Atomically store the new `*ConfigSnapshot`.
5. Inflight requests finish with whichever snapshot they loaded.

```go
type AppState struct {
    snapshot atomic.Pointer[ConfigSnapshot]

    keys     *UserTable[*KeyRecord, ResolvedKey]
    profiles *UserTable[*ProfileRecord, ResolvedProfile]

    interns     *InternPool
    generation  atomic.Uint64
    lastVersion atomic.Uint64
}

func NewAppState() *AppState {
    return &AppState{
        keys:     NewUserTable[*KeyRecord, ResolvedKey](),
        profiles: NewUserTable[*ProfileRecord, ResolvedProfile](),
        interns:  NewInternPool(),
    }
}

func (s *AppState) ApplySnapshotEnvelope(envelope SnapshotEnvelope) error {
    if envelope.Version > 0 && envelope.Version <= s.lastVersion.Load() {
        return nil // stale SoTW payload; keep current snapshot live
    }

    raw, err := decodeRawConfig(envelope)
    if err != nil {
        return fmt.Errorf("decode: %w", err)
    }

    generation := s.generation.Add(1)
    snapshot, err := compile(raw, s.interns, generation)
    if err != nil {
        return fmt.Errorf("compile: %w", err)
    }

    // Seed tables before publishing the snapshot so readers never see a new
    // snapshot paired with stale L2 data.
    s.keys.Seed(snapshot.Keys)
    s.profiles.Seed(snapshot.Profiles)
    s.snapshot.Store(snapshot)
    if envelope.Version > 0 {
        s.lastVersion.Store(envelope.Version)
    }

    return nil
}

func (s *AppState) LoadConfig(yamlBytes []byte) error {
    return s.ApplySnapshotEnvelope(SnapshotEnvelope{
        Format:      SnapshotFormatYAML,
        Compression: CompressionNone,
        Payload:     yamlBytes,
    })
}

func (s *AppState) Resolve(keyID, profileID string) (*ResolvedRequest, error) {
    snapshot := s.snapshot.Load()
    if snapshot == nil {
        return nil, errors.New("config is not loaded")
    }
    key, err := s.keys.Get(keyID, snapshot, resolveKey)
    if err != nil {
        return nil, err
    }
    profile, err := s.profiles.Get(profileID, snapshot, resolveProfile)
    if err != nil {
        return nil, err
    }
    return &ResolvedRequest{Key: key, Profile: profile}, nil
}
```

---

## 15. UserTable — L1 / L2

The data plane never contacts the database directly; all user records arrive via
the snapshot. Two tiers suffice:

| Tier | Contents | Approx cost |
|------|----------|-------------|
| L1 | Fully resolved record + snapshot generation | ~100 ns |
| L2 | Minimal record (atomic immutable map, seeded from snapshot) | ~1 µs |

**L1 eviction is benign.** L2 always holds the complete record set from the last
snapshot, so an evicted L1 entry is re-resolved from L2 on the next access with
no external call.

**Refresh = new snapshot.** The only way records change is through a new snapshot
delivery. `Seed` atomically replaces the entire L2 map and purges L1; there is no
per-record invalidation path on the data plane.

`Seed` is called by `ApplySnapshotEnvelope` before the new `*ConfigSnapshot` is
published, so readers never observe a new snapshot paired with stale L2 data.

```go
type ResolvedEntry[R any] struct {
    Generation uint64
    Value      R
}

type ResolvedKey struct {
    Workspace        string
    User             string
    RoutingOverrides map[string]*RoutingConfig // model ID → routing; absent = use global default
}

type ResolvedProfile struct {
    Workspace   string
    User        string
    ToolFilters []ToolFilter
    AuthOverrides []AuthOverride
}

// UserTable[M, R] is generic over the minimal L2 type M and the resolved L1 type R.
type UserTable[M, R any] struct {
    l1 *lru.Cache[string, ResolvedEntry[R]]
    l2 atomic.Pointer[map[string]M]
}

func NewUserTable[M, R any]() *UserTable[M, R] {
    cache, _ := lru.New[string, ResolvedEntry[R]](100_000)
    return &UserTable[M, R]{l1: cache}
}

// Seed atomically replaces L2 with records and purges L1.
// Called by AppState.ApplySnapshotEnvelope on every snapshot apply.
func (t *UserTable[M, R]) Seed(records map[string]M) {
    t.l2.Store(&records)
    t.l1.Purge()
}

func (t *UserTable[M, R]) Get(
    id string,
    snapshot *ConfigSnapshot,
    resolve func(M, *ConfigSnapshot) (R, error),
) (R, error) {
    if entry, ok := t.l1.Get(id); ok && entry.Generation == snapshot.Generation {
        return entry.Value, nil
    }
    m2 := t.l2.Load()
    if m2 == nil {
        var zero R
        return zero, fmt.Errorf("record %q not found", id)
    }
    rec, ok := (*m2)[id]
    if !ok {
        var zero R
        return zero, fmt.Errorf("record %q not found", id)
    }
    v, err := resolve(rec, snapshot)
    if err != nil {
        var zero R
        return zero, err
    }
    t.l1.Add(id, ResolvedEntry[R]{Generation: snapshot.Generation, Value: v})
    return v, nil
}
```

---

## 16. SecretResolver

`AuthConfig.SecretRef` is an opaque reference string that travels inside the
snapshot without ever being resolved. Resolution happens at request time via a
`SecretResolver`, which is injected into the handler that needs the credential.
This means secrets rotate without a config reload: the secret-service sends a
webhook → handler calls `Invalidate` → next request fetches the fresh value.

```go
// SecretResolver resolves an opaque secret reference to its plaintext value.
// Implementations must be safe for concurrent use.
type SecretResolver interface {
    Resolve(ctx context.Context, ref string) (string, error)
    Invalidate(ref string)
}
```

**Built-in resolvers** (all stateless, `Invalidate` is a no-op):

| Scheme | Example ref | Behaviour |
|--------|-------------|-----------|
| `env://` | `env://ANTHROPIC_API_KEY` | `os.LookupEnv` at call time |
| `file://` | `file:///run/secrets/token` | `os.ReadFile`, whitespace trimmed |
| `literal://` | `literal://hardcoded` | Returns value verbatim; dev/test only |

**`DispatchResolver`** routes each ref to the resolver registered for its scheme.
Construct one via `NewDispatchResolver(map[string]SecretResolver{...})`.

**`CachedResolver`** (backed by `github.com/hashicorp/golang-lru/v2/expirable`)
wraps any inner resolver with TTL-based caching and explicit `Invalidate`:

```go
func NewCachedResolver(inner SecretResolver, ttl time.Duration, maxEntries int) *CachedResolver
```

Errors are not cached — a failing inner call is retried on the next request.

**`NewDefaultResolver(ttl, maxEntries)`** returns a `CachedResolver` that
dispatches across the three built-in schemes. For production, build a custom
`DispatchResolver` that includes a SecretService backend and pass it to
`NewCachedResolver`.

---

## 17. Rate Limit Resolution

Rate limits are resolved at request time after the key and model are known.
Rules from all three scopes accumulate — none short-circuits the others.

```go
// ResolveRateLimitRules returns all policy entries that apply to the given
// key+model combination, ordered workspace → user → key. Tier references
// are already expanded by compile(); entries are ready for enforcement.
func (g *GlobalConfig) ResolveRateLimitRules(keyID, modelID string) []RateLimitRule {
    parts := strings.SplitN(keyID, "/", 3)
    if len(parts) != 3 {
        return nil
    }
    var result []RateLimitRule
    result = append(result, filterRulesByModel(g.RateLimit.Policies[parts[0]], modelID)...)
    result = append(result, filterRulesByModel(g.RateLimit.Policies[parts[0]+"/"+parts[1]], modelID)...)
    result = append(result, filterRulesByModel(g.RateLimit.Policies[keyID], modelID)...)
    return result
}

func filterRulesByModel(rules []RateLimitRule, modelID string) []RateLimitRule {
    var result []RateLimitRule
    for _, r := range rules {
        if r.MatchesModel(modelID) {
            result = append(result, r)
        }
    }
    return result
}
```

### Tier expansion

Before compile stores policy entries into `GlobalConfig`, it expands tier
references. For each `RawRateLimitPolicyEntry` with a non-empty `Rule` field,
the named `RawRateLimitTier` is looked up from `raw.RateLimit.Tiers` and merged
in — entry inline fields take precedence, tier fields fill in zeros:

```go
func applyTier(entry RawRateLimitPolicyEntry, tier RawRateLimitTier) RawRateLimitPolicyEntry {
    if entry.USDPerDay.IsZero() { entry.USDPerDay = tier.USDPerDay }
    if entry.RPM == 0           { entry.RPM = tier.RPM }
    // ... all other fields
    return entry
}
```

The proto decode path produces entries with `Rule: ""` — orange CP expands tiers
server-side before encoding the snapshot proto. `Tiers` is always empty on the
wire; it is a YAML-authoring convenience only.

### Accumulation walkthrough

Given this config:

```yaml
rate_limit:
  rules:
    standard:
      usd_per_day: 200.00
      rpm: 100
    premium:
      usd_per_day: 1_000.00
      rpm: 500

  policies:
    demo:
      - rule: premium
        models: ["*"]

    demo/adi:
      - rule: standard
        models: ["*"]

    demo/adi/sk-direct:
      - models: [claude-haiku-4-5, gpt-4o-mini]
        usd_per_hour: 5.00
        input_tokens_per_hour: 800_000
      - models: ["*"]
        rpm: 20
```

A request from key `demo/adi/sk-direct` calling `claude-haiku-4-5`:

```text
1. workspace "demo"          → rule matches ["*"]  → usd_per_day, rpm checked
2. user "demo/adi"           → rule matches ["*"]  → usd_per_day, rpm checked
3. key "demo/adi/sk-direct"  → first rule matches  → usd_per_hour, input_tokens_per_hour checked
                              → second rule ["*"]   → rpm: 20 checked
```

All four checks must pass. The request is rejected if any counter is exhausted.

### Counter storage

`GlobalConfig.RateLimit.Policies` carries only policy definitions. Actual counters are
owned by the enforcement layer: a sidecar, Redis cluster, or in-process atomics
depending on deployment. The config system does not track enforcement state.

### USD cost computation

```go
cost := model.Pricing.Cost(
    resp.Usage.InputTokens,
    resp.Usage.OutputTokens,
    resp.Usage.CacheReadTokens,
    resp.Usage.CacheWriteTokens,
)
// cost is debited against usd_per_hour, usd_per_day, usd_per_minute
// counters in all matching rules returned by ResolveRateLimitRules.
```

---

## 18. Benchmark Suite

The interesting question is not raw proto decode throughput in isolation, but
**end-to-end snapshot pipeline latency** — decompress → unmarshal → `protoToRaw`
→ `compile()` — because that is the wall-clock pause between a control-plane
push and the new config going live.

### What to measure

| Benchmark | Why |
|-----------|-----|
| `BenchmarkDecodeProto` | `proto.Unmarshal` only — isolates the library |
| `BenchmarkDecodeYAML` | baseline comparison; YAML is the dev path |
| `BenchmarkDecompressZstd` | zstd decompress only — is the CPU cost worth the wire savings? |
| `BenchmarkProtoToRaw` | string-table expansion cost |
| `BenchmarkCompile` | validation + pointer resolution + pool build |
| `BenchmarkFullPipeline` | the number that actually matters |
| `BenchmarkFullPipelineParallel` | concurrent reload contention on `AppState` |

### Fixture scales

Three deterministic fixture sizes, built once in `TestMain`:

| Name | Providers | Models | Servers | Profiles | Keys | RL scopes |
|------|-----------|--------|---------|----------|------|-----------|
| `small` | 3 | 8 | 4 | 50 | 50 | 10 |
| `medium` | 10 | 40 | 15 | 500 | 500 | 50 |
| `large` | 25 | 100 | 30 | 5000 | 5000 | 200 |

`large` is the realistic production ceiling for admin-owned sections. User
records at millions of rows live in the DB, not in snapshots.

### `benchmark_test.go`

```go
package config_test

import (
    "bytes"
    "crypto/sha256"
    "fmt"
    "testing"

    "github.com/klauspost/compress/zstd"
    "google.golang.org/protobuf/proto"
    "gopkg.in/yaml.v3"

    configpb "yourrepo/internal/config/proto"
    "yourrepo/internal/config"
)

// ── Fixture scales ────────────────────────────────────────────────────────────

type fixtureScale struct {
    name      string
    providers int
    models    int
    servers   int
    profiles  int
    keys      int
    rlScopes  int
}

var scales = []fixtureScale{
    {"small",  3,   8,   4,   50,   50,  10},
    {"medium", 10,  40,  15,  500,  500,  50},
    {"large",  25, 100,  30, 5000, 5000, 200},
}

// ── String table builder (test-only) ─────────────────────────────────────────

type stringTableBuilder struct {
    table  []string
    lookup map[string]uint32
}

func (b *stringTableBuilder) intern(s string) uint32 {
    if b.lookup == nil {
        b.lookup = make(map[string]uint32)
        b.table = append(b.table, "") // index 0 reserved
    }
    if id, ok := b.lookup[s]; ok {
        return id
    }
    id := uint32(len(b.table))
    b.table = append(b.table, s)
    b.lookup[s] = id
    return id
}

// ── Fixture construction ──────────────────────────────────────────────────────

func buildProtoPayload(s fixtureScale) *configpb.ConfigPayload {
    st := &stringTableBuilder{}
    p := &configpb.ConfigPayload{Strings: &configpb.StringTable{}}

    for i := range s.providers {
        name     := fmt.Sprintf("provider-%02d", i)
        endpoint := fmt.Sprintf("https://api.provider-%02d.example.com", i)
        secret   := fmt.Sprintf("env://PROVIDER_%02d_KEY", i)
        p.Providers = append(p.Providers, &configpb.Provider{
            NameIdx:     st.intern(name),
            Kind:        configpb.ProviderKind_PROVIDER_KIND_ANTHROPIC,
            EndpointIdx: st.intern(endpoint),
            Auth: &configpb.Auth{
                Type:      configpb.AuthType_AUTH_TYPE_ANTHROPIC,
                SecretIdx: st.intern(secret),
            },
        })
    }

    for i := range s.models {
        name := fmt.Sprintf("model-%03d", i)
        prov := fmt.Sprintf("provider-%02d", i%s.providers)
        p.Models = append(p.Models, &configpb.Model{
            NameIdx:     st.intern(name),
            ProviderIdx: st.intern(prov),
            Pricing: &configpb.ModelPricing{
                InputMtok:  0.80,
                OutputMtok: 4.00,
            },
        })
    }

    for i := range s.servers {
        name  := fmt.Sprintf("server-%02d", i)
        ep    := fmt.Sprintf("https://mcp.server-%02d.example.com", i)
        ns    := fmt.Sprintf("ns%02d", i)
        tools := make([]uint32, 5)
        for j := range tools {
            tools[j] = st.intern(fmt.Sprintf("tool-%02d-%02d", i, j))
        }
        p.Servers = append(p.Servers, &configpb.Server{
            NameIdx:          st.intern(name),
            EndpointIdx:      st.intern(ep),
            NamespaceIdx:     st.intern(ns),
            ToolsIncludeIdxs: tools,
        })
    }

    for i := range s.profiles {
        ws  := fmt.Sprintf("ws%03d", i%10)
        usr := fmt.Sprintf("user%04d", i)
        id  := fmt.Sprintf("%s/%s/default", ws, usr)
        s0  := fmt.Sprintf("server-%02d", i%s.servers)
        s1  := fmt.Sprintf("server-%02d", (i+1)%s.servers)
        p.Profiles = append(p.Profiles, &configpb.Profile{
            IdIdx: st.intern(id),
            Tools: []*configpb.ToolFilter{
                {
                    ServerIdx:   st.intern(s0),
                    IncludeIdxs: []uint32{st.intern(fmt.Sprintf("tool-%02d-00", i%s.servers))},
                },
                {
                    ServerIdx: st.intern(s1),
                    Optional:  true,
                },
            },
        })
    }

    for i := range s.keys {
        ws    := fmt.Sprintf("ws%03d", i%10)
        usr   := fmt.Sprintf("user%04d", i)
        id    := fmt.Sprintf("%s/%s/sk-%04d", ws, usr, i)
        model := fmt.Sprintf("model-%03d", i%s.models)
        prov  := fmt.Sprintf("provider-%02d", i%s.providers)
        p.Keys = append(p.Keys, &configpb.Key{
            IdIdx: st.intern(id),
            RoutingOverrides: []*configpb.RoutingOverride{
                {
                    ModelIdx: st.intern(model),
                    Node: &configpb.RoutingNode{
                        Kind: &configpb.RoutingNode_Target{
                            Target: &configpb.RoutingTarget{
                                ProviderIdx: st.intern(prov),
                            },
                        },
                    },
                },
            },
        })
    }

    for i := range s.rlScopes {
        var scope string
        switch i % 3 {
        case 0:
            scope = fmt.Sprintf("ws%03d", i%10)
        case 1:
            scope = fmt.Sprintf("ws%03d/user%04d", i%10, i)
        case 2:
            scope = fmt.Sprintf("ws%03d/user%04d/sk-%04d", i%10, i, i)
        }
        model := fmt.Sprintf("model-%03d", i%s.models)
        p.RateLimits = append(p.RateLimits, &configpb.RateLimitScope{
            ScopeIdx: st.intern(scope),
            Rules: []*configpb.RateLimitRule{
                {
                    ModelIdxs: []uint32{st.intern(model)},
                    UsdPerDay: 200.0,
                    Rpm:       100,
                },
            },
        })
    }

    p.Strings.Strings = st.table
    return p
}

// ── Pre-built fixtures ────────────────────────────────────────────────────────

type benchFixture struct {
    scale        fixtureScale
    protoPayload *configpb.ConfigPayload
    protoBytes   []byte  // proto-marshalled ConfigPayload (uncompressed)
    protoZstd    []byte  // zstd-compressed proto bytes
    yamlBytes    []byte  // equivalent RawConfig as YAML
    yamlZstd     []byte
    checksum     []byte  // SHA-256 of protoBytes
}

var fixtures []*benchFixture

func TestMain(m *testing.M) {
    enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
    defer enc.Close()

    for _, s := range scales {
        f := &benchFixture{scale: s}
        f.protoPayload = buildProtoPayload(s)

        var err error
        f.protoBytes, err = proto.Marshal(f.protoPayload)
        if err != nil {
            panic(err)
        }
        sum := sha256.Sum256(f.protoBytes)
        f.checksum = sum[:]
        f.protoZstd = enc.EncodeAll(f.protoBytes, nil)

        raw := config.ProtoPayloadToRawConfig(f.protoPayload) // test export
        f.yamlBytes, err = yaml.Marshal(raw)
        if err != nil {
            panic(err)
        }
        f.yamlZstd = enc.EncodeAll(f.yamlBytes, nil)
    }
    m.Run()
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func zstdDecompress(src []byte) ([]byte, error) {
    dec, err := zstd.NewReader(bytes.NewReader(src))
    if err != nil {
        return nil, err
    }
    defer dec.Close()
    var buf bytes.Buffer
    _, err = buf.ReadFrom(dec)
    return buf.Bytes(), err
}

// ── Benchmarks ────────────────────────────────────────────────────────────────

// BenchmarkDecompressZstd measures zstd decompression cost alone.
// Answers: is zstd CPU overhead justified by wire savings?
func BenchmarkDecompressZstd(b *testing.B) {
    for _, f := range fixtures {
        b.Run(f.scale.name, func(b *testing.B) {
            b.SetBytes(int64(len(f.protoZstd)))
            b.ReportMetric(
                float64(len(f.protoBytes))/float64(len(f.protoZstd)), "ratio")
            b.ResetTimer()
            for range b.N {
                out, err := zstdDecompress(f.protoZstd)
                if err != nil {
                    b.Fatal(err)
                }
                _ = out
            }
        })
    }
}

// BenchmarkDecodeProto measures proto.Unmarshal alone.
func BenchmarkDecodeProto(b *testing.B) {
    for _, f := range fixtures {
        b.Run(f.scale.name, func(b *testing.B) {
            b.SetBytes(int64(len(f.protoBytes)))
            b.ResetTimer()
            for range b.N {
                var pb configpb.ConfigPayload
                if err := proto.Unmarshal(f.protoBytes, &pb); err != nil {
                    b.Fatal(err)
                }
            }
        })
    }
}

// BenchmarkDecodeYAML measures yaml.Unmarshal into RawConfig alone.
func BenchmarkDecodeYAML(b *testing.B) {
    for _, f := range fixtures {
        b.Run(f.scale.name, func(b *testing.B) {
            b.SetBytes(int64(len(f.yamlBytes)))
            b.ResetTimer()
            for range b.N {
                var raw config.RawConfig
                if err := yaml.Unmarshal(f.yamlBytes, &raw); err != nil {
                    b.Fatal(err)
                }
            }
        })
    }
}

// BenchmarkProtoToRaw measures string-table expansion from proto to RawConfig.
func BenchmarkProtoToRaw(b *testing.B) {
    for _, f := range fixtures {
        // Pre-unmarshal so we isolate only the expansion step.
        var pb configpb.ConfigPayload
        if err := proto.Unmarshal(f.protoBytes, &pb); err != nil {
            b.Fatal(err)
        }
        b.Run(f.scale.name, func(b *testing.B) {
            b.ResetTimer()
            for range b.N {
                raw, err := config.ProtoToRaw(&pb)
                if err != nil {
                    b.Fatal(err)
                }
                _ = raw
            }
        })
    }
}

// BenchmarkCompile measures compile() given a pre-expanded RawConfig.
func BenchmarkCompile(b *testing.B) {
    for _, f := range fixtures {
        raw, err := config.ProtoToRaw(f.protoPayload)
        if err != nil {
            b.Fatal(err)
        }
        b.Run(f.scale.name, func(b *testing.B) {
            b.ResetTimer()
            for i := range b.N {
                snap, err := config.Compile(raw, config.NewInternPool(), uint64(i))
                if err != nil {
                    b.Fatal(err)
                }
                _ = snap
            }
        })
    }
}

// BenchmarkFullPipeline measures the complete path a snapshot envelope travels:
// decompress -> checksum -> proto.Unmarshal -> protoToRaw -> compile.
// This is the wall-clock latency between a control-plane push and the new
// config going live.
func BenchmarkFullPipeline(b *testing.B) {
    for _, f := range fixtures {
        envelope := config.SnapshotEnvelope{
            Version:     1,
            Format:      config.SnapshotFormatProto,
            Compression: config.CompressionZstd,
            Payload:     f.protoZstd,
            Checksum:    f.checksum,
        }
        interns := config.NewInternPool()
        b.Run(f.scale.name, func(b *testing.B) {
            b.SetBytes(int64(len(f.protoZstd)))
            b.ResetTimer()
            for i := range b.N {
                raw, err := config.DecodeRawConfig(envelope)
                if err != nil {
                    b.Fatal(err)
                }
                snap, err := config.Compile(raw, interns, uint64(i))
                if err != nil {
                    b.Fatal(err)
                }
                _ = snap
            }
        })
    }
}

// BenchmarkFullPipelineYAML is the YAML equivalent of BenchmarkFullPipeline.
// Use to compare decode cost between formats at each scale.
func BenchmarkFullPipelineYAML(b *testing.B) {
    for _, f := range fixtures {
        envelope := config.SnapshotEnvelope{
            Version:     1,
            Format:      config.SnapshotFormatYAML,
            Compression: config.CompressionNone,
            Payload:     f.yamlBytes,
        }
        interns := config.NewInternPool()
        b.Run(f.scale.name, func(b *testing.B) {
            b.SetBytes(int64(len(f.yamlBytes)))
            b.ResetTimer()
            for i := range b.N {
                raw, err := config.DecodeRawConfig(envelope)
                if err != nil {
                    b.Fatal(err)
                }
                snap, err := config.Compile(raw, interns, uint64(i))
                if err != nil {
                    b.Fatal(err)
                }
                _ = snap
            }
        })
    }
}

// BenchmarkFullPipelineParallel measures ApplySnapshotEnvelope under concurrent
// load. The atomic store is cheap but the intern pool uses a RWMutex; this
// surfaces any contention when multiple goroutines reload simultaneously.
func BenchmarkFullPipelineParallel(b *testing.B) {
    for _, f := range fixtures {
        envelope := config.SnapshotEnvelope{
            Version:     1,
            Format:      config.SnapshotFormatProto,
            Compression: config.CompressionZstd,
            Payload:     f.protoZstd,
            Checksum:    f.checksum,
        }
        b.Run(f.scale.name, func(b *testing.B) {
            app := config.NewAppState()
            b.ResetTimer()
            b.RunParallel(func(pb *testing.PB) {
                for pb.Next() {
                    if err := app.ApplySnapshotEnvelope(envelope); err != nil {
                        b.Fatal(err)
                    }
                }
            })
        })
    }
}
```

### Reading the results

Run with:

```bash
go test ./internal/config/... -bench=. -benchmem -count=6 | tee bench.txt
benchstat bench.txt
```

The numbers to watch:

| Signal | Meaning |
|--------|---------|
| `BenchmarkFullPipeline/large` ns/op | Upper bound on reload latency; should be well under 100 ms to be imperceptible |
| `BenchmarkDecompressZstd ratio` | Typical 4–8× reduction; if ratio < 2 for a given scale, zstd is not worth the CPU |
| Proto ns/op vs YAML ns/op at same scale | Justify the added toolchain complexity of proto |
| `BenchmarkFullPipelineParallel` vs `BenchmarkFullPipeline` | Should be similar; divergence indicates InternPool lock contention |
| `allocs/op` in `BenchmarkCompile` | Should be O(providers + models + servers), not O(profiles + keys) |

---

## 19. Postgres Schema

Three concerns map to three schema groups:

- **Admin snapshot store** — versioned, pre-compiled envelope blobs. The control
  plane writes here; proxies read and stream from here.
- **User record tables** — `keys` and `profiles`, the L3 source of truth for the
  `UserTable` tiered cache.
- **Rate limit counters** — enforcement state owned by the enforcement layer, not
  the config pipeline. Included here for completeness; a Redis cluster is equally
  valid and often preferred for the hot counter path.

All tables use `timestamptz` for timestamps, `text` for IDs (the
`workspace/user/name` path is the natural key), and `bytea` for opaque blobs.
No surrogate integer PKs on user-facing tables — the path string is stable,
externally meaningful, and already interned in Go.

### Extensions and shared setup

```sql
-- Shared enum for on_exceed policy; mirrors the proto OnExceed enum.
create type on_exceed_action as enum ('reject', 'throttle', 'log_only');
```

No extensions are required by the tables in this section. A future compliance
requirement will add audit/event tables; those will use UUID v7 for their
primary keys so that compliance range queries (time-bounded `WHERE id BETWEEN
...`) can use the PK index directly without a separate timestamp column index.
Postgres 17+ exposes `uuidv7()` as a built-in; earlier versions need
`CREATE EXTENSION IF NOT EXISTS "pg_uuidv7"`. Do not reach for `pgcrypto` /
`gen_random_uuid()` — UUID v4 is random and fragments append-heavy indexes.

### 18.1 Admin snapshot store

The control plane compiles a `ConfigSnapshot`, serialises it as a
`SnapshotEnvelope` (proto + zstd), and writes one row per successful compile.
Proxies poll or stream from this table. Only the row with the highest `version`
that has `compiled_ok = true` is ever served.

The payload stored here is the **already-compiled, validated envelope**. A proxy
that fetches it calls `decodeRawConfig` → `compile()` locally (for pointer
resolution into its own process), but it will never receive a payload that failed
validation at the control plane.

```sql
create table config_snapshots (
    version         bigint          not null,
    format          text            not null default 'proto',   -- proto | yaml | json
    compression     text            not null default 'zstd',    -- zstd | none
    payload         bytea           not null,  -- compressed ConfigPayload proto bytes
    checksum        bytea           not null,  -- SHA-256 of decompressed payload
    compiled_ok     boolean         not null default true,
    compile_error   text,                      -- non-null only on failed compile (not served)
    byte_size       int             not null,  -- len(payload); for monitoring
    created_at      timestamptz     not null default now(),
    created_by      text            not null,  -- service account or operator identity

    constraint config_snapshots_pk primary key (version),
    constraint config_snapshots_version_positive check (version > 0),
    constraint config_snapshots_format_known
        check (format in ('proto', 'yaml', 'json')),
    constraint config_snapshots_compression_known
        check (compression in ('zstd', 'none')),
    constraint config_snapshots_compiled_ok_consistent
        check (compiled_ok = true or compile_error is not null)
);

-- Only one index needed: serving query fetches max(version) where compiled_ok.
create index config_snapshots_serving_idx
    on config_snapshots (version desc)
    where compiled_ok = true;

-- Retain the last N snapshots for rollback; older rows can be archived.
-- Enforced by a periodic job, not a trigger, to avoid write amplification.
comment on table config_snapshots is
    'Versioned, pre-compiled SnapshotEnvelope blobs. '
    'Only rows with compiled_ok = true are served to proxies. '
    'Retain at least 10 rows for rollback; archive the rest.';
```

### 18.2 User record tables

These are the control-plane source of truth for user records. The data plane
never reads from these tables; it receives user records embedded in the compiled
snapshot payload. The control plane writes here when records are created or
updated, then compiles and publishes a new snapshot that carries the changes.

#### `keys`

```sql
create table keys (
    -- Path identity: workspace/user/name segments stored separately for
    -- efficient scope queries, and together as the natural PK.
    id              text        not null,   -- "workspace/user/name"
    workspace       text        not null,
    usr             text        not null,   -- "user" is reserved in some SQL dialects
    name            text        not null,

    -- Routing shape. NULL means "use default routing from GlobalConfig.Models".
    -- Stored as a stable canonical JSON string so the shape key is reproducible
    -- without round-tripping through Go. The Go layer re-interns this into the
    -- current snapshot's RoutingPool on resolution.
    routing_shape   jsonb,

    created_at      timestamptz not null default now(),
    updated_at      timestamptz not null default now(),
    deleted_at      timestamptz,            -- soft delete; NULL = active

    constraint keys_pk primary key (id),
    constraint keys_id_format check (
        id = workspace || '/' || usr || '/' || name
    ),
    constraint keys_no_empty_segments check (
        workspace <> '' and usr <> '' and name <> ''
    )
);

create index keys_workspace_idx     on keys (workspace)       where deleted_at is null;
create index keys_workspace_usr_idx on keys (workspace, usr)  where deleted_at is null;
create index keys_updated_at_idx    on keys (updated_at desc) where deleted_at is null;

comment on column keys.routing_shape is
    'Canonical JSON of RawRoutingNode. NULL = default routing. '
    'Used as RoutingShapeKey in Go; re-resolved against the live snapshot on each cache miss.';
```

#### `profiles`

```sql
create table profiles (
    id              text        not null,
    workspace       text        not null,
    usr             text        not null,
    name            text        not null,

    -- Tool filter and auth shapes stored as stable canonical JSON.
    -- NULL means no overrides for that dimension.
    tool_filter_shape   jsonb,
    auth_shape          jsonb,

    created_at      timestamptz not null default now(),
    updated_at      timestamptz not null default now(),
    deleted_at      timestamptz,

    constraint profiles_pk primary key (id),
    constraint profiles_id_format check (
        id = workspace || '/' || usr || '/' || name
    ),
    constraint profiles_no_empty_segments check (
        workspace <> '' and usr <> '' and name <> ''
    )
);

create index profiles_workspace_idx     on profiles (workspace)       where deleted_at is null;
create index profiles_workspace_usr_idx on profiles (workspace, usr)  where deleted_at is null;
create index profiles_updated_at_idx    on profiles (updated_at desc) where deleted_at is null;
```

#### Trigger: keep `updated_at` current

```sql
create or replace function touch_updated_at()
returns trigger language plpgsql as $$
begin
    new.updated_at = now();
    return new;
end;
$$;

create trigger keys_updated_at
    before update on keys
    for each row execute function touch_updated_at();

create trigger profiles_updated_at
    before update on profiles
    for each row execute function touch_updated_at();
```

### 18.3 Rate limit authoring tables

These are the control-plane source of truth for `rate_limit.rules` and
`rate_limit.policies`. The data plane never reads from these tables directly; the
control plane reads them at compile time, expands tier references into policy
entries, embeds the result in the `SnapshotEnvelope` proto, and writes the
compiled blob to `config_snapshots`. The snapshot payload contains already-
expanded policy entries — tiers are a YAML/admin-API authoring convenience.

#### `rate_limit_tiers`

```sql
-- Named tier primitives (rate_limit.rules in YAML).
-- Each row is one named tier; all limit fields default to 0 (unconstrained).
create table rate_limit_tiers (
    name        text        not null,   -- unique tier name, e.g. "standard", "premium"

    -- Spend limits (stored as exact numeric to avoid float rounding).
    usd_per_minute  numeric(18,8)   not null default 0,
    usd_per_hour    numeric(18,8)   not null default 0,
    usd_per_day     numeric(18,8)   not null default 0,

    -- Request rate limits.
    rpm     int     not null default 0,
    rph     int     not null default 0,
    rpd     int     not null default 0,

    -- Token throughput limits.
    input_tokens_per_minute     int     not null default 0,
    input_tokens_per_hour       int     not null default 0,
    input_tokens_per_day        int     not null default 0,
    output_tokens_per_minute    int     not null default 0,
    output_tokens_per_hour      int     not null default 0,
    output_tokens_per_day       int     not null default 0,
    cache_read_tokens_per_hour  int     not null default 0,
    cache_read_tokens_per_day   int     not null default 0,
    cache_write_tokens_per_hour int     not null default 0,
    cache_write_tokens_per_day  int     not null default 0,

    on_exceed   on_exceed_action    not null default 'reject',

    created_at  timestamptz not null default now(),
    updated_at  timestamptz not null default now(),
    deleted_at  timestamptz,            -- soft delete; NULL = active

    constraint rate_limit_tiers_pk primary key (name),
    constraint rate_limit_tiers_name_nonempty check (name <> '')
);

create index rate_limit_tiers_updated_at_idx
    on rate_limit_tiers (updated_at desc)
    where deleted_at is null;

create trigger rate_limit_tiers_updated_at
    before update on rate_limit_tiers
    for each row execute function touch_updated_at();

comment on table rate_limit_tiers is
    'Named rate-limit tier primitives (rate_limit.rules in YAML). '
    'Referenced by rate_limit_policies.tier_name. '
    'Control plane expands references before compiling the snapshot; '
    'the snapshot proto carries only expanded policy entries.';
```

#### `rate_limit_policies`

```sql
-- Per-scope policy assignments (rate_limit.policies in YAML).
-- scope follows the workspace/user/name path convention with 1–3 segments.
-- Inline limit fields (non-zero) override the named tier; either tier_name
-- or at least one inline limit field must be set — enforced by application.
create table rate_limit_policies (
    id          bigint      generated always as identity primary key,
    scope       text        not null,   -- e.g. "demo", "demo/adi", "demo/adi/sk-vip"
    models      text[]      not null,   -- client-facing model IDs; '{*}' = catch-all
    tier_name   text        references rate_limit_tiers (name),  -- may be null (inline-only entry)
    sort_order  int         not null default 0,  -- order within a scope's entry list

    -- Inline overrides (0 = inherit from tier or unconstrained).
    usd_per_minute  numeric(18,8)   not null default 0,
    usd_per_hour    numeric(18,8)   not null default 0,
    usd_per_day     numeric(18,8)   not null default 0,
    rpm     int     not null default 0,
    rph     int     not null default 0,
    rpd     int     not null default 0,
    input_tokens_per_minute     int not null default 0,
    input_tokens_per_hour       int not null default 0,
    input_tokens_per_day        int not null default 0,
    output_tokens_per_minute    int not null default 0,
    output_tokens_per_hour      int not null default 0,
    output_tokens_per_day       int not null default 0,
    cache_read_tokens_per_hour  int not null default 0,
    cache_read_tokens_per_day   int not null default 0,
    cache_write_tokens_per_hour int not null default 0,
    cache_write_tokens_per_day  int not null default 0,

    on_exceed   on_exceed_action,       -- NULL = inherit from tier or use default

    created_at  timestamptz not null default now(),
    updated_at  timestamptz not null default now(),
    deleted_at  timestamptz,

    constraint rate_limit_policies_scope_nonempty check (scope <> ''),
    constraint rate_limit_policies_models_nonempty check (array_length(models, 1) > 0)
);

-- Compile query: fetch all active entries ordered per scope.
create index rate_limit_policies_scope_idx
    on rate_limit_policies (scope, sort_order)
    where deleted_at is null;

create index rate_limit_policies_tier_idx
    on rate_limit_policies (tier_name)
    where tier_name is not null and deleted_at is null;

create trigger rate_limit_policies_updated_at
    before update on rate_limit_policies
    for each row execute function touch_updated_at();

comment on table rate_limit_policies is
    'Per-scope rate-limit policy assignments (rate_limit.policies in YAML). '
    'Tier references are expanded by the control plane at compile time. '
    'The snapshot proto stores only expanded entries; this table is the '
    'authoring source of truth.';
```

### 18.4 Rate limit counter tables

These tables are owned by the enforcement layer. They are not read by the config
pipeline; they are listed here so the full persistence picture is in one place.
A Redis cluster with sliding-window counters is equally valid and often faster
for the hot path.

```sql
-- One row per (scope, model, window_start) combination.
-- Scope is the rate_limits key: "workspace", "workspace/user", or
-- "workspace/user/keyname". window_start is truncated to the window size
-- (minute, hour, day) by the enforcement layer before upsert.
create table rate_limit_counters (
    scope           text        not null,
    model_id        text        not null,   -- client-facing model ID, or "*"
    window          text        not null,   -- 'minute' | 'hour' | 'day'
    window_start    timestamptz not null,

    request_count   bigint      not null default 0,
    input_tokens    bigint      not null default 0,
    output_tokens   bigint      not null default 0,
    cache_read_tokens  bigint   not null default 0,
    cache_write_tokens bigint   not null default 0,
    usd_cost        numeric(18,8) not null default 0,

    updated_at      timestamptz not null default now(),

    constraint rate_limit_counters_pk
        primary key (scope, model_id, window, window_start),
    constraint rate_limit_counters_window_known
        check (window in ('minute', 'hour', 'day'))
);

-- Partial index for active windows; enforcement queries always filter by
-- window_start >= now() - interval '1 day'.
create index rate_limit_counters_active_idx
    on rate_limit_counters (scope, model_id, window, window_start desc);

comment on table rate_limit_counters is
    'Enforcement-layer counters. Not read by the config pipeline. '
    'Rows older than 25 hours can be archived or deleted.';
```

---

## 20. Snapshot Serving Query

The serving query has three jobs:

1. Return the single current envelope (highest `version` with `compiled_ok = true`).
2. Return nothing if the caller already has the current version (version-check
   parameter), avoiding unnecessary payload transfer.
3. Return enough metadata for the caller to verify the checksum and skip
   decompression if the payload format is not what it needs.

Because the payload is pre-compiled and pre-validated, the proxy's only failure
modes on fetch are network errors and checksum mismatches — not config errors.

### Primary fetch — latest compiled snapshot

```sql
-- Returns at most one row: the current live snapshot.
-- Pass :since_version = 0 on first fetch (always returns a row if one exists).
-- Pass :since_version = <last accepted version> for polling; returns no row
-- if the caller is already up to date.
select
    version,
    format,
    compression,
    payload,
    checksum,
    byte_size,
    created_at,
    created_by
from config_snapshots
where compiled_ok = true
  and version > :since_version         -- 0 on first fetch, last version on poll
order by version desc
limit 1;
```

The `config_snapshots_serving_idx` partial index on `(version desc) where
compiled_ok = true` makes this a single index scan with a `limit 1` stop.
At any realistic snapshot history size (tens to low hundreds of rows) this is
effectively O(1).

### Metadata-only check (poll without payload transfer)

Useful when the proxy wants to know whether a new snapshot exists before pulling
the full payload — for example, to decide whether to open a larger read
connection.

```sql
select version, byte_size, created_at
from config_snapshots
where compiled_ok = true
order by version desc
limit 1;
```

### Rollback — fetch a specific prior version

```sql
-- Used by operators to roll a proxy back to a known-good version.
-- The proxy calls ApplySnapshotEnvelope with the returned payload.
select
    version,
    format,
    compression,
    payload,
    checksum,
    byte_size,
    created_at,
    created_by
from config_snapshots
where version     = :target_version
  and compiled_ok = true;
```

### Recent history (admin / observability)

```sql
-- List the last :limit snapshots with size and authorship.
-- Does not return payload bytes.
select
    version,
    format,
    compression,
    byte_size,
    compiled_ok,
    compile_error,
    created_at,
    created_by
from config_snapshots
order by version desc
limit :limit;    -- typically 10–20
```

### Write path — insert a new compiled snapshot

Called by the control plane after a successful `compile()`. The version is the
output of the same monotonic counter used by `AppState.generation`.

```sql
insert into config_snapshots (
    version,
    format,
    compression,
    payload,
    checksum,
    compiled_ok,
    compile_error,
    byte_size,
    created_by
) values (
    :version,
    :format,
    :compression,
    :payload,
    :checksum,
    :compiled_ok,
    :compile_error,    -- NULL on success
    :byte_size,
    :created_by
)
on conflict (version) do nothing;   -- idempotent; control plane may retry
```

The `on conflict do nothing` makes the insert idempotent. If the control plane
retries after a transient error, a duplicate version write is silently ignored
rather than raising an error or overwriting an existing row.

### Failed compile audit record

Failed compiles are never served but are written for observability. The
`compiled_ok = false` constraint ensures they are excluded from all serving
queries without any application-level filtering.

```sql
insert into config_snapshots (
    version,
    format,
    compression,
    payload,
    checksum,
    compiled_ok,
    compile_error,
    byte_size,
    created_by
) values (
    :version,
    :format,
    :compression,
    :payload,          -- raw bytes that failed; useful for debugging
    :checksum,
    false,
    :compile_error,    -- the compile() error string
    :byte_size,
    :created_by
);
```

### Retention cleanup

```sql
-- Delete snapshots older than the last :keep_count compiled versions.
-- Run as a periodic job (e.g. daily). Never deletes failed-compile rows
-- within the window; they are small and useful for audit.
delete from config_snapshots
where version < (
    select min(version)
    from (
        select version
        from config_snapshots
        where compiled_ok = true
        order by version desc
        limit :keep_count   -- typically 10
    ) recent
);
```

### Go integration

The Go layer wraps these queries in a `SnapshotStore` interface so the rest of
the pipeline never imports `database/sql` directly:

```go
type SnapshotStore interface {
    // FetchLatest returns the current compiled envelope.
    // Returns (nil, nil) if sinceVersion is already the latest.
    FetchLatest(ctx context.Context, sinceVersion uint64) (*SnapshotEnvelope, error)

    // FetchVersion returns a specific version for rollback.
    // Returns an error if the version does not exist or compiled_ok = false.
    FetchVersion(ctx context.Context, version uint64) (*SnapshotEnvelope, error)

    // Store writes a new envelope. compiled_ok and compile_error are derived
    // from whether err is nil.
    Store(ctx context.Context, env *SnapshotEnvelope, compiledBy string, compileErr error) error
}
```

`AppState` calls `SnapshotStore.FetchLatest` on its poll ticker, compares the
returned version against its last accepted version, and calls
`ApplySnapshotEnvelope` only when a newer row is returned. The decompression,
checksum verification, proto unmarshal, `protoToRaw`, and `compile` steps all
happen inside `ApplySnapshotEnvelope` as before — the store layer only moves
bytes.

---

## Reference Map

```text
ConfigSnapshot
  -> GlobalConfig
       -> ProviderRecord                    (leaf)
       -> ModelRecord -> ProviderRecord
                      -> ModelPricing       (leaf)
       -> ServerRecord                      (leaf)
       -> RateLimits  -> []RateLimitRule    (leaf: no outbound pointers)
  -> Pools
       -> RoutingPool    -> RoutingConfig -> ProviderRecord
       -> ToolFilterPool -> []ToolFilter
       -> AuthPool       -> []AuthOverride

KeyRecord / ProfileRecord
  -> stable intern IDs (uint32) and shape keys (string)
  -> seeded into UserTable.L2 from ConfigSnapshot on every apply
  -> resolved on demand against the current ConfigSnapshot

InternPool
  -> shared, append-only string table          (leaf)

Postgres (control-plane only — not accessed by the data plane)
  config_snapshots          pre-compiled SnapshotEnvelope blobs
  rate_limit_counters       enforcement-layer counters (not read by config pipeline)

SnapshotStore (Go interface)
  -> config_snapshots       FetchLatest / FetchVersion / Store
  -> AppState               drives ApplySnapshotEnvelope on poll
```

`ProviderRecord`, `ServerRecord`, `ModelPricing`, `RateLimitRule`, and
`InternPool` are the true leaves. Every value served on the hot path is reached
through one atomic load of an immutable snapshot pointer. The database is never
on the hot path for admin config: it is read once per poll cycle, and only when
the version has advanced. User records travel with the snapshot and are never
fetched from the database on the data plane.

---

## Package Layout

All implementation lives flat inside `examples/orange/internal/config/`.
The only subdirectory is `proto/` for generated protobuf code.

```text
examples/orange/internal/config/
  config.go                  existing — YAML load, schema validation, secret resolution
  config.schema.json         existing — embedded JSON Schema
  config_test_exports.go     existing — test-only exported symbols

  config_intern.go           InternPool (§9)
  config_raw.go              raw serde structs (§6)
  config_domain.go           domain structs: ProviderRecord, ModelRecord, … (§10, §12)
  config_compile.go          compile(), validateUSDDependency, validateScopeKey (§13)
  config_envelope.go         SnapshotEnvelope, SnapshotFormat, CompressionKind (§4)
  config_decode.go           decodeRawConfig, protoToRaw, decompress, verifyChecksum (§4a)
  config_appstate.go         AppState, hot-reload, LoadConfig (§14)
  config_usertable.go        UserTable[M,R], ResolvedEntry, KeyRecord, ProfileRecord (§11, §15)
  config_ratelimit.go        ResolveRateLimitRules, filterRulesByModel (§16)
  config_store.go            SnapshotStore interface (§19)
  config_store_postgres.go   pgx implementation of SnapshotStore (§19)

  proto/
    snapshot.proto           wire schema (§4a)
    snapshot.pb.go           generated by protoc / buf
```
