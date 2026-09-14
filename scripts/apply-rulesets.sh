#!/usr/bin/env bash
# Apply the branch and tag rulesets in .github/rulesets to the repository.
#
# Rulesets are kept in version control so a change to who can push where is
# reviewed like any other change, rather than clicked into a settings page and
# forgotten. Running this is idempotent: an existing ruleset of the same name is
# updated in place.
set -euo pipefail

cd "$(dirname "$0")/.."

readonly REPO="${ILAVRITA_REPO:-Ilavrita/Ilavrita}"

existing_id_for() {
  gh api "repos/$REPO/rulesets" --jq \
    ".[] | select(.name == \"$1\") | .id" 2>/dev/null | head -1
}

for definition in .github/rulesets/*.json; do
  name=$(python3 -c "import json,sys; print(json.load(open(sys.argv[1]))['name'])" "$definition")
  id=$(existing_id_for "$name")

  if [ -n "$id" ]; then
    gh api -X PUT "repos/$REPO/rulesets/$id" --input "$definition" >/dev/null
    echo "updated  $name"
  else
    gh api -X POST "repos/$REPO/rulesets" --input "$definition" >/dev/null
    echo "created  $name"
  fi
done
