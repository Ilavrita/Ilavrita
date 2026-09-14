#!/usr/bin/env bash
# Prepare a working tree for development.
#
# Ilavrita compiles against the Ilavrita PocketBase fork pinned at
# third_party/pocketbase. Without the submodule the Go build cannot resolve the
# replace directive in go.mod.
set -euo pipefail

cd "$(dirname "$0")/.."

echo "==> Fetching the pinned PocketBase fork"
git submodule update --init --recursive

echo "==> Downloading Go dependencies"
go mod download

if command -v pnpm >/dev/null 2>&1; then
  echo "==> Installing workspace dependencies"
  pnpm install
else
  echo "==> Skipping workspace dependencies (pnpm is not installed)"
fi

echo "==> Ready. Run 'make run' to start the server."
