# Orange Loader: File-Based Bootstrap for Local Demos

File-based bootstrap (in `internal/config/loader/loader_file.go`) enables the orange Envoy module to operate independently without a remote control plane by loading config from a local YAML file at startup.

## Overview

Orange supports two operating modes:

| Mode | Setup | Use Case |
|---|---|---|
| **Local file** | `ORANGE_CONFIG=orange.yaml make demo` | Development, demos, testing |
| **Control plane** | No `ORANGE_CONFIG`, egress pulls from CP | Production |

Both modes use the same codebase; the loader makes the choice automatic at runtime.

## Local File Mode (`make demo`)

When `ORANGE_CONFIG` is set, the loader runs at module init time and:

1. Reads the YAML file
2. Compiles it into an `AppState` (with validation)
3. Distributes the `AppState` to all five pipeline packages (`pick`, `match`, `adapt`, `mcp`, `responsesws`)
4. Starts a background file watcher using `fsnotify` for instant change detection
   (automatically falls back to 5-second polling if `fsnotify` fails)

### Usage

```bash
# Build the module
make build

# Run Envoy with config from a local file
ORANGE_CONFIG=examples/orange/orange.yaml make run

# Or directly with envoy:
ORANGE_CONFIG=examples/orange/orange.yaml \
  ENVOY_DYNAMIC_MODULES_SEARCH_PATH=. \
  /path/to/envoy -c envoy.yaml --log-level info
```

### Live Reload

The background watcher automatically reloads `ORANGE_CONFIG` when the file changes. File changes are detected instantly via `fsnotify`, with a 5-second fallback to `mtime` polling if `fsnotify` fails. Envoy continues running — no restart required.

```bash
# While `make run` is executing:
$ echo "... edit orange.yaml ..."
$ # within 2 seconds the loader reloads and redistributes to all packages
$ # Envoy processes new requests using the updated config
```

This enables the tight edit-test loop needed for `make demo`.

## Control Plane Mode (Production)

When `ORANGE_CONFIG` is unset, the loader is silent and returns immediately. Each pipeline package holds an `AppState` pointer initialized to `nil`:

```go
var matchAppState *config.AppState  // nil at startup
```

During request handling, packages check `if matchAppState == nil` and block or error appropriately, signaling that config must be pushed from the control plane.

When an egress connects, the CP's `ConfigService.Fetch()` RPC returns a snapshot envelope. The egress client decodes it and calls the package-level `SetAppState` to inject the live config:

```go
match.SetAppState(appState)
adapt.SetAppState(appState, resolver)
mcp.SetAppState(appState, resolver)
pick.SetAppState(appState)  // package-level, affects future clusters
responsesws.SetAppState(appState, resolver)
```

Requests from that point forward use the live `AppState`.

## Init Order

Go initialises packages in dependency order, which is critical:

1. **Pipeline packages** (`pick`, `match`, `adapt`, `mcp`, `responsesws`) call `init()` and register themselves with Envoy's SDK
2. **Loader package** calls `init()` and injects `AppState` into the now-initialized pipeline packages
3. **Envoy** queries the SDK and instantiates filters/clusters, which inherit the `AppState` from the loader

The ordering is enforced by the import graph in `cmd/module/main.go`:

```go
import (
    _ "github.com/dio/transit/examples/orange/internal/pipeline/pick"
    _ "github.com/dio/transit/examples/orange/internal/pipeline/match"
    // ... other pipeline packages ...
    _ "github.com/dio/transit/examples/orange/internal/loader"  // last
)
```

Because `loader` doesn't import any pipeline packages, it has no dependency on them. Go places it last in the topological sort, ensuring all pipeline inits run first.

## Package-Level `SetAppState`

Each pipeline package exports `SetAppState` with a signature matching its needs:

- `pick.SetAppState(s *config.AppState)` — only state
- `match.SetAppState(s *config.AppState)` — only state
- `adapt.SetAppState(s *config.AppState, r config.SecretResolver)` — state + secrets
- `mcp.SetAppState(s *config.AppState, r config.SecretResolver)` — state + secrets
- `responsesws.SetAppState(s *config.AppState, r config.SecretResolver)` — state + secrets

The loader calls all five in `distribute()` using the single `AppState` and `SecretResolver` loaded from the file.

For `pick` specifically, the global `SetAppState` affects future cluster creation:

```go
var globalPickAppState *config.AppState

func SetAppState(s *config.AppState) {
    globalPickAppState = s
}

func (f cfgFactory) NewCluster(h up.ClusterHandle) up.Cluster {
    return &cluster{handle: h, logger: f.logger, appState: globalPickAppState}
}
```

This ensures that when Envoy creates clusters after module init, they inherit the loaded config.

## File Format

The config file is a standard Orange YAML snapshot. Since it must compile cleanly and be fully contained for a single workspace, it must follow:

- Profile IDs: `workspace/user/name` (3 segments, e.g. `demo/maya/default`)
- Key IDs: `workspace/user/sk-name` (3+ segments, e.g. `demo/maya/sk-fallback`)
- Scope: the file represents a single workspace, so all user records must belong to that workspace

Example:

```yaml
llm:
  providers:
    anthropic:
      kind: anthropic
      auth:
        type: anthropic
        secret_ref: env://ANTHROPIC_API_KEY

  models:
    claude-haiku-4-5:
      provider: anthropic
      name: claude-haiku-4-5-20251001

profiles:
  demo/maya/default:
    tools:
      kiwi:
        include: ["search-flight"]

keys:
  demo/maya/sk-fallback:
    routing_overrides:
      claude-haiku-4-5:
        chain:
          retry:
            retry_on: "connect-failure,reset,5xx,retriable-status-codes"
          children:
            - target: { provider: anthropic, name: claude-haiku-4-5 }

mcp:
  servers:
    kiwi:
      endpoint: https://mcp.kiwi.com
      namespace: kiwi
      tools_include: ["search-flight"]
```

## Error Handling

If the file cannot be read or parsed, the loader logs an error and exits:

```
loader: failed to load ORANGE_CONFIG: path=/path/to/orange.yaml err=<error>
```

This is intentional — a bad config at startup is fatal and should not be masked.

If a reload fails (live watcher detects a change but the new file is invalid), the watcher logs a warning and keeps the previous config in place:

```
loader: reload failed: path=/path/to/orange.yaml err=<error>
```

Requests continue using the last-known-good snapshot until a valid file is written.

## Secrets

The loader uses `config.NewDefaultResolver(5 * time.Minute)`, which dispatches across three built-in schemes:

- `env://VAR` — read from environment variable `VAR`
- `file://path` — read from file at `/path` (absolute)
- `literal://value` — inline literal string (for testing only)

For production `make run`, ensure all referenced secrets are set in the environment:

```bash
export ANTHROPIC_API_KEY="sk-ant-..."
export OPENAI_API_KEY="sk-..."
export GITHUB_TOKEN="gh_..."
make run
```

The Makefile already checks for required variables before starting Envoy (see the `run` target in `examples/orange/Makefile`).

## Demo Workflow

To develop and test locally:

```bash
# Terminal 1: build and run Envoy with config reload
cd examples/orange
make demo

# Terminal 2: run a demo request
curl -s localhost:8080/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}'

# Terminal 3 (optional): edit orange.yaml
# The loader picks up the change within 2 seconds; Terminal 2 requests use the new config
```

## Implementation Details

The file-based loader is in `internal/config/loader/loader_file.go` and is blank-imported from `cmd/module/main.go`. It's in a subpackage to avoid import cycles with the config package.

Key functions:

- `loadFile(state, path)` — reads YAML and calls `state.LoadConfig()`
- `distribute(state, resolver)` — calls `SetAppState` on each pipeline package
- `watchFile(path, state, resolver)` — watches file changes using `fsnotify` with 5-second fallback polling
- `watchFilePolling(path, state, resolver)` — polling-based fallback (used if fsnotify fails)
- `reloadIfModified(path, lastMod, state, resolver)` — checks mtime and reloads if changed

The watcher uses `fsnotify` for efficient file watching with OS-level notifications. If fsnotify initialization fails or encounters errors, it automatically falls back to polling with a 5-second check interval. A 5-second fallback timer also ensures that changes are detected even if fsnotify events are missed.
