# 3. Pin the fork as a submodule

Date: 2026-09-14

## Status

Accepted

## Context

The build must resolve PocketBase to the Ilavrita fork, never to upstream.

The obvious approach fails. The fork's `go.mod` still declares the upstream
module path, so

```
replace github.com/pocketbase/pocketbase => github.com/Ilavrita/pocketbase v0.0.0-...
```

is rejected by the Go toolchain: the replacement module must declare the path it
is required as. The fork also publishes no tags, so there is no version to
require.

Rewriting the module path inside the fork would work, but it makes every future
merge from upstream conflict on import paths across the entire tree.

## Decision

Pin the fork as a git submodule at `third_party/pocketbase` and replace by
directory:

```
require github.com/pocketbase/pocketbase v0.0.0
replace github.com/pocketbase/pocketbase => ./third_party/pocketbase
```

A directory replacement accepts the declared upstream path, so the fork compiles
unmodified and merges from upstream stay clean.

`scripts/verify-fork.sh` asserts in CI that the submodule points at
`Ilavrita/pocketbase` and that the module still resolves to it.

## Consequences

Building requires `git submodule update --init --recursive`; a plain clone does
not compile. `make bootstrap` and every CI checkout handle this, and the
requirement is stated in the README and CONTRIBUTING.

The fork's history and licence file stay intact and auditable, which copying the
source into this tree would not achieve.

If the fork is ever re-declared as `github.com/Ilavrita/pocketbase` and tagged,
this decision should be revisited in favour of a plain module requirement.
