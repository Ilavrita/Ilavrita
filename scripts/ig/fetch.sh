#!/usr/bin/env bash
# Fetch an implementation guide and its published examples.
#
# Nothing here is committed: an IG is somebody else's artefact under somebody
# else's terms, so it is obtained at test time rather than vendored. See
# docs/terminology.md for the same rule applied to licensed terminology.
set -euo pipefail

cd "$(dirname "$0")/../.."

readonly BASE="${IG_BASE:-https://nrces.in/ndhm/fhir/r4}"
readonly OUT="${IG_DIR:-.ig/ndhm}"

mkdir -p "$OUT/package" "$OUT/examples"

if [ ! -f "$OUT/package/.index.json" ]; then
  echo "==> Fetching the guide from $BASE"
  curl -fsSL "$BASE/package.tgz" -o "$OUT/package.tgz"
  tar xzf "$OUT/package.tgz" -C "$OUT"
fi

if [ -z "$(ls -A "$OUT/examples" 2>/dev/null)" ]; then
  echo "==> Fetching the examples the guide publishes"
  curl -fsSL "$BASE/all-examples.html" -o "$OUT/all-examples.html"

  # The examples are linked from that page and shipped beside it as JSON; the
  # package itself indexes none of them.
  grep -oE 'href="[A-Za-z]+-[A-Za-z0-9_.-]+\.html"' "$OUT/all-examples.html" \
    | sed 's/href="//; s/\.html"//' \
    | grep -vE '^(StructureDefinition|ValueSet|CodeSystem|ImplementationGuide)-' \
    | sort -u \
    | while read -r name; do
        curl -fsS "$BASE/$name.json" -o "$OUT/examples/$name.json" 2>/dev/null || true
      done
fi

printf '%s profiles, %s examples in %s\n' \
  "$(ls "$OUT/package"/StructureDefinition-*.json 2>/dev/null | wc -l | tr -d ' ')" \
  "$(ls "$OUT/examples"/*.json 2>/dev/null | wc -l | tr -d ' ')" \
  "$OUT"
