# Provider and MCP Server Add/Remove Runbook

This runbook covers adding and removing LLM providers and MCP servers after
WS-H ships. Two paths exist depending on whether the provider uses public WebPKI
or a private CA.

Related: `docs/orange-pipeline-sdk/ws-h-transport.md`, `docs/auto-host-sni-verdict.md`.

## Concepts

**HostSpec.Hostname** — the FQDN stored on each dynamic-module host at
`AddHosts` time. Envoy reads this via `HostDescription::hostname()` when
establishing a TLS connection. `auto_host_sni` uses it as the TLS SNI;
`auto_sni_san_validation` uses it as the SAN match target.

**Pipeline config** — the JSON (or file/polling) config consumed by the
dynamic module. Provider catalog changes flow through this config; the cluster
extension reacts on the next refresh cycle without any Envoy or Gateway restart.

**EPP (EnvoyPatchPolicy)** — the Envoy Gateway resource that patches the
generated xDS cluster. Under V1, the EPP is written once per cluster and never
needs to change for provider catalog updates.

---

## Path A: Config-only (public WebPKI provider)

Use this path when the provider's TLS certificate is issued by a public CA
included in the system CA bundle (OpenAI, Anthropic, Cohere, AWS Bedrock
endpoint via public hostname, standard MCP SaaS servers, etc.).

### Add a provider

1. **Edit the pipeline config** (file source or config service) — add an entry
   to the provider catalog:

   ```json
   {
     "name": "cohere",
     "address": "api.cohere.com:443",
     "hostname": "api.cohere.com",
     "model_prefix": "command"
   }
   ```

   The `hostname` field maps to `HostSpec.Hostname`. The `address` field is the
   resolved IP:port or DNS name passed to `AddHosts`.

2. **Wait for the next poll cycle.** The dynamic module calls `AddHosts` with
   `HostSpec{Address: "api.cohere.com:443", Hostname: "api.cohere.com"}` on the
   next config refresh (interval configured in pipeline config, typically
   `refresh_millis: 200`).

3. **Verify.** Send a request routed to the new provider. Envoy derives
   `SNI = "api.cohere.com"` at connect time, presents it in the ClientHello,
   and validates the server cert SAN against the same value. No 503 from TLS
   handshake failure means the provider is reachable.

   ```sh
   curl -H "x-model: command-r-plus" http://gateway/v1/chat/completions -d '...'
   ```

4. **Check Envoy stats** (optional):

   ```sh
   curl http://admin:9901/stats | grep upstream_cx_connect_fail
   # Should remain 0 for the new provider's cluster.
   ```

### Remove a provider

1. **Remove the provider entry** from pipeline config.
2. **Wait for poll cycle.** The dynamic module calls `RemoveHosts` (or
   equivalent) on the next refresh.
3. **Verify.** Requests routed to the removed provider receive a routing error
   (no host available), not a TLS failure.

No EPP update. No Envoy restart.

### Add an MCP server (same path)

Same steps. The MCP server catalog entry carries `hostname` alongside `address`.
`x-mcp-server` header routing continues to work; the L2 cluster derives TLS
from `HostSpec.Hostname` at connect time.

---

## Path B: Two-channel rollout (private CA or mutual TLS)

Use this path when the provider uses a certificate not covered by the system CA
bundle: internal services, providers with a private intermediate CA, or any
provider requiring client certificates.

### Add a provider (private CA)

**Channel 1 — Gateway resource (minutes, controller-driven).**

1. Create a `BackendTLSPolicy` for the new CA bundle:

   ```yaml
   apiVersion: gateway.networking.k8s.io/v1alpha3
   kind: BackendTLSPolicy
   metadata:
     name: internal-provider-tls
     namespace: transit-dataplane
   spec:
     targetRefs:
       - group: ""
         kind: Service
         name: internal-provider
     validation:
       caCertificateRefs:
         - name: internal-ca-bundle
           group: ""
           kind: ConfigMap
       hostname: internal-provider.internal.example.com
   ```

2. Create the `Backend` resource pointing at the provider endpoint.

3. Wait for EG controller reconciliation (~seconds). Confirm the cluster appears
   in Envoy admin `/config_dump`.

**Channel 2 — Pipeline config (same as Path A).**

4. Add the provider entry to pipeline config with the cluster name matching
   the EG-generated cluster:

   ```json
   {
     "name": "internal-provider",
     "address": "internal-provider.internal.example.com:443",
     "hostname": "internal-provider.internal.example.com",
     "cluster_override": "backend/transit-dataplane/internal-provider/443"
   }
   ```

5. Wait for poll cycle. Traffic now flows through the EG-managed cluster with
   the private CA bundle.

### Remove a provider (private CA)

1. Remove the provider from pipeline config. Wait for poll cycle — traffic stops
   routing to the cluster.
2. Delete the `Backend` and `BackendTLSPolicy` resources. Controller cleans up
   the cluster.

Order matters: remove from config first, then remove the Gateway resources.
Reversing the order causes a brief window where requests route to a missing
cluster (503).

---

## Decision Table

| Provider characteristic | Path | EPP change? | Envoy restart? |
|---|---|---|---|
| Public CA (OpenAI, Anthropic, Cohere, AWS public endpoint) | A | No | No |
| Standard MCP SaaS server | A | No | No |
| Internal service with private CA | B | No (new BackendTLSPolicy) | No |
| Provider requiring mutual TLS (client cert) | B | No (cert in BackendTLSPolicy) | No |
| New provider family needing a dedicated cluster type | Manual | Yes | No |

A new provider family requiring a dedicated cluster type (not CLUSTER_PROVIDED
dynamic module) is a break-glass case; document the missing Envoy capability and
the rollout steps as a separate runbook entry.

---

## Verification Checklist

After adding a provider via either path:

- [ ] `upstream_cx_connect_fail` counter stays 0 for the new provider's cluster
- [ ] `ssl.handshake` counter increments on successful connections
- [ ] `ssl.fail_verify_san` counter stays 0 (SAN validation passing)
- [ ] Envoy access log shows upstream connection with correct `%UPSTREAM_HOST%`
- [ ] At least one end-to-end request succeeds with the expected response shape

After removing a provider:

- [ ] No outstanding requests are in-flight to the removed host
- [ ] Subsequent requests to the removed model/server receive a routing error,
  not a connection error
- [ ] `upstream_cx_active` for the removed host reaches 0

---

## Emergency: roll back a provider add

Both paths are reversible:

- **Path A**: remove the entry from pipeline config; next poll cycle removes the
  host. No other action needed.
- **Path B**: remove from pipeline config first, then delete Gateway resources.

If the pipeline config source is unavailable, the last-good snapshot remains
active (guaranteed by `PipelineConfig[T]` last-good behavior). The added
provider persists until the config source recovers and a successful poll removes
the entry.
