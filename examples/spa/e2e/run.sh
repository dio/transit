#!/usr/bin/env bash
# Run the spa e2e tests against a real Envoy instance.
#
# Usage:
#   ./e2e/run.sh                      # build .so, start Envoy, run tests
#   TRANSIT_SKIP_BUILD=1 ./e2e/run.sh # reuse existing libspa.so
#   ENVOY_BIN=.bin/envoy ./e2e/run.sh
#
# Requires:
#   - Go with CGO enabled
#   - Envoy at $ENVOY_BIN (default: .bin/envoy from transit project root)
#   - Node >= 24
#   - Chrome installed for Playwright (run: npm --prefix examples/spa/e2e exec -- playwright install chrome)
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
spa_dir="$(cd "$script_dir/.." && pwd)"
project_root="$(git rev-parse --show-toplevel)"

envoy_bin="${ENVOY_BIN:-$project_root/.bin/envoy}"
so_path="$spa_dir/libspa.so"

free_port() {
  node -e '
    const net = require("node:net");
    const server = net.createServer();
    server.listen(0, "127.0.0.1", () => {
      console.log(server.address().port);
      server.close();
    });
  '
}

if [[ ! -x "$envoy_bin" ]]; then
  echo "ERROR: Envoy not found at $envoy_bin (run: make download-envoy)" >&2
  exit 1
fi

# Build the .so unless TRANSIT_SKIP_BUILD=1.
if [[ "${TRANSIT_SKIP_BUILD:-}" != "1" ]]; then
  echo "==> building libspa.so ..."
  (cd "$project_root/examples" && CGO_ENABLED=1 GOWORK=off go build \
    -trimpath \
    -buildmode=c-shared \
    -o "$so_path" \
    ./spa/cmd)
  echo "==> build OK: $so_path"
else
  if [[ ! -f "$so_path" ]]; then
    echo "ERROR: TRANSIT_SKIP_BUILD=1 but $so_path not found" >&2
    exit 1
  fi
  echo "==> reusing $so_path (TRANSIT_SKIP_BUILD=1)"
fi

# Install e2e npm deps. Use npm ci when a lockfile is present, otherwise
# fall back to npm install so a fresh checkout with only package.json works.
if [[ -f "$script_dir/package-lock.json" ]]; then
  npm ci --prefix "$script_dir" --silent
else
  npm install --prefix "$script_dir" --silent
fi

proxy_port="$(free_port)"
admin_port="$(free_port)"
admin_url="http://127.0.0.1:$admin_port"
spa_url="${SPA_URL:-http://127.0.0.1:$proxy_port}"
cfg_path="$(mktemp "${TMPDIR:-/tmp}/transit-spa-e2e.XXXXXX.yaml")"

sed \
  -e "s/{{.ProxyPort}}/$proxy_port/g" \
  -e "s/{{.AdminPort}}/$admin_port/g" \
  "$script_dir/testdata/envoy.tmpl.yaml" > "$cfg_path"

# Start Envoy.
GODEBUG=cgocheck=0 \
ENVOY_DYNAMIC_MODULES_SEARCH_PATH="$spa_dir" \
  "$envoy_bin" -c "$cfg_path" --log-level warning &
envoy_pid=$!
trap 'kill "$envoy_pid" 2>/dev/null || true; rm -f "$cfg_path"' EXIT

# Wait for Envoy to be ready (up to 10 s).
ready=
for _ in {1..50}; do
  if ! kill -0 "$envoy_pid" 2>/dev/null; then
    echo "ERROR: Envoy exited unexpectedly" >&2
    wait "$envoy_pid"
  fi
  if curl -fsS "$admin_url/ready" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 0.2
done
if [[ "$ready" != "1" ]]; then
  echo "ERROR: Envoy not ready after 10s" >&2
  exit 1
fi
echo "==> Envoy ready (pid=$envoy_pid)"

# Run the tests.
SPA_URL="$spa_url" node --test "$script_dir/spa.test.mjs"
