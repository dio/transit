---
name: envoy-dynamic-modules
description: Build and debug Envoy HTTP dynamic modules in transit. Use when a .so loads but a filter does not register, callback hooks do not fire, filter_name/dynamic_module_config wiring may be wrong, module init depends on env vars, or an Envoy dynamic-module e2e unexpectedly routes past the filter.
---

# Envoy Dynamic Modules - transit

Use this skill for HTTP dynamic module loading and registration failures in
Transit examples or e2e suites. Use `envoy-abi-wrapper` instead when changing
the low-level C ABI wrappers, and use `transit-e2e-runner` for ordinary e2e
command selection.

## Mental Model

There are four distinct checkpoints. Debug them in order:

1. **Shared library built**: the expected `lib*.so` exists and is the one Envoy
   is loading.
2. **Module loaded**: Envoy reports the dynamic module ABI version matched.
3. **Filter registered**: the Go package `init()` reached `up.Register*` with a
   name matching Envoy's `filter_name`.
4. **Callbacks invoked**: request headers/body callbacks run and affect the
   stream before Envoy's router forwards it.

Do not jump to callout, routing, or body-buffering explanations until checkpoint
3 is proven.

## Required Entrypoint Shape

Every HTTP filter shared-library command should look like:

```go
package main

import (
	"log"

	_ "github.com/dio/transit/down/abi_impl"
	example "github.com/dio/transit/examples/<name>"
)

func init() {
	config, err := example.LoadConfigFromEnv()
	if err != nil {
		log.Printf("<name>: %v", err)
		return
	}
	example.RegisterTransitFilter("<registered-filter-name>", config)
}

func main() {}
```

Rules:

- The blank `down/abi_impl` import belongs in the `cmd/main.go` package built
  with `-buildmode=c-shared`, not in a reusable library package.
- Registration must happen from `init()`. Envoy loads the `.so`; it does not run
  `main()`.
- If config loading fails and `init()` returns before `up.Register*`, Envoy can
  still load the module, but the filter will not be registered.
- Keep reusable code in the library package. Keep Envoy ABI details out of
  unit-testable pure Go packages.

## Envoy Config Wiring

For HTTP filters, these three names are different and must be checked:

```yaml
dynamic_module_config:
  name: <module-file-stem-or-configured-module-name>
filter_name: <registered-filter-name>
```

- `dynamic_module_config.name` selects the loaded module. In examples it should
  usually match the shared-library stem without `lib`/`.so`.
- `filter_name` must match the exact string passed to `up.Register`,
  `up.RegisterWithBody`, or `up.RegisterWithMutableBody`.
- The enclosing `http_filters[].name` is mostly Envoy config naming. Keep it
  readable, but do not confuse it with the registered Go filter name.

For body-dependent filters:

- Use `up.RegisterWithMutableBody` when the handler needs the full request body
  or needs `SetRequestBody`.
- When the body callback initiates an `HTTPCallout` and sends a local response
  from the callout callback, the fallback route cluster **must be reachable**.
  The header callback returns `Continue`, so Envoy's router immediately attempts
  to connect to the fallback cluster. If that connection fails (e.g. port 1 /
  mcp-blackhole), Envoy calls `OnStreamComplete` and sets `streamDone=true`
  before the callout callback fires — the callback is then silently skipped and
  the client sees the upstream error instead of the filter's local response.
  Use a cluster backed by a real listener (e.g. the same cluster the callout
  targets) as the fallback. The e2e can still prove the filter intercepted by
  asserting that the upstream received no credential headers (only the callout
  path strips them).

## Fast Failure Triage

When Envoy e2e unexpectedly routes past a filter:

1. Confirm the shared library was rebuilt after edits. Do not use cached builds
   or `TRANSIT_SKIP_BUILD=1` while debugging registration.
2. Check Envoy stderr for `Dynamic module ABI version ... matched`. If absent,
   inspect `ENVOY_DYNAMIC_MODULES_SEARCH_PATH`, module name, file location, and
   build errors.
3. Add a temporary log immediately before `RegisterTransitFilter`. If it does
   not appear, the package `init()` did not reach registration; check env vars,
   JSON config, and validation.
4. Temporarily make config loading fail loudly in the test by asserting the
   env var string and keeping the Envoy stderr visible. A logged config error
   followed by no registration is a registration failure, not a filter bug.
5. Add a temporary request-header marker in the handler:

   ```go
   w.SetRequestHeader("x-debug-filter-seen", "1")
   ```

   Route to a reachable recorder cluster only for this diagnostic. If the
   recorder does not see the marker, callbacks are not invoked. If it sees the
   marker but the body/local-response behavior fails, move to body/callout
   debugging.
6. Remove all temporary marker headers, direct-recorder routes, and init logs
   before finishing.

## Common Symptoms

`Dynamic module ABI version matched`, then request routes normally:

- Module loaded, but the filter may not be registered or the configured
  `filter_name` does not match.
- Check `cmd/main.go` `init()`, environment config, and `filter_name`.

`upstream connect error ... remote connection failure` from a blackhole route:

- This often means the filter did not stop the request before router routing.
- First prove the handler callback ran. Then inspect body callback status or
  `HTTPCallout` state.

Direct route to the intended upstream works, but callout path fails:

- The cluster endpoint is reachable. Inspect `HTTPCalloutRequest.Cluster`,
  required pseudo headers, `host`, `:path`, and callout result handling.

Header callback marker appears, but body callback does not:

- Confirm the filter was registered with `RegisterWithBody` or
  `RegisterWithMutableBody`.
- Confirm the request really has a body or that `endOfStream` synthetic body
  callbacks are expected for the method under test.

## Validation

For example HTTP dynamic modules:

```sh
make -C examples/<name> test
make -C examples/<name> e2e
git diff --check -- examples/<name>
```

If the e2e starts local listeners or Envoy must connect to in-process upstreams,
restricted sandboxes may block it. Rerun the same `make -C examples/<name> e2e`
with permission to bind/connect on loopback rather than changing the test to
avoid Envoy.
