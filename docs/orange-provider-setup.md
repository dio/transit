# Adding an LLM provider

This guide covers two ways to add an LLM provider to a workspace:

- [One-shot CLI](#one-shot-cli-orange-admin-provider-set) — single command, no REPL needed
- [Step-by-step REPL](#step-by-step-repl) — useful for understanding or scripting each step

In both cases, the provider's API key is stored in the orange secret service and referenced from `orange.yaml` via `orange://<ws-id>/api-keys/<name>`. The secret is resolved at request time by the proxy; it never appears in the published snapshot.

---

## One-shot CLI: `orange admin provider set`

```bash
orange admin provider set anthropic \
  --kind=anthropic \
  --ws=<ws-id> \
  --value=sk-ant-... \
  --config=orange.yaml \
  --publish
```

Or pipe the key from a secrets manager:

```bash
vault kv get -field=key secret/anthropic | \
  orange admin provider set anthropic \
    --kind=anthropic --ws=<ws-id> \
    --config=orange.yaml --publish
```

### What it does

1. **Stores the API key** in the secret service at `ws/<ws-id>/api-keys/anthropic`, enabled immediately.
2. **Patches `orange.yaml`** — adds or replaces `llm.providers.anthropic` with:
   ```yaml
   llm:
     providers:
       anthropic:
         kind: anthropic
         endpoint: https://api.anthropic.com
         extra:
           anthropic_version: "2023-06-01"
         auth:
           type: anthropic
           secret_ref: orange://<ws-id>/api-keys/anthropic
   ```
3. **Publishes** the patched YAML as a new config snapshot for the workspace (because `--publish` was given).

### Flags

| Flag | Default | Description |
|---|---|---|
| `--kind` | required | `anthropic`, `openai`, `gemini`, or a custom kind |
| `--ws` | `ORANGE_WS_ID` | workspace ID |
| `--value` | stdin `-` | API key material |
| `--endpoint` | inferred | upstream base URL (inferred for `anthropic`/`openai`/`gemini`) |
| `--auth-type` | inferred | `anthropic`, `bearer`, `gemini`, `gcp`, `aws` |
| `--config` | — | `orange.yaml` to patch in place; omit to print fragment only |
| `--publish` | false | publish snapshot after patching |

### Kind shortcuts

| `--kind` | Endpoint | Auth type | Extra |
|---|---|---|---|
| `anthropic` | `https://api.anthropic.com` | `anthropic` | `anthropic_version: 2023-06-01` |
| `openai` | `https://api.openai.com` | `bearer` | — |
| `gemini` | `https://generativelanguage.googleapis.com` | `gemini` | — |
| custom | required | `bearer` (default) | — |

### Preview without writing

Omit `--config` to print the YAML fragment without touching any files or publishing:

```bash
orange admin provider set openai --kind=openai --ws=<ws-id>
# → prints fragment to stdout
```

---

## Step-by-step REPL

Same three steps, performed manually in the admin REPL:

### 1. Store the API key

```
orange [orange.io / proj1 / demo]> secret set api-keys/anthropic my-anthropic-key
# prompts for value (hidden input)
```

This stores the key at realm `ws/<ws-uuid>/api-keys`, name `my-anthropic-key`.

### 2. Update `orange.yaml`

Add or update the provider entry, referencing the secret by its `orange://` URI:

```yaml
llm:
  providers:
    anthropic:
      kind: anthropic
      endpoint: https://api.anthropic.com
      extra:
        anthropic_version: "2023-06-01"
      auth:
        type: anthropic
        secret_ref: orange://<ws-uuid>/api-keys/my-anthropic-key
```

### 3. Publish the snapshot

In the REPL (workspace context inferred from the prompt):

```
orange [orange.io / proj1 / demo]> config publish orange.yaml
```

Or with an explicit workspace:

```
orange [orange.io / proj1]> config publish orange.yaml ws=<ws-id>
```

---

## How `orange://` secret refs work

At runtime, the proxy calls `SecretResolverService/Resolve` on the orange management plane to fetch the plaintext API key. The `orange://` URI is never expanded at publish time — the snapshot stores it verbatim.

URI format:
```
orange://<workspace-id>/<realm>/<secret-name>
```

- `workspace-id` — the workspace UUID (same as `--ws`)
- `realm` — the purpose group (e.g. `api-keys`)
- `secret-name` — the name passed to `secret set`

The workspace is also authenticated via the egress assertion on every `Resolve` call, so the realm must fall within the caller's workspace ancestry.

---

## Rotating a key

Store a new version and enable it. The old version is automatically superseded:

```bash
orange admin secret set \
  --realm=ws/<ws-id>/api-keys \
  --name=anthropic \
  --value=sk-ant-new-... \
  --enable
```

Or in the REPL:

```
orange [orange.io / proj1 / demo]> secret set api-keys/anthropic anthropic
```

The proxy picks up the new version on the next `Resolve` call (cache TTL applies).

---

## First-run shortcut: `orange server --local --purge`

When running locally, `orange server --local --purge` bootstraps and auto-publishes `orange.yaml`. If the file already contains `orange://` refs, the snapshot is published immediately. Store the secrets in the REPL after the server starts:

```
orange [orange.io / proj1 / demo]> secret set api-keys/anthropic my-key
```

The proxy resolves the `orange://` ref on the first real request — secrets don't need to exist at publish time.
