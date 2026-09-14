#!/usr/bin/env bash
# Confirm the build resolves PocketBase to the Ilavrita fork.
#
# Ilavrita must never compile upstream PocketBase. The guarantee rests on a
# submodule plus a replace directive, and both are easy to lose in a merge, so
# this check runs in CI.
set -euo pipefail

cd "$(dirname "$0")/.."

readonly MODULE="github.com/pocketbase/pocketbase"
readonly FORK_PATH="third_party/pocketbase"
readonly FORK_REMOTE="Ilavrita/pocketbase"

fail() {
  echo "verify-fork: $1" >&2
  exit 1
}

if [ ! -f "$FORK_PATH/go.mod" ]; then
  fail "$FORK_PATH is empty; run 'git submodule update --init --recursive'"
fi

submodule_url=$(git config --file .gitmodules --get "submodule.$FORK_PATH.url")
case "$submodule_url" in
  *"$FORK_REMOTE"*) ;;
  *) fail "submodule points at '$submodule_url', expected $FORK_REMOTE" ;;
esac

resolved=$(go list -m "$MODULE")
case "$resolved" in
  *"=> ./$FORK_PATH"*) ;;
  *) fail "module resolves to '$resolved', expected a replacement with ./$FORK_PATH" ;;
esac

echo "verify-fork: building against $FORK_REMOTE at $(git -C "$FORK_PATH" rev-parse --short HEAD)"
