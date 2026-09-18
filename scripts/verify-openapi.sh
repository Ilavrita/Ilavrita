#!/usr/bin/env bash
# Check that api/openapi.yaml still describes the server it claims to describe.
#
# A specification that drifts from the implementation is worse than none: callers
# trust it and are wrong. This boots the server and compares every documented
# route's status, content type and response shape against what it actually
# returns.
set -euo pipefail

cd "$(dirname "$0")/.."

readonly PORT="${ILAVRITA_VERIFY_PORT:-8111}"
WORKDIR=$(mktemp -d)

# The server deletes its own WAL and SHM files as it shuts down, so the wait is
# what stops the removal below racing it. set -e would otherwise let a vanished
# file decide this script's exit status even though every check passed.
cleanup() {
  if [ -n "${SERVER_PID:-}" ]; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi

  $RM -rf "$WORKDIR" 2>/dev/null || true
}
RM=$(command -v rm)
trap cleanup EXIT

echo "==> Bundling the description"
# Bundling resolves every $ref, so the comparison below reads plain JSON.
./node_modules/.bin/redocly bundle api/openapi.yaml --dereferenced -o "$WORKDIR/openapi.json" >/dev/null

echo "==> Building the server"
go build -o "$WORKDIR/ilavrita" ./apps/ilavrita

echo "==> Starting it on port $PORT"
"$WORKDIR/ilavrita" serve --http="127.0.0.1:$PORT" --dir="$WORKDIR/data" >"$WORKDIR/log" 2>&1 &
SERVER_PID=$!

for _ in $(seq 1 60); do
  curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break
  sleep 0.5
done

echo "==> Comparing the specification against the live server"
ILAVRITA_VERIFY_PORT="$PORT" python3 scripts/verify_openapi.py "$WORKDIR/openapi.json"
