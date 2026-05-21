#!/usr/bin/env bash
# Run the request-ui e2e tests against a real Envoy instance.
#
# Usage:
#   ./e2e/run.sh                        # build .so, start servers, run tests
#   TRANSIT_SKIP_BUILD=1 ./e2e/run.sh   # reuse existing librequest-ui.so
#   ENVOY_BIN=.bin/envoy ./e2e/run.sh
#
# Requires:
#   - Go with CGO enabled
#   - Envoy at $ENVOY_BIN (default: .bin/envoy from transit project root)
#   - Node >= 22
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
example_dir="$(cd "$script_dir/.." && pwd)"
project_root="$(git rev-parse --show-toplevel)"

envoy_bin="${ENVOY_BIN:-$project_root/.bin/envoy}"
so_path="$example_dir/librequest-ui.so"
admin_url="http://127.0.0.1:9901"
ui_url="http://127.0.0.1:6062"
proxy_url="http://127.0.0.1:10000"
testserver_addr="127.0.0.1:11000"

if [[ ! -x "$envoy_bin" ]]; then
  echo "ERROR: Envoy not found at $envoy_bin (run: make download-envoy)" >&2
  exit 1
fi

# Build the .so unless TRANSIT_SKIP_BUILD=1.
if [[ "${TRANSIT_SKIP_BUILD:-}" != "1" ]]; then
  echo "==> building librequest-ui.so ..."
  cd "$project_root"
  CGO_ENABLED=1 go build \
    -trimpath \
    -buildmode=c-shared \
    -o "$so_path" \
    "$example_dir/cmd"
  echo "==> build OK: $so_path"
else
  if [[ ! -f "$so_path" ]]; then
    echo "ERROR: TRANSIT_SKIP_BUILD=1 but $so_path not found" >&2
    exit 1
  fi
  echo "==> reusing $so_path (TRANSIT_SKIP_BUILD=1)"
fi

# Build and start the test backend.
echo "==> building testserver ..."
testserver_bin="$script_dir/.bin/testserver"
mkdir -p "$script_dir/.bin"
cd "$project_root"
go build -o "$testserver_bin" "$example_dir/e2e/testserver"

TESTSERVER_ADDR="$testserver_addr" "$testserver_bin" &
testserver_pid=$!
trap 'kill "$testserver_pid" 2>/dev/null || true; kill "$envoy_pid" 2>/dev/null || true' EXIT

# Wait for testserver to be ready.
for _ in {1..25}; do
  if curl -fsS "http://$testserver_addr/health" >/dev/null 2>&1; then break; fi
  sleep 0.2
done
echo "==> testserver ready (pid=$testserver_pid)"

# Start Envoy in memory mode (no Postgres needed).
REQUI_MODE=memory \
REQUI_ADDR=127.0.0.1:6062 \
GODEBUG=cgocheck=0 \
ENVOY_DYNAMIC_MODULES_SEARCH_PATH="$example_dir" \
  "$envoy_bin" -c "$script_dir/envoy-local.yaml" --log-level warning &
envoy_pid=$!

# Wait for Envoy admin to be ready (up to 10 s).
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

# Wait for the UI HTTP server to be ready (access logger starts the server
# in its first OnLog call; send one warm-up request to trigger it).
curl -fsS "$proxy_url/health" >/dev/null 2>&1 || true
sleep 0.5
for _ in {1..20}; do
  if curl -fsS "$ui_url/" >/dev/null 2>&1; then break; fi
  sleep 0.3
done
echo "==> request-ui HTTP server ready"

# Run the Node.js tests.
PROXY_URL="$proxy_url" UI_URL="$ui_url" \
  node --test "$script_dir/requi.test.mjs"
