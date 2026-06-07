# orange egress emulate --repl

Interactive REPL mode for the egress emulator. Adds `--repl` to the existing
`orange egress emulate` command so an operator can query and observe the
configuration delivery mechanics interactively — the same Fetch/heartbeat loop
a real egress (Envoy dynamic module) runs, but with human-readable output and
on-demand commands.

## Goal

Exercise the CP↔egress communication path — configuration delivery and
mechanics — without deploying Envoy. The emulator impersonates a real egress:
loads a bundle, signs assertions, sends heartbeats, polls config snapshots, and
resolves secret refs. REPL mode makes this interactive.

## Non-goals

- gRPC server-streaming Watch. The CP may not support Watch for quite some
  time; the emulator uses Fetch polling exclusively, identical to what the
  production Envoy module will use until Watch is available.
- Direct config mutations from within the egress emulator. Config changes
  are made through admin or user-scoped tasks/records and are then observed
  by the emulator as they arrive in the next config snapshot. The egress-facing
  path is read-only by design — an egress consumes configuration, it does not
  produce it.

  Admin-scoped mutations: `orange admin --repl`
  User-scoped mutations: `ORANGE_API_KEY=<user-api-key> orange --repl` (planned —
  will let users manage their own records, e.g. provider credentials and MCP
  server entries, that feed into the workspace config snapshot delivered to
  egresses).

## Background mechanics

A real egress:

1. Loads its bundle (egress ID, workspace ID, Ed25519 signing key, server URL,
   PASETO public keys).
2. On every request, signs a short-lived Ed25519 assertion and sends it in
   `X-Egress-Assertion`. No mTLS; the assertion is transport-independent.
3. Periodically calls `EgressService.Heartbeat` to mark itself online.
4. Periodically calls `SnapshotService.Fetch` with `last_version` +
   `last_checksum` to receive the workspace config snapshot. The server returns
   `Unchanged` when nothing has changed since the last fetch.
5. Decodes the snapshot, resolves embedded `secret_ref` strings (env, file,
   literal schemes), and applies the configuration.

The REPL drives steps 3–5 in the background and exposes them as interactive
commands.

## Architecture

### Polling model (no Watch stream)

A single background goroutine runs a periodic poll loop:

```
every <interval>:
    Heartbeat()                → update watcher.lastHeartbeat
    Fetch(lastVersion, lastChecksum)
        → Unchanged: no-op
        → Snapshot: decode, resolve secrets, update watcher state, print notification
```

No gRPC server-streaming. No backoff-on-reconnect complexity. The loop exits
when the parent context is cancelled (CTRL-C).

### Shared state

```go
type egressWatcher struct {
    mu sync.RWMutex

    snap         *configv1.SnapshotEnvelope  // nil until first successful Fetch
    raw          *config.RawConfig
    lastVersion  uint64
    lastChecksum []byte

    pollStatus    string    // "polling", "error", "stopped"
    pollErr       error     // last Fetch/Heartbeat error
    lastHeartbeat time.Time
    lastFetch     time.Time

    history []egressChangeEntry  // ring buffer, cap=20
}

type egressChangeEntry struct {
    receivedAt time.Time
    version    uint64
    checksum   []byte
    source     string // always "fetch" for now
    providers  int
    servers    int
    profiles   int
}
```

The poll goroutine holds the write lock only during the state swap
(O(1) — pointer swap + append). REPL commands read under the read lock.

### Async notifications

When the poll goroutine receives a new snapshot it writes a one-line
notification to stderr and prints a new prompt line:

```
[poll] config updated: version=6 providers=2 servers=3 profiles=1
egress [v=6 poll=ok]>
```

Writing to stderr avoids corrupting readline's stdout-owned input line. No
`rl.Refresh()` call (not part of the readline API).

### Context and shutdown

```
cmd.Context()  ←  CTRL-C cancels this
    └─ passed to runEgressEmulateREPL
           ├─ poll goroutine: exits when ctx.Done()
           └─ readline loop: exits when ctx.Done() or EOF
```

`runEgressEmulateREPL` starts the poll goroutine first, then enters the
readline loop. On CTRL-C the parent context is cancelled; the poll goroutine
exits cleanly on its next iteration, and the readline loop receives an
interrupt.

## Command set

```
snapshot [full]          show metadata of current snapshot;
                         'full' also prints decoded RawConfig as YAML
secrets                  list all secret refs in snapshot (values masked)
resolve <ref>            resolve a specific ref (e.g. env://ANTHROPIC_API_KEY)
fetch                    manually trigger one Fetch cycle
heartbeat                manually trigger one Heartbeat
poll status              show poll goroutine status, last heartbeat/fetch times
poll history             show last N snapshots received (version, time, counts)
status                   combined: heartbeat result + poll status + snapshot summary
help / ?
exit / quit / Ctrl+D
```

`fetch` and `heartbeat` are one-shot calls from the REPL goroutine, independent
of the background poll loop. They update `egressWatcher` state the same way.
This is how the production Envoy module will work: heartbeat runs on one timer,
snapshot fetch on another.

## Prompt

```
egress [v=42 poll=ok]>
egress [v=42 poll=error]>
egress [no snapshot poll=ok]>
```

`egressReplState.prompt()` acquires a read lock on `egressWatcher` to read
`lastVersion` and `pollStatus`. Called on every `rl.SetPrompt()` — O(1).

## File layout

Two new files, one modification:

```
internal/server/egress_emulate.go          (modified) add --repl flag
internal/server/egress_emulate_repl.go     (new) REPL entry, dispatch, commands
internal/server/egress_watcher.go          (new) egressWatcher, poll loop
```

`egress_watcher.go` has no readline dependency. It is the unit that will be
lifted into the Envoy dynamic module package when the time comes. It only
imports config, egress, and connect packages.

`egress_emulate_repl.go` imports readline and owns the interactive surface.

### egress_emulate.go change

```go
// in newEgressEmulateCmd():
var replMode bool
cmd.Flags().BoolVar(&replMode, "repl", false, "run interactive REPL (poll mode)")

// in RunE, after bundle loading:
if replMode {
    return runEgressEmulateREPL(cmd.Context(), bundle, interval)
}
return runEgressEmulate(cmd.Context(), bundle, interval, once)
```

All existing flags (`--bundle`, `--interval`, `--once`) remain unchanged.
`--interval` is reused as the poll interval in REPL mode.

## egress_watcher.go sketch

```go
package server

type egressWatcher struct { /* as above */ }

// startPoll launches the background poll goroutine and returns immediately.
func (w *egressWatcher) startPoll(
    ctx context.Context,
    interval time.Duration,
    heartbeatClient egressv1connect.EgressServiceClient,
    snapshotClient  configv1connect.SnapshotServiceClient,
    resolver        *config.CachedResolver,
    notifyFn        func(version uint64),  // called after state swap, from poll goroutine
) {
    go w.pollLoop(ctx, interval, heartbeatClient, snapshotClient, resolver, notifyFn)
}

func (w *egressWatcher) pollLoop(ctx context.Context, interval time.Duration, ...) {
    ticker := time.NewTicker(interval)
    defer ticker.Stop()
    w.tick(ctx, ...)   // immediate first tick
    for {
        select {
        case <-ctx.Done():
            w.setStatus("stopped", nil)
            return
        case <-ticker.C:
            w.tick(ctx, ...)
        }
    }
}

func (w *egressWatcher) tick(ctx context.Context, ...) {
    // 1. Heartbeat
    // 2. Fetch with lastVersion/lastChecksum
    // 3. If new snapshot: decode, resolve refs, update state, call notifyFn
}

// Fetch performs a one-shot Fetch, same as tick's step 2.
// Called by the REPL 'fetch' command.
func (w *egressWatcher) Fetch(ctx context.Context, ...) error { ... }

// Heartbeat performs a one-shot Heartbeat.
// Called by the REPL 'heartbeat' command.
func (w *egressWatcher) Heartbeat(ctx context.Context, ...) error { ... }
```

`notifyFn` is called from the poll goroutine after the state swap. In
`runEgressEmulateREPL` it prints the one-line notification to stderr.

## egress_emulate_repl.go sketch

```go
type egressReplState struct {
    bundle          *egress.BundleData
    watcher         *egressWatcher
    resolver        *config.CachedResolver
    heartbeatClient egressv1connect.EgressServiceClient
    snapshotClient  configv1connect.SnapshotServiceClient
    rl              *readline.Instance
}

func runEgressEmulateREPL(ctx context.Context, bundle *egress.BundleData, interval time.Duration) error {
    // 1. Build transport (assertion signing), clients, resolver — same as runEgressEmulate.
    // 2. Create egressWatcher.
    // 3. Start poll goroutine (notifyFn writes to stderr).
    // 4. Print startup summary.
    // 5. Enter readline loop.
}
```

## Secret resolution

A single `*config.CachedResolver` (TTL=5min) is created at startup and shared
between:

- The poll goroutine (resolves refs on each new snapshot for display).
- The `resolve <ref>` REPL command (direct call).
- The `secrets` REPL command (iterates `collectSecretRefs`, resolves each).

Resolved values are masked via `maskSecret` before printing (already in
`egress_emulate.go`).

## History ring buffer

Slice-shift append, capped at 20 entries:

```go
const maxHistory = 20

func (w *egressWatcher) appendHistory(e egressChangeEntry) {
    if len(w.history) >= maxHistory {
        w.history = w.history[1:]
    }
    w.history = append(w.history, e)
}
```

Called inside the write-lock region of `tick`.

## Testing

**Unit-testable:**
- `egressWatcher.tick` with a fake Fetch client that returns a sequence of
  snapshots and `Unchanged` responses; verify `lastVersion` advances correctly
  and `history` grows.
- `maskSecret` edge cases (0, 1, 6, 7 chars).
- `appendHistory` cap enforcement.

**Race detector:** `go test -race` with a goroutine running `pollLoop` and
concurrent reads from a simulated REPL goroutine.

**Integration (manual):**
```
orange egress emulate --repl --bundle <path>
[background poll fires immediately]
egress [v=5 poll=ok]> poll status
egress [v=5 poll=ok]> snapshot
egress [v=5 poll=ok]> secrets
egress [v=5 poll=ok]> resolve env://ANTHROPIC_API_KEY
egress [v=5 poll=ok]> poll history
egress [v=5 poll=ok]> fetch          # manual one-shot; should return Unchanged
egress [v=5 poll=ok]> heartbeat      # independent of poll loop
egress [v=5 poll=ok]> exit
```

## Relation to Envoy dynamic module

`egress_watcher.go` is designed for extraction:

- No readline imports.
- `startPoll` takes only connect clients and a `notifyFn` callback — the module
  will supply its own notify (e.g., trigger an Envoy worker-thread reload).
- The `lastVersion`/`lastChecksum` SoTW contract is identical to what the
  module will use with the CP.
- Heartbeat independence from config fetch is preserved — both are separate
  timer-driven operations in the module.

When Watch becomes available, `pollLoop` in the module is replaced with a
Watch stream loop. The `egressWatcher` state struct, the state-swap logic, and
the REPL commands are unchanged.
