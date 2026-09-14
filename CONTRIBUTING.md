# Contributing to Ilavrita

Thank you for helping build open healthcare infrastructure. This document covers
how to get a working tree, what we expect from a change, and how a change gets
merged.

Everyone taking part agrees to the [Code of Conduct](CODE_OF_CONDUCT.md).

## Before you start

Open an issue before writing a large change. Ilavrita has a deliberately narrow
v0.1 scope (see [ROADMAP.md](ROADMAP.md)), and a pull request that expands it
will be hard to accept no matter how good the code is.

Small fixes — a typo, a broken link, a clearly wrong error message — need no
issue. Just send the pull request.

## Setting up

Requires Go 1.27+, and Node 22+ with pnpm 10+ for the workspace tooling.

```bash
git clone https://github.com/Ilavrita/Ilavrita.git
cd Ilavrita
make bootstrap
```

Ilavrita compiles against the [Ilavrita PocketBase fork](https://github.com/Ilavrita/pocketbase),
pinned as a submodule at `third_party/pocketbase`. Without it the build cannot
resolve the `replace` directive in `go.mod`, so a clone without submodules will
not compile. `make bootstrap` handles this.

```bash
make build   # compile the server
make run     # run it
make test    # Go tests
make lint    # golangci-lint
pnpm build   # everything, through Turborepo
```

## Architectural rules

These are enforced in review and, where possible, by `golangci-lint`:

1. **The FHIR boundary is ours.** Routes, error shapes, search semantics and
   versioning are defined by Ilavrita. A PocketBase collection name, admin route
   or error format must never be visible through `/fhir/R4`.
2. **Only the storage backend imports PocketBase.** Everything else depends on
   the interfaces in `packages/storage`. This is what keeps a second backend
   possible.
3. **No SQL outside the storage backend.** Higher layers reason in FHIR terms.
4. **Never advertise what is not implemented.** If the CapabilityStatement says a
   resource is supported, there is a test proving it.
5. **Never log resource bodies.** They can contain patient data. Log a request
   id and correlate.

## Code style

Follow the conventions already in the file you are editing. Beyond that:

- Small functions that do one thing, with names that say what that is.
- Prefer a named type over a bare `string` when the value has meaning.
- Comments explain *why*. If a comment restates the code, delete one of them.
- Do not comment out code — delete it; the history keeps it.
- Leave the area cleaner than you found it, without turning a fix into a rewrite.

Run `make fmt` before committing.

## Tests

New behaviour ships with tests. A test should assert one thing, read clearly,
run fast, and pass regardless of what ran before it.

Core FHIR behaviour must be testable without driving a browser.

## Commits

One line. [Conventional Commits](https://www.conventionalcommits.org) format:

```
feat(search): add token parameter indexing
fix(fhir): return not-found rather than deleted for missing resources
docs: correct the backup restore order
```

Types in use: `feat`, `fix`, `perf`, `refactor`, `docs`, `test`, `build`, `ci`,
`chore`. A `!` after the type, as in `feat(fhir)!:`, marks a breaking change.

Keep commits small and atomic — one logical change each. `CHANGELOG.md` and the
release notes are generated from these subjects by
[git-cliff](https://git-cliff.org), so a vague subject line becomes a vague
changelog. Never hand-edit `CHANGELOG.md`; run `make changelog`.

## Pull requests

- Sign off your commits (`git commit -s`) to certify the
  [Developer Certificate of Origin](https://developercertificate.org).
- Fill in the pull request template. The interesting part is *why*.
- Keep the branch rebased on `dev`.
- CI must be green.
- A maintainer reviews and merges. See [GOVERNANCE.md](GOVERNANCE.md).

Ilavrita intends to offer a commercial licence alongside the AGPL. The
contributor agreement that makes that possible is still under legal review, so
until it lands we cannot accept large external contributions to the core. Small
fixes are welcome now, and issues, testing and review always are.

## Security

Do not open a public issue for a vulnerability. Follow [SECURITY.md](SECURITY.md).

## Running CI locally

[`act`](https://github.com/nektos/act) runs the GitHub Actions workflows on your
machine, which is faster than pushing to find out something is broken.

```bash
brew install act          # or see the act README
act -l                    # list jobs
act push -j go            # run one job
```

Settings live in [`.actrc`](.actrc). Two known differences from real CI: the
`Post setup-go` cache step fails under act because `node` is missing from the
container's PATH — harmless, and it appears after every real step has already
run — and the CodeQL job cannot run locally at all.
