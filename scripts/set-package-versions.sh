#!/usr/bin/env bash
# Set the version of every publishable npm package, and resolve workspace
# references to that version.
#
# Package versions follow the release tag rather than being maintained by hand,
# so a published package can always be traced back to the tag that produced it.
#
# The workspace: protocol is resolved here rather than left to `pnpm publish`,
# because publishing goes through `npm publish --provenance` and npm would ship
# "workspace:*" verbatim, which no consumer can resolve.
set -euo pipefail

cd "$(dirname "$0")/.."

if [ $# -ne 1 ]; then
  echo "usage: $0 <version>   # e.g. 0.1.0, or a v-prefixed tag" >&2
  exit 2
fi

version="${1#v}"

# This rewrite is one-way: once workspace: references are resolved they cannot be
# recovered from the manifest. Run it on an ephemeral CI checkout, never on a
# working tree you intend to commit from.
if [ -z "${CI:-}" ] && [ -z "${ILAVRITA_ALLOW_LOCAL_VERSION_WRITE:-}" ]; then
  echo "refusing to rewrite manifests outside CI" >&2
  echo "set ILAVRITA_ALLOW_LOCAL_VERSION_WRITE=1 if you really mean to" >&2
  exit 1
fi

if ! printf '%s' "$version" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
  echo "refusing to set a version that is not semver: $version" >&2
  exit 1
fi

for manifest in packages/sdk-typescript/package.json packages/tsconfig/package.json; do
  node -e '
    const fs = require("fs");
    const [manifest, version] = process.argv.slice(1);
    const pkg = JSON.parse(fs.readFileSync(manifest, "utf8"));
    pkg.version = version;

    for (const group of ["dependencies", "devDependencies", "peerDependencies"]) {
      for (const [name, range] of Object.entries(pkg[group] ?? {})) {
        if (typeof range === "string" && range.startsWith("workspace:")) {
          pkg[group][name] = version;
        }
      }
    }

    fs.writeFileSync(manifest, JSON.stringify(pkg, null, 2) + "\n");
    console.log(`${pkg.name} -> ${version}`);
  ' "$manifest" "$version"
done
