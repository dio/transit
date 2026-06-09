# orange local development quickstart

Two commands cover the full local dev loop:

```
orange server --local              # server + bootstrap + admin REPL
orange egress serve --local        # envoy + redis + rls + egress REPL (standalone)
ORANGE_SERVER_URL=http://localhost:3000 \
  orange egress serve --local \
  --bundle=<egress-id>.tar.gz     # same stack, config from server
```

---

## `orange server --local`

Starts the orange management plane for local development. On first run it
bootstraps everything from `orange.yaml`, then drops into the admin REPL.
Ctrl-D / `exit` in the REPL shuts the server down.

### What it does on startup

1. Starts embedded Postgres under `~/.orange/data/` and loads (or generates) a
   local KEK at `~/.orange/kek`.
2. If the database is empty **and `--no-seed` was not given**, bootstraps from
   `orange.yaml`:
   - Reads unique workspace names from the first path component of each key in
     the `keys:` map. Example: `demo/dio/sk-default` → workspace `demo`.
   - Creates org `orange.io`, project `proj1`, one workspace per derived name,
     user `dio@orange.io`, admin user `admin@orange.io` with an org-admin API key,
     and an egress record per workspace.
3. Starts the HTTP/2 management plane on `:3000` (override with `--port`).
4. Prints `export ORANGE_API_KEY=<key>` — save this for future `--no-purge`
   restarts and for `orange admin` sessions from a second terminal.
5. Enters the **admin REPL** seeded with the org and project context.

Re-runs on existing data skip the bootstrap and enter the REPL directly.
To wipe and start fresh, add `--purge` (see Flags below).

### Flags

| Flag | Default | Env | Description |
|---|---|---|---|
| `--local` | — | — | enable local mode (required) |
| `--purge` | false | — | wipe `~/.orange/data/` and `~/.orange/kek` before starting (requires `--local`) |
| `--no-seed` | false | — | skip auto-bootstrap after `--purge`; start with an empty DB |
| `--config` | `orange.yaml` | `ORANGE_CONFIG` | config file for workspace derivation |
| `--org` | `orange.io` | — | org name |
| `--project` | `proj1` | — | project name |
| `--user` | `dio` | — | initial workspace member |
| `--port` | `3000` | `PORT` | listen port |
| `--public-url` | `http://localhost:<port>` | `ORANGE_PUBLIC_URL` | URL written into egress bundles |

### First run

```bash
cd examples/orange
orange server --local
```

Output:

```
# local dev server ready
# admin: admin@orange.io
export ORANGE_API_KEY=sk-org-<key>
# user:  dio@orange.io
export ORANGE_USER_API_KEY=sk-<key>
orange [orange.io / proj1]>
```

Save both keys. `ORANGE_API_KEY` is the admin key for the management API;
`ORANGE_USER_API_KEY` is the workspace-scoped key used to issue PASETO tokens
and switch to the user REPL (`su $ORANGE_USER_API_KEY`). The prompt is now the
admin REPL.

### Admin REPL — useful commands after start

```
# List what was created
ws list              # show workspaces
egress list          # show egress records per workspace
member list          # show workspace members

# Download the egress bundle so you can run egress serve --local --bundle
egress bundle <egress-id>     # writes <egress-id>.tar.gz in cwd
# or interactively:
ws                             # show ws IDs
egress bundle                  # prompts if >1 egress

# Config snapshot is auto-published from orange.yaml on first start (--purge).
# Re-publish after editing orange.yaml:
config publish orange.yaml ws=<ws-id>

# User operations
user list
apikey list

help                  # full command reference
exit                  # shut down server and exit
```

### Restarting (re-attach to existing data)

```bash
export ORANGE_API_KEY=sk-org-<key>   # from the first-run output
orange server --local
```

The server starts, detects existing org data, skips bootstrap, and enters the
admin REPL using your existing API key.

### Starting fresh

```bash
orange server --local --purge
```

Wipes `~/.orange/data/` and `~/.orange/kek`, then re-bootstraps from
`orange.yaml`. Add `--no-seed` to skip the bootstrap and start with an empty DB.

---

## `orange egress serve --local` — standalone mode

Runs the full local egress stack (Envoy + redis-server + in-process RLS) using
a local `orange.yaml` as the config source. No orange server needed.

```bash
orange egress serve --local
orange egress serve --local --config path/to/orange.yaml
```

The REPL prompt is `egress:local>`. Hot-reload happens automatically on save.

### Prerequisites

```bash
brew install redis          # redis-server must be in PATH
export ENVOY_BIN=...        # or: make download-envoy in the transit repo root
```

---

## `orange egress serve --local --server-url` — connected mode

Runs the same local stack (Envoy + redis + RLS + REPL) but pulls config from a
running orange server instead of a local yaml file. Use this after
`orange server --local` has bootstrapped and published a workspace snapshot.

```bash
ORANGE_SERVER_URL=http://localhost:3000 \
  orange egress serve --local \
  --bundle <egress-id>.tar.gz
```

Or with explicit flags:

```bash
orange egress serve --local \
  --server-url http://localhost:3000 \
  --bundle <egress-id>.tar.gz
```

### What it does

1. Loads `<egress-id>.tar.gz` for bundle credentials.
2. Overrides the bundle's baked-in server URL with `ORANGE_SERVER_URL`.
3. Fetches the initial config snapshot from the server and writes it to a temp
   yaml file.
4. Starts redis, RLS, and Envoy pointing at that temp file (identical to
   standalone mode).
5. Background poller re-fetches every `--interval` (default 30 s); when the
   snapshot changes the temp file is atomically replaced and the existing
   `WatchFile` goroutine triggers `provider.Reload` in the RLS.
6. Enters the `egress:local>` REPL exactly as in standalone mode.

### Flags

| Flag | Default | Env | Description |
|---|---|---|---|
| `--server-url` | — | `ORANGE_SERVER_URL` | orange server URL (triggers connected mode) |
| `--bundle` | — | `ORANGE_EGRESS_BUNDLE` | bundle path; required with `--server-url` |
| `--interval` | `30s` | — | snapshot poll interval |
| `--no-rls` | false | — | skip redis + RLS (Envoy only) |
| `--rls-listen` | `:8081` | — | RLS gRPC listen address |

### Full two-terminal workflow

**Terminal 1 — server:**

```bash
cd examples/orange
orange server --local
# → export ORANGE_API_KEY=sk-org-<key>
# → orange [orange.io / proj1]>
```

In the admin REPL:

```
egress bundle <egress-id>           # write <egress-id>.tar.gz
# Config snapshot was auto-published from orange.yaml on startup.
# Re-publish after changes: config publish orange.yaml ws=<ws-id>
```

**Terminal 2 — egress:**

```bash
export ORANGE_API_KEY=sk-org-<key>  # from terminal 1
ORANGE_SERVER_URL=http://localhost:3000 \
  orange egress serve --local \
  --bundle <egress-id>.tar.gz
```

Config changes pushed via `config push` in the server REPL propagate to the
egress stack within one poll interval.
