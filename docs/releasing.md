# Releasing

## Branches

| Branch | Purpose |
| --- | --- |
| `main` | Release branch. Every tag is cut from here. Protected. |
| `dev` | Integration branch. Pull requests target this. |
| `feat/*`, `fix/*` | Short-lived work, merged into `dev`. |

Work flows `feature -> dev -> main -> tag`. Nothing is released from anywhere
other than `main`, and the release workflow enforces it: a `v*` tag whose commit
is not an ancestor of `origin/main` fails before anything is published.

## Versioning

[Semantic Versioning](https://semver.org). While the major version is `0`, any
release may change behaviour — `0.x` is not a stability promise.

Release notes are derived from commit subjects, so the one-line Conventional
Commit format described in [CONTRIBUTING.md](../CONTRIBUTING.md) is what makes a
readable changelog possible.

## Cutting a release

1. Confirm CI is green on `dev`.
2. Update [CHANGELOG.md](../CHANGELOG.md): move `Unreleased` into a new version
   section with the date, and record known limitations honestly.
3. Merge `dev` into `main`.
4. Tag and push:

   ```bash
   git checkout main && git pull
   git tag -a v0.1.0 -m "v0.1.0"
   git push origin v0.1.0
   ```

5. The release workflow verifies the tag is on `main`, builds binaries for
   Linux, macOS and Windows on amd64 and arm64, generates checksums and an SBOM,
   publishes the GitHub release, and pushes a multi-architecture image to
   `ghcr.io/ilavrita/ilavrita` with build provenance attached.

## Before a stable release

A `0.x` scaffold release needs only the steps above. A release claiming stable
FHIR behaviour additionally requires the evidence the product requirements call
for: conformance and integration results, tenant-isolation tests, an upgrade test
from the previous supported version, a restore test, a security review, migration
and rollback notes, and a documented list of known limitations.

Do not advertise a capability in the CapabilityStatement that the release test
matrix does not cover.
