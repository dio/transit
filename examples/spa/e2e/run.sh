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
#   - Node >= 24 (for @lightpanda/browser + playwright-core)
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
spa_dir="$(cd "$script_dir/.." && pwd)"
project_root="$(git rev-parse --show-toplevel)"

envoy_bin="${ENVOY_BIN:-$project_root/.bin/envoy}"
admin_url="${SPA_ENVOY_ADMIN_URL:-http://127.0.0.1:9901}"
so_path="$spa_dir/libspa.so"

if [[ ! -x "$envoy_bin" ]]; then
  echo "ERROR: Envoy not found at $envoy_bin (run: make download-envoy)" >&2
  exit 1
fi

# Build the .so unless TRANSIT_SKIP_BUILD=1.
if [[ "${TRANSIT_SKIP_BUILD:-}" != "1" ]]; then
  echo "==> building libspa.so ..."
  CGO_ENABLED=1 go build \
    -trimpath \
    -buildmode=c-shared \
    -o "$so_path" \
    "$spa_dir/cmd"
  echo "==> build OK: $so_path"
else
  if [[ ! -f "$so_path" ]]; then
    echo "ERROR: TRANSIT_SKIP_BUILD=1 but $so_path not found" >&2
    exit 1
  fi
  echo "==> reusing $so_path (TRANSIT_SKIP_BUILD=1)"
fi

# Install e2e npm deps.
npm ci --prefix "$script_dir" --silent

# Start Envoy.
GODEBUG=cgocheck=0 \
ENVOY_DYNAMIC_MODULES_SEARCH_PATH="$spa_dir" \
  "$envoy_bin" -c "$spa_dir/envoy.yaml" --log-level warning &
envoy_pid=$!
trap 'kill "$envoy_pid" 2>/dev/null || true' EXIT

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
node --test "$script_dir/spa.test.mjs"
