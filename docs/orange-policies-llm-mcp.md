# Applying LLM + MCP Policies to `examples/orange`

**Status:** Research / suggestion. Scope: how to map the chapter 15/16/17
policy model onto the static `examples/orange/orange.yaml` config, with a
sharp focus on **fallback policies** and **traffic splitting** for the
LLM side, plus the equivalent shape for MCP.

This is a local materialization story. There is no Config API Server
in front of orange, but the chapter-15 entity model still holds: a
**Pear Key is owned by `(User, Workspace)`, not by User alone**. A key
cannot cross workspaces. Maya with access to two workspaces has two
keys, with two materialized blobs. The materialization unit is
therefore `(workspace, user, key)`, and an opaque key id like
`ws-prod/user-maya/key-a` carries those three identifiers — orange
can decode them for attribution without consulting any other table.

The YAML is a flat dictionary of those per-key blobs, each carrying
its own LLM routing tree (primary + fallback chain + splits). MCP
profiles are a sibling concept selected by URL path (§ 3); they too
belong to one workspace and are picked from the workspace's profile
set.

**`examples/orange/orange.yaml` is a snapshot at time `t`.** It plays
the role the Config API Server's materialized output plays in
chapter 15 — the full set of per-key blobs that the data plane needs
to serve traffic right now. There is no live stream, no rebuild
fanout, no revocation channel: editing the file and restarting (or
hot-reloading) is the only update mechanism. Every property below is
phrased in those terms — "at time `t`, key `K` resolves model `M` to
Target `T`" — and changes between snapshots are simply a new file.

This matches chapter 15's "materialize per Pear Key" model exactly.
**Inheritance is a control-plane concern, not a data-plane one.** The
control plane intersects Project ∩ Workspace ∩ Key, picks Defaults,
unions Merges, and ships one fully self-contained blob per key. Orange
— the data plane — receives those blobs and serves traffic. It does
not think, cascade, or reduce. A single lookup by key id returns
everything the request needs.

orange's static YAML stands in for the control plane's output: the
`keys[]` table is the set of already-materialized blobs at time `t`.

The question is what shape inside the YAML lets each key express
fallback chains and traffic splits without inventing semantics that
diverge from chapters 16/17.

---

## Where orange is today

`examples/orange/orange.yaml`:

- `llm.models[model] = {provider, name?, endpoints?}` — 1:1 model →
  provider mapping, **globally shared across all callers**. No
  fallback, no split, no per-key override.
- `llm.providers[name] = {kind, endpoint, auth, …}` — provider binding +
  credentials. Equivalent to chapter 16's `provider_binding` + `byok`,
  but flattened and global.
- `mcp.profiles[name] = {tools{server: {...}}, auth{server: {...}}}` —
  Profile spec, also global. Anyone who hits orange gets the same
  profile.
- `mcp.servers[name] = {endpoint, namespace, auth, tools}` — cataloged
  server registry.

Today `match.LookupModel(model, endpoint) -> (upstream, backendModel)`
returns one upstream. `pick.lookupHost` round-robins across DNS-resolved
IPs for that single upstream. **No caller identity, no policy-driven
multi-target selection, no per-key configuration.**

For the chapter-15-style "the caller's key picks the materialized blob"
model, we need to grow the schema along two axes at once: (a)
per-key blobs, and (b) routing trees inside each blob. The two are
independent — either could land first — but the worked examples below
assume both.

---

## Proposal at a glance

Shared frame:

1. **`keys[]`** — top-level table of materialized per-key blobs,
   keyed by an opaque key id that encodes `(workspace, user, key)`
   (e.g. `ws-prod/user-maya/key-a`, `ws-prod/user-maya/key-b`,
   `ws-dev/user-maya/key-a`). Each entry carries its own model table.
   The shared `llm.providers` / `mcp.servers` blocks remain catalog
   reference data only — never policy.

LLM additions (Part I, § 1-2):

2. **`keys[].llm.models[].routing`** — per-key routing tree
   (Chain | Split | Target). Existing single-`provider` shape stays as
   sugar for a single-Target leaf.
3. **`llm.providers[].bindings[]`** — optional named bindings per
   provider so a Target can pick East vs West, prod vs sandbox.
   Providers stay in the shared catalog block — credentials and
   endpoints are platform-owned, not per-key authored. (Per-key BYOK
   credential overrides come later; see § 6.)

MCP additions (Part II, § 3):

4. **`mcp.profiles[].servers[].fallback[]`** and
   **`mcp.profiles[].fanout_failure`** — per chapter 17, on the
   profile keyed by URL path.

The schema is the minimum surface that the data plane needs. The
control-plane-side ergonomics (presets, Caps validation, AUTO
expansion) are out of scope for orange — orange consumes already-
materialized data.

### Key resolution at request time

**Orange does no inheritance.** Inheritance lives in the control plane
(chapter 15 § "Resolution pipeline"). The control plane intersects
Project ∩ Workspace ∩ Key Caps, picks the nearest Default, unions
Merges, materializes the result, and ships a self-contained blob per
key. The data plane reads that blob and serves the request. No
cascade, no fall-through to a shared block, no per-axis reduce at
request time. Orange consumes the blob; it does not compute it.

In orange, `keys[id]` *is* that materialized blob. Today it is
hand-authored in the static file; that is a stopgap. A config provider
inside orange will subscribe to the control plane and receive the same
per-key materialized blobs over the wire — no manual materialization,
no YAML editing on rotation. The runtime contract on the consuming
side is identical either way:

```text
key_id = parse(Authorization)
blob   = keys[key_id]                  # one lookup, no cascade
target = resolve(blob.llm.models[model])
```

If the key id is unknown → reject with `orange.unknown_key` (401). If
the model is not in `blob.llm.models` → reject with
`orange.model_not_found` (404). There is no "fall through to a shared
model table" — that would be inheritance, and inheritance happened
upstream when the blob was minted.

The shared `llm.providers` / `mcp.servers` blocks in the YAML are
**catalog reference data**, not policy. They contain endpoints,
binding URLs, and DNS targets — platform-owned plumbing that every
materialized blob points at by name. The control plane pinned a
specific catalog version into each key's blob; orange just looks the
references up. This matches chapter 15 § "What the Shard PEP does
*not* hold": no catalog composition at request time. Whether the YAML
factors the catalog out of each blob for authoring convenience or
inlines it is a syntactic choice; the data plane's behaviour is the
same.

MCP follows a separate resolution path (URL-keyed, not Authorization-
keyed); see Part II.

---

# Part I — LLM

## 1. LLM routing tree (per-key)

### Schema

```yaml
keys:
  # Maya's prod key inside ws-prod.
  ws-prod/user-maya/key-a:
    workspace: ws-prod
    user: user-maya
    llm:
      models:
        sonnet:
          # Existing simple form still works (degenerate single-Target tree):
          provider: anthropic           # backwards-compatible
          name: claude-haiku-4-5-20251001

        smart:
          # New form: explicit routing tree. `provider`/`name` ignored
          # when `routing` is present.
          routing:
            chain:
              retry_on: [429, 5xx, timeout]
              children:
                - target:
                    provider: anthropic
                    binding: prod        # optional; defaults to provider's default binding
                    name: claude-haiku-4-5-20251001
                - target:
                    provider: openai
                    name: gpt-4o-mini

  # Maya's second key in the same workspace.
  ws-prod/user-maya/key-b:
    workspace: ws-prod
    user: user-maya
    llm:
      models:
        cheap-balanced:
          routing:
            split:
              children:
                - weight: 80
                  target: { provider: openai, name: gpt-4o-mini }
                - weight: 20
                  target: { provider: gemini, name: gemini-2.5-flash }

  # Maya again, but in ws-dev — this is a *different* Pear Key with a
  # *different* materialized blob. Keys do not cross workspaces.
  ws-dev/user-maya/key-a:
    workspace: ws-dev
    user: user-maya
    llm:
      models:
        high-availability:
          routing:
            chain:
              retry_on: [5xx, timeout]
              children:
                - split:                 # primary: 50/50 across two regions
                    children:
                      - weight: 50
                        target: { provider: anthropic, binding: us-east }
                      - weight: 50
                        target: { provider: anthropic, binding: us-west }
                - target: { provider: openai, name: gpt-4o-mini }  # fallback
```

The explicit `workspace` / `user` fields are denormalised from the key
id — they're the attribution tuple chapter 15 § "Attribution: logs,
traces, ledgers" pins into the blob, so `match` and the access log
don't have to re-parse the opaque id. The control plane stamps both
fields when it mints the blob; orange just reads them.

Two callers can ask for the same model alias and get materially
different upstreams — `ws-prod/user-maya/key-a` asking for `smart`
rides the Anthropic-then-OpenAI chain; `ws-dev/user-maya/key-a`'s
`smart` is whatever that
key authored, or 404 if it didn't author one. There is no
cross-key inheritance.

Three node kinds, matching chapter 16:

- `target` (leaf): `{provider, binding?, name?}`. `name` defaults to the
  model alias itself.
- `chain`: `{retry_on, children[]}`. Sequential; advance to the next
  child only when the previous child fails with a `retry_on` status.
- `split`: `{children[{weight, ...}]}`. Per-request probabilistic.
  Weights are integers; must sum to 100. Choice is sampled once and
  pinned for the lifetime of the request (no mid-stream re-pick).

### Resolution at request time

In `pipeline/match`, after extracting `key_id` from the
`Authorization` header and `model` from the body, the resolver loads
the key's blob and walks that blob's tree to choose **one** Target for
the initial attempt:

1. `split` → sample a child by weight; recurse into it.
2. `chain` → recurse into `children[0]` for the first attempt. Children
   `[1..n]` are *candidates*, attached to the Decision for later use.
3. `target` → done.

The Decision struct grows from a single `ProviderBackend` to:

```go
type Decision struct {
    Primary    Target         // first attempt
    Fallbacks  []Target       // ordered remainder of the active Chain
    RetryOn    []int          // status codes that trigger advance
    // ... existing fields
}

type Target struct {
    ProviderBackend string
    Binding         string  // "" = provider's default binding
    BackendModel    string
    ProviderKind    string
}
```

### Fallback enforcement (the hard part)

Fallback inside Envoy's per-request lifecycle without buffering the
response body has two practical shapes:

| Shape | What it can do | What it costs |
|---|---|---|
| **A. Retry-policy via route** | Use Envoy's HTTP route `retry_policy` with `retry_on: 5xx,gateway-error,reset` and per-try host predicates that exclude already-tried hosts. Drop the request on its second attempt into a different cluster via dynamic cluster selection. | Streaming responses (SSE, `/v1/responses` WS) cannot be retried after bytes start flowing. The fallback only helps on early errors (connect, 5xx-with-no-body, request-timeout). |
| **B. Pre-egress probe + dial** | Inside `pick.ChooseHost`, pre-check the primary target's host health (`HostHealth`, recent error rate from `meter`) and short-circuit to `Fallbacks[0]` before dialing. | No live fallback once upstream accepted the request. Same coverage limitation as A. |

**Recommendation: do A, but keep all target selection local to the
dynamic module — do not lean on xDS-supplied fallback clusters.**

The literal "drop the second attempt into a different cluster via
dynamic cluster selection" shape requires that every fallback cluster
is already known to Envoy via xDS. Two reasons we don't want that:

1. **Round-trip defeats the point.** The materialized blob already
   lives in the dynamic module. Asking the control plane "is there a
   cluster for `openai-prod`?" at retry time defeats the point of
   having pre-materialized per-key blobs.
2. **Envoy Gateway is a hard dep in real deployments.** Pushing
   infra-like config that affects xDS means authoring/applying CRDs
   (HTTPRoute, Backend, BackendTrafficPolicy, EnvoyPatchPolicy, …) and
   waiting for the Gateway controller to reconcile. That is the wrong
   shape for per-key blob churn — blobs change at user/policy cadence,
   not at infra-CRD cadence.

The workaround we use (see
`https://gist.github.com/dio/965d1e555909c02013ca882a2b3caa78`) is to
register fallback hosts at runtime against the same logical
`orange-pick` cluster from inside the dynamic module, with
`auto_host_sni` and a bounded SNI-scoped TLS session cache so the
runtime-added hosts behave correctly for TLS/SNI without xDS supplying
their hostnames. With that in place, fallback is entirely a
`ChooseHost` decision — no cluster swap, no xDS consult.

Wire it as follows:

- `match` writes the Decision (primary + fallbacks) to filter state.
- The route is a single logical cluster `orange-pick`. Set
  `retry_policy` on the route with `num_retries = len(Fallbacks)`
  (clamped) and `retry_on` derived from `RetryOn`.
- On blob load, the module calls `AddHosts` on `orange-pick` for every
  distinct `(ProviderBackend, Binding)` referenced by any Target in the
  blob, tagged so `ChooseHost` can find them. Hosts carry their own
  hostname/SNI (per the gist) so TLS works without xDS.
- `pick.ChooseHost` reads attempt count from `ClusterLBContext`
  (`envoy.lb.previous_hosts` / `attempt_count`) and on attempt N
  selects the registered host matching `Fallbacks[N-1]` instead of
  `Primary`. All decisions are local to the module.
- Per-target credential injection still happens in the upstream HTTP
  filter; switching target switches credentials because `pick` rewrote
  `:authority` and updated filter-state `upstream` for the retry.

Edge cases worth pinning:

- **Streaming.** Once HCM has flushed the response status to the
  client, retry is disabled. Document this explicitly; chapter 16's
  WebSocket path acknowledges the same limitation. Splits work for
  streaming (sampling is pre-dial); chains don't, post-dial.
- **`429` from a provider's free tier vs a real rate-limit** — both
  look the same. Default `retry_on` should include `429` only when the
  user opted in. Otherwise a retry storms a healthy upstream that
  returned a legitimate back-pressure signal.
- **Idempotency.** Chat completions and messages are POSTs; treat them
  as safely retriable only when nothing has streamed. Envoy's
  `retry_on: reset,connect-failure,refused-stream` is safe; `5xx` is
  safe pre-stream; `429` and `gateway-error` are user-opt-in.

### Traffic splits

Splits are easier than chains because they are pre-dial. Implementation:

- In `match.bodyHandler`, after resolving `model` to a tree, sample the
  Split nodes top-down with `crypto/rand` (or `math/rand` seeded per
  request from a header for sticky testing). Produce a single Target.
- No retry-policy changes needed unless the split is wrapped by a
  Chain.
- For canary releases (`weight: 95/5`) this is sufficient — neither
  arm needs cross-request coordination.

Per-route sticky split (e.g., "this user always lands on Anthropic") is
out of scope; that's a chapter-15 concern, not a data-plane primitive.

---

## 2. Provider bindings

Today: `llm.providers[name]` is one provider with one endpoint.

Proposal:

```yaml
llm:
  providers:
    anthropic:
      kind: anthropic
      auth:
        type: anthropic
        secret_ref: env://ANTHROPIC_API_KEY
      bindings:
        # The "default" binding is what existing config uses implicitly.
        - name: default
          endpoint: https://api.anthropic.com
          extra:
            anthropic_version: "2023-06-01"
        - name: us-east
          endpoint: https://api.anthropic.com
          # Could also override auth.secret_ref here for a per-region key.
        - name: us-west
          endpoint: https://api-west.anthropic.com
```

When `Target.binding` is empty, the default binding is used. When the
provider declares no `bindings`, the flat `endpoint`/`auth`/`extra`
fields keep working — that's the backwards-compatible path. Today's
config gets one synthetic default binding at load time.

The downstream effect: `pick` no longer keys its resolved-host map by
`provider name` alone — it keys by `(provider, binding)`. Schema-wise
this is a one-line widening of `resolvedUpstream`'s map key.

`credinject` already reads filter-state to pick the right
`Authorization` shape; it now reads `(upstream, binding)` to choose the
right `auth.secret_ref` and `extra.anthropic_version`.

---

# Part II — MCP

MCP resolution diverges from LLM at the first hop. Where LLM keys off
`Authorization`, MCP keys off the **URL path**:
`/mcp/<opaque-profile-id>` deterministically identifies one profile,
and that profile carries its owner `(workspace, user)` as a property
(`owner = f(mcp-path)`). The caller's key, if any, only feeds the
AccessGate — it does not pick the profile.

## 3. MCP fallback and split (per-profile, owned by users)

MCP's dispatch is mechanical — no user-authored tree per chapter 17.
Three operationally important hooks belong in the schema, plus an
ownership model that differs materially from the LLM side.

### 3a. Profile ownership

**A profile is owned by `(User, Workspace)` — same shape as a Pear
Key.** Chapter 17 fixes "a Profile is owned by exactly one Workspace",
and orange records the user who created it on the same entry. So one
user can own many profiles, but every profile lives in exactly one
workspace and cannot be reached from another. The profile id is
selected by URL path, and the owner tuple is a pure function of the
path:

```text
/mcp/<opaque-profile-id>  →  profiles[<opaque-id>]
                          →  (workspace = ws-prod, owner = user-maya)
```

This invariant holds across every AccessGate variant (`PearKeyGate`,
`BuiltinOAuth`, `CustomOAuthGate`, `OpenGate`) — including the no-key
case. **OpenGate is the load-bearing example: there is no caller
identity at all, but `owner` is still unambiguous from the path
alone.** Without that property, OpenGate traffic would be unattributed
and the platform couldn't bill the user whose profile is doing the
work.

The gate decides *whether* the caller may use the profile and *what
subject* the request attributes to for vault lookups (chapter 17
§ "Subject resolution by AccessGate"). It does not decide *who owns*
the profile. Owner and subject are orthogonal.

Schema:

```yaml
mcp:
  profiles:
    d8f099575461318c-34ebaa6e65d4804e:
      workspace: ws-prod            # profile cannot cross workspaces
      owner: user-maya              # owning user inside that workspace
      display_name: dev-tools
      access_gate:
        kind: PearKeyGate
        allowed_subjects: [user-maya]   # who may call it
      fanout_failure:
        initialize: any_succeeds
        tools_list: partial
      tools:
        github: { include: ["search_repositories"], optional: true }
        kiwi:   { include: ["search-flight"] }

    a14c0e1d4ab2f7e9-c0ff3e8b2d5e9a01:
      workspace: ws-prod            # same workspace, same user, different profile
      owner: user-maya
      display_name: flight-only
      access_gate:
        kind: OpenGate
        abuse_controls: { rate_per_ip: 60/m }
      tools:
        kiwi: { include: ["search-flight"] }

    7c9f12a3b8e0d1f4-559e0a6c4d2b1f8e:
      workspace: ws-travelapp       # different workspace entirely
      owner: user-travelapp
      display_name: travelapp-public
      access_gate:
        kind: CustomOAuthGate
        issuer_url: https://idp.travelapp.example
        jwks_url: https://idp.travelapp.example/.well-known/jwks.json
        audience: orange-mcp
        claims_map: { subject: sub }
      tools:
        kiwi: { include: ["search-flight"] }
```

Consequences:

- **Keys do not "pick" a profile.** The `keys[id].mcp.profile` field
  from the earlier draft is dropped. A key with `PearKeyGate` access
  to profile `X` can call `/mcp/X`; the same key can also call
  `/mcp/Y` if `Y`'s gate admits it. Profile selection is the URL.
- **The owner is the attribution principal for billing/audit on this
  profile.** Even when an OpenGate caller has no subject, the
  emitted records carry `owner: user-maya` so usage rolls up to her.
- **The subject (for `vault[profile][server][subject]`) is the
  identity the gate produced**, per chapter 17 § "Subject resolution
  by AccessGate". Owner ≠ subject in the general case. For a
  `PearKeyGate` profile where `allowed_subjects = [owner]`, they
  coincide; for OpenGate, subject is `SHARED` and the owner-supplied
  credentials are the only ones the vault can serve.
- **One user, many profiles is the common case.** `user-maya` owns
  `dev-tools` and `flight-only` in `ws-prod`. If she also works in
  `ws-dev`, those would be separate profile entries with
  `workspace: ws-dev`. Listing "Maya's profiles in ws-prod" is
  `profiles.where(owner == user-maya AND workspace == ws-prod)`; no
  separate index is needed.
- **Keys cannot reach profiles in another workspace.** When a
  `PearKeyGate` profile in `ws-prod` is called by a key bound to
  `ws-dev`, the gate rejects (`workspace_mismatch`, hard deny per
  chapter 16 § "Violation Handling"). This is the chapter-15
  "Workspace boundary" property surfacing at the MCP layer.

### 3b. Resolving the owner from a request

At request time the orange MCP sidecar runs:

```text
1. parse path: /mcp/<opaque-id>            -> profile_id
2. profile = profiles[profile_id]          -> 404 if missing
3. owner   = profile.owner                 -> attribution principal
4. evaluate profile.access_gate            -> subject (or 401/403)
5. dispatch tools/* using profile.tools, applying fanout_failure
6. for tools/call name=server.tool:
     subject already known from step 4
     credential = vault[profile_id][server][subject]
     forward with envelope
```

Step 3 is the load-bearing change versus chapter 17 read literally:
orange treats `owner` as authoritative from the profile object, not
derived from the key. This matches the integration shape where a
Profile ID can encode ownership for internal routing — orange just
makes the encoding explicit (`owner:` field) instead of packing it
into the opaque id.

### 3c. `fanout_failure` per Profile

```yaml
mcp:
  profiles:
    dev-tools:
      fanout_failure:
        initialize:  any_succeeds   # or: all_succeed
        tools_list:  partial        # or: all_succeed
      tools:
        kiwi:        { include: ["search-flight"] }
        aws-knowledge:
          include:   ["read_documentation", "search_documentation"]
          optional:  true
        github:
          include:   ["search_repositories"]
          optional:  true
```

`optional: true` already exists in the file; that's exactly the
"server may fail; session continues" behavior. Lift it to a Profile-
level explicit policy with the chapter-17 vocabulary so the orange MCP
sidecar can apply uniform semantics.

### 3d. Per-member backend fallback

For one cataloged server, register a fallback host inside `mcp.servers`:

```yaml
mcp:
  servers:
    github:
      endpoint: https://api.githubcopilot.com/mcp/
      auth: { type: bearer, secret_ref: env://GITHUB_TOKEN }
      tools: { include: ["search_repositories", "get_file_contents"] }
      fallback:
        - endpoint: https://github-mcp.internal.example.com/mcp/
          auth: { type: bearer, secret_ref: env://GITHUB_INTERNAL_TOKEN }
```

This is below the policy surface per chapter 17 § "Membership and
dispatch" — fallback for one cataloged server lives on the
server/cluster, not on Profile membership. It uses Envoy's retry-policy
on the `/mcp/s/{server}` route inside the MCP sidecar.

### 3e. Profile-level split (NOT recommended for v1)

Splitting a Profile across two GitHub MCP backends in a weighted way
would diverge from chapter 17 ("dispatch is mechanical from JSON-RPC
method + membership list"). If we need it for testing, gate it behind a
`mcp.servers[name].split[]` shape with the same semantics as LLM
splits, but call it explicitly **non-canonical** in code and docs.

---

## 4. Concrete worked example

Two users, three keys, two MCP profiles, divergent behavior. Catalog
shared, then per-key blobs. § 4a covers LLM behavior; § 4b covers MCP
behavior on the same snapshot.

### 4a. Shared catalog and per-key LLM blobs

```yaml
# Shared catalog. Tenant policy never edits this in v1.
llm:
  providers:
    anthropic:
      kind: anthropic
      auth: { type: anthropic, secret_ref: env://ANTHROPIC_API_KEY }
      bindings:
        - name: us-east
          endpoint: https://api.anthropic.com
        - name: us-west
          endpoint: https://api-west.anthropic.com
    openai:
      kind: openai
      endpoint: https://api.openai.com
      auth: { type: bearer, secret_ref: env://OPENAI_API_KEY }
    gemini:
      kind: openai
      endpoint: https://generativelanguage.googleapis.com
      auth: { type: gemini, secret_ref: env://GEMINI_API_KEY }

mcp:
  servers:
    kiwi:    { endpoint: https://mcp.kiwi.com, namespace: kiwi }
    github:  { endpoint: https://api.githubcopilot.com/mcp/, namespace: github,
               auth: { type: bearer, secret_ref: env://GITHUB_TOKEN } }
  profiles:
    d8f099575461318c-34ebaa6e65d4804e:
      workspace: ws-prod
      owner: user-maya
      display_name: dev-tools
      access_gate: { kind: PearKeyGate, allowed_subjects: [user-maya] }
      fanout_failure: { initialize: any_succeeds, tools_list: partial }
      tools:
        github: { include: ["search_repositories"], optional: true }
        kiwi:   { include: ["search-flight"] }
    7c9f12a3b8e0d1f4-559e0a6c4d2b1f8e:
      workspace: ws-travelapp
      owner: user-travelapp
      display_name: travelapp-flight-only
      access_gate: { kind: OpenGate, abuse_controls: { rate_per_ip: 60/m } }
      tools:
        kiwi: { include: ["search-flight"] }

# Per-key materialized blobs. Key id encodes (workspace, user, key).
keys:
  # Maya's production key inside ws-prod: HA across Anthropic regions,
  # OpenAI cross-provider disaster fallback.
  ws-prod/user-maya/key-a:
    workspace: ws-prod
    user: user-maya
    llm:
      models:
        sonnet:
          routing:
            chain:
              retry_on: [5xx, timeout, reset]
              children:
                - split:
                    children:
                      - weight: 50
                        target: { provider: anthropic, binding: us-east, name: claude-haiku-4-5-20251001 }
                      - weight: 50
                        target: { provider: anthropic, binding: us-west, name: claude-haiku-4-5-20251001 }
                - target: { provider: openai, name: gpt-4o-mini }

  # Maya again, but in ws-dev — separate Pear Key, separate blob.
  ws-dev/user-maya/key-a:
    workspace: ws-dev
    user: user-maya
    llm:
      models:
        cheap:
          routing:
            split:
              children:
                - weight: 80
                  target: { provider: openai, name: gpt-4o-mini }
                - weight: 20
                  target: { provider: gemini, name: gemini-2.5-flash }

  # Travel app's key: single LLM target.
  ws-travelapp/user-travelapp/key-a:
    workspace: ws-travelapp
    user: user-travelapp
    llm:
      models:
        sonnet: { provider: anthropic, name: claude-haiku-4-5-20251001 }
```

LLM behavior:

- `ws-prod/user-maya/key-a` asking for `sonnet` samples one of
  `(anthropic, us-east)` or `(anthropic, us-west)` 50/50; on
  `5xx/timeout/reset` *and* before any byte of the response has
  streamed, Envoy retries on `(openai, gpt-4o-mini)`.
- `ws-dev/user-maya/key-a` asking for `sonnet` → 404
  (`orange.model_not_found`); `cheap` works. Maya's prod and dev keys
  are entirely separate blobs — same human, different
  `(User, Workspace)` tuple.
- `ws-travelapp/user-travelapp/key-a` asking for `cheap` → 404. The
  same alias means different things per key.
- Streaming: once SSE bytes start flowing, the chain fallback is
  unreachable; client sees the upstream error.

### 4b. MCP behavior on the same snapshot

Using the two profiles from the catalog above:

- `/mcp/d8f099575461318c-34ebaa6e65d4804e` is attributed to
  `(ws-prod, user-maya)` from the profile entry. The gate is
  `PearKeyGate` with `allowed_subjects: [user-maya]`; calls with
  `ws-prod/user-maya/key-a` pass. A call with
  `ws-dev/user-maya/key-a` is rejected with `workspace_mismatch`
  even though the user matches — keys do not cross workspaces.
- `/mcp/7c9f12a3b8e0d1f4-559e0a6c4d2b1f8e` carries no caller identity
  (`OpenGate`); usage rolls up to `(ws-travelapp, user-travelapp)`
  from the profile entry, and the subject for vault lookups is the
  `SHARED` sentinel.

---

## 5. Implementation steps (sketch)

1. **Config**:
   - Add top-level `keys[id]` table to `internal/config/config.go`.
     Each entry is a self-contained materialized blob with its own
     `llm.models`. MCP profiles are not referenced from `keys[]`; they
     are selected by URL path at request time.
   - Add `RoutingNode` (chain/split/target) types. Lift
     `provider`/`name` to a synthetic Target leaf when `routing` is
     absent. Update `config.schema.json`.
   - Add `bindings[]` to provider. Treat absent as one default binding.
2. **Match**:
   - Extract `key_id` from `Authorization: Bearer <key>` at headers
     phase. Reject with `orange.unknown_key` (401) if the key isn't in
     `keys[]`.
   - Replace `LookupModel(model, endpoint)` with
     `ResolveModel(keyID, model, endpoint) -> (primary Target,
     fallbacks []Target, retryOn []int)`. The resolver reads
     `keys[keyID].llm.models` only. No cascade, no inheritance — the
     blob is already materialized.
   - Sample splits inside the resolver. Determinism is per-request; no
     cross-request state.
   - Store `key_id`, fallbacks, and retryOn in filter state for `pick`
     and for the access log (attribution tuple).
3. **Pick**:
   - Key the resolved-host map by `(provider, binding)`. Provider
     bindings live in the shared catalog so the host map stays
     proxy-wide; per-key state is just the Decision in filter state.
   - Read attempt count from `ClusterLBContext` filter state and pick
     `Fallbacks[N-1]` on retry attempt N.
4. **Route / Envoy template**:
   - Set a `retry_policy` on the `/v1/chat/completions`,
     `/v1/messages`, `/v1/responses` routes with
     `num_retries: <max chain length across all keys>` and a
     `retry_on` union of all configured `RetryOn` codes. Per-request
     gating is enforced by `pick` returning a no-fallback host on
     attempt > len(Fallbacks).
5. **Credinject**:
   - Resolve `(upstream, binding)` instead of `upstream`.
6. **MCP sidecar**:
   - Parse `/mcp/<profile-id>` from the path. Look up
     `mcp.profiles[profile-id]` and resolve `owner = profile.owner`
     **before** evaluating the AccessGate — owner attribution is a
     pure function of the path and does not require a caller key.
   - Evaluate the AccessGate to derive the subject (or reject). For
     `OpenGate`, no key is needed and the subject is `SHARED`; owner
     is still authoritative for billing/audit.
   - Honor `fanout_failure` per Profile.
   - Add per-server route-level retry policy reading `servers[name].fallback`.
7. **Tests**:
   - Table-driven: same model alias resolves to different upstreams
     under different `key_id`s; chain falls back on 502; split
     distribution within 5% of weights over 10k samples; streaming
     bypasses fallback; unknown `key_id` → 401.

Use `gsed` (per local convention) for any in-place schema edits during
implementation.

### 5.1 Memory shape: don't hold the YAML at runtime

Per-key materialization scales the number of resident blobs with the
key population, not the operator's authoring effort. A naïve "parse
YAML into Go structs, keep in a `map[string]*Key`" implementation pays
heavily once `len(keys)` reaches the thousands:

- Every duplicated provider name, model alias, endpoint URL, secret
  ref is a separate Go string with its own header (16 B) and backing
  array.
- Every `map[string]any` rung in the parsed tree allocates.
- A 1-2 KB authoring blob inflates to 10-20 KB resident.

The shape we want is closer to the **confpack** sketch
(`/Users/dio/Downloads/README.md`): a string-interned, fixed-record
binary format that each instance can hold in compact form, decode
lazily (or zero-copy), and drop on snapshot replacement.

Properties to lift directly:

- **One string pool per snapshot.** Provider names (`anthropic`,
  `openai`, `gemini`), model aliases, endpoint URLs, secret refs,
  binding names all repeat across keys. A pool with `idx → string`
  collapses `N keys × M strings` to `M unique strings` plus per-key
  `uint8`/`uint16` indices.
- **Fixed-size records for hot fields.** A Target leaf is
  `(provider_idx, binding_idx, model_idx, kind_idx)` — four bytes
  amortised against a much larger Go struct.
- **Zero-copy reads in the hot path.** `match.ResolveModel` and
  `pick.lookupHost` only need indices and pointers into the snapshot
  buffer; they shouldn't allocate to read a Target.
- **Optional compression at rest.** The snapshot on disk (or in
  shard-local cache) can be zstd-compressed; decompress once on load,
  keep the raw byte slice resident.
- **Atomic snapshot swap.** New snapshot = new byte slice + new view.
  `atomic.Pointer[snapshot]` swap is the only mutation; readers never
  see a half-built table. The old snapshot is GC'd after in-flight
  readers complete.

Per-instance scoping matters too: a sharded orange fleet (§ 6.5)
means each instance holds only the slice of keys assigned to it, not
the global table. The snapshot the control plane (or the static YAML
loader) hands to instance `i` is *already* trimmed to the keys
`i` owns. With confpack-style packing, that resident slice is small
enough that a 10k-key instance fits comfortably in tens of MB rather
than hundreds.

What this *does not* mean:

- **The on-disk authoring format stays YAML.** Operators read and
  edit YAML. The compact binary is the loaded form, produced by the
  config loader (today: file watcher → re-pack on change).
- **No premature optimisation for v1.** The first cut can use plain
  Go structs; refactor to the packed form when key population grows
  past a few hundred. The schema in this doc is unchanged by the
  encoding choice — confpack-style packing is orthogonal to the
  policy surface.

The point of mentioning it now: implementers should design the
config-loading boundary (`config.Get()`, `LookupModel`, etc.) so the
struct shape is *behind* a small interface. Swapping plain structs for
a packed view later then doesn't ripple through `match` / `pick` /
`credinject`.

---

## 6. What's deliberately out of scope

- **Caps / sub-allocation / Merge-sticky guardrails.** These need a
  control plane to validate at edit time. Without one, runtime
  enforcement would be unsound.
- **`token_quota`, `budget_usd`, `cost_ceiling_per_request`.** Require
  a durable ledger. Out of scope for orange; revisit when a metering
  store lands.
- **Pear-pool egress identity / per-Workspace fairness.** No
  multi-tenant surface in orange yet.
- **MCP `AccessGate` variants.** orange's MCP sidecar today is
  `PearKeyGate`-equivalent (bearer to the proxy). OAuth and OpenGate
  are control-plane shaped; defer.
- **WebSocket (`/v1/responses`) post-`101` fallback.** Acknowledge as
  a known gap; document the same way chapter 16 does.

---

## 6.5. Forward-looking: Gateway shard hashing beyond `key_id`

Not for v1 of this proposal, but worth pinning before it's forgotten.

Chapter 15 § "Bridging to the Shard PEP" defines the Gateway's only
job as `shard_group = hash(key_id) % N`. That's the minimum viable
shape — one hash dimension, one shard table. In practice, when the
Gateway fronts an orange shard fleet, we likely want the shard
selector to mix in additional **request hints** so that the policy
materialization can be sharded along more axes than just the caller's
key:

```text
shard_group = hash(key_id, hint_1, hint_2, ...) % N
```

Concrete hints that may matter:

- **Traffic class** — `LLMProxy` vs `MCPProxy` is already a separate
  shard fleet (chapter 13), but within a fleet the model alias or MCP
  path could co-locate related blobs.
- **MCP profile path** — `/mcp/<opaque-id>` may want to land on the
  shard that already holds the Profile's vault entries and Catalog
  blob references, independent of which key authenticated.
- **Workspace / project id** when those become first-class — co-locate
  a Workspace's keys for warm caches and per-Workspace counter
  affinity (chapter 15 § "Per-Key sharding and counters").
- **Tenant / region hint from Host or SNI** — keep EU keys on EU
  shards for residency reasons without paying a cross-region hop.

The data-plane consequence is mostly clean: orange shards still serve
self-contained blobs keyed by `key_id`; the Gateway just routes more
selectively so the shard that ends up loading the blob is the "right"
one for adjacent state (counters, vaults, caches).

Two things to nail down before this lands:

1. **What's the shape of the shard table?** A single `(key_id,
   hint_tuple) → shard_group` table grows combinatorially. More
   likely: per-hint sub-tables that compose, or rendezvous hashing
   over a labeled host set so the Gateway can express "prefer EU
   shards, then any" without a full table edit.
2. **Hint authority.** Hints sourced from the request (path, headers,
   SNI) are trivially spoofable; hints sourced from the materialized
   blob aren't visible at the Gateway (the Gateway doesn't load
   blobs). The control plane has to publish a sidecar **routing
   directive** for the Gateway — something like "Key K wants traffic
   class T on shard set S" — separate from the blob itself.

Tracked here as an open thread; details belong in a follow-on chapter
once the orange Gateway tier is real.

---

## 7. Mapping back to chapter 16/17 vocabulary

Shared (chapter 15):

| Orange concept (proposed) | Chapter 15 concept |
|---|---|
| `keys[id]` | materialized Pear Key blob |

Part I — LLM (chapter 16):

| Orange concept (proposed) | Chapter 16 concept |
|---|---|
| `keys[id].llm.models[m].routing` | `routing` resolved at Key scope (Default mode) |
| `llm.providers[p].bindings[]` | `provider_binding` (Default, Workspace-only in ch.16; here proxy-wide catalog) |
| `llm.providers[p].auth.secret_ref` | Pear-pool credential (no BYOK in v1) |

Part II — MCP (chapter 17):

| Orange concept (proposed) | Chapter 17 concept |
|---|---|
| `/mcp/<profile-id>` path → `mcp.profiles[id].owner` | attribution principal (`owner = f(mcp-path)`, gate-independent) |
| `mcp.profiles[name]` | `profile_spec` |
| `mcp.profiles[name].fanout_failure` | `profile_spec.fanout_failure` |
| `mcp.profiles[name].tools[srv].include` | `mcp_tool_allowlist` per member |
| `mcp.servers[name]` | catalog entry + (implicit) Catalog blob |
| `mcp.servers[name].fallback[]` | below-policy server cluster retry |

The structural alignment is intentional: the `keys[]` table is exactly
the chapter-15 "materialized blob per Pear Key" model. The hand-authored
YAML is a stopgap; the config provider that talks to the control plane
will push the same `keys[id]` blobs (packed, per § 5.1) to each orange
instance — same schema, same lookup, no manual materialization step.

