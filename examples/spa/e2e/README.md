# spa e2e

Browser-based end-to-end tests for the `examples/spa` filter using
[Lightpanda](https://lightpanda.io) (headless browser) + Playwright (CDP).

## What it tests

13 tests covering the full SPA + API surface:

| Suite | Tests |
|-------|-------|
| Home page (`/`) | `#root` element, `h2` heading, nav links |
| About page (`/about`) | direct navigation renders client-side route |
| Dashboard (`/dashboard`) | heading, `/api/time` fetch button returns ISO timestamp |
| SPA fallback | `/unknown-page` and `/deep/nested/route` return `index.html` |
| `GET /api/hello` | JSON body, `filter: "api-backend"` field |
| `GET /api/time` | valid ISO 8601 UTC timestamp |
| `GET /api/unknown` | 404 with `{"error":"not found"}` |
| Static assets | `/assets/*` has `immutable` cache header; `index.html` has `no-cache` |

## Prerequisites

- Envoy binary at `../../.bin/envoy` (run `make download-envoy` from project root)
- Node >= 24 (`@lightpanda/browser` requires it)
- `npm ci` (run once, or let `run.sh` do it)

## Run

```sh
# From the examples/spa directory — builds libspa.so, starts Envoy, runs tests:
make e2e

# Or directly:
bash e2e/run.sh

# Skip the .so build (reuse existing libspa.so):
TRANSIT_SKIP_BUILD=1 bash e2e/run.sh

# Run tests against an already-running Envoy:
SPA_URL=http://localhost:10000 node --test spa.test.mjs
```

## How it works

`run.sh`:
1. Builds `libspa.so` from `examples/spa/cmd`
2. Runs `npm ci` to install Lightpanda + Playwright
3. Starts Envoy with the spa filter loaded, waits for `/ready`
4. Runs `node --test spa.test.mjs`
5. Kills Envoy on exit (trap)

`spa.test.mjs` uses `before()`/`after()` (node:test) to start Lightpanda
on port 9222 and connect Playwright over CDP. Each test opens a page and
closes it in a `withPage()` helper: Lightpanda only supports one page at
a time.

`page.evaluate()` quirk: no `async/await` inside `evaluate` — use
`function()` + `.then()` chaining + `JSON.stringify` return.

## File structure

```
examples/spa/e2e/
  spa.test.mjs       13 browser tests (node:test + Playwright + Lightpanda)
  run.sh             build .so, start Envoy, run tests, tear down
  package.json       pinned: @lightpanda/browser 1.3.1, playwright-core 1.60.0
  .gitignore         node_modules/
```
