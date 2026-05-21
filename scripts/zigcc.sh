#!/usr/bin/env bash
# scripts/zigcc.sh
# Wraps "zig cc" for use as CGO_CC.
#
# Strips flags incompatible with zig cc / lld:
#   --unresolved-symbols=*               not supported by lld
#   -Wl,--compress-debug-sections=*     Linux lld only; macOS ld rejects it
#
# For macOS targets (TARGET=*-macos*) -target is omitted so zig cc uses the
# native macOS toolchain and resolves system libraries automatically.
# For Linux cross-compilation TARGET sets the target triple explicitly.
#
# ZIG env var overrides the zig binary path.
# TARGET env var sets the cross-compile target triple (default: native).
set -euo pipefail

ZIG="${ZIG:-$(dirname "$(realpath "${BASH_SOURCE[0]}")")/../.bin/zig-dist/zig}"
TARGET="${TARGET:-}"

args=()
for arg in "$@"; do
    [[ "$arg" == "--unresolved-symbols="* ]] && continue
    [[ "$arg" == "-Wl,--compress-debug-sections="* ]] && continue
    args+=("$arg")
done

if [[ "$TARGET" == *-macos* || -z "$TARGET" ]]; then
    exec "$ZIG" cc "${args[@]}"
else
    exec "$ZIG" cc -target "$TARGET" "${args[@]}"
fi
