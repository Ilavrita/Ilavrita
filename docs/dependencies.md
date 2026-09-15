# Dependency updates

Dependabot opens updates weekly for Go modules, npm packages, GitHub Actions and
the container base images. How they merge depends on what kind of update it is.

## Policy

| Update type | What happens |
| --- | --- |
| Patch, minor | Auto-merge queued; merges when the required checks pass |
| Major | Held for review, labelled `needs:review`, never auto-merged |

[`.github/workflows/dependabot.yml`](../.github/workflows/dependabot.yml)
implements this. Auto-merge is queued with `gh pr merge --auto`, which waits for
the branch's required status checks, so an update that breaks the build never
merges regardless of what the workflow intended.

A major version is the upstream's own signal that something changed in a way it
expects to break callers. Green CI does not disprove that — it only proves the
paths we test still work.

## Reviewing a major update

Green checks are not automatically evidence. **Check that CI actually exercises
the thing being changed**, because a workflow that only runs on tags is not
covered by any pull request check.

```bash
grep -rln "actions/<name>@" .github/workflows/
```

Cross-reference that against which workflows run on `pull_request`. If the
dependency appears only in `release.yml`, no pull request check touches it, and
its green checks say nothing about the change.

Then:

1. Read the upstream release notes for the major version.
2. Run the workflows locally where possible: `make ci-local`.
3. For anything in the release path, treat the next release as the real test and
   say so on the pull request rather than implying it was verified.

## Known gap

`release.yml` runs only on `v*` tags, so nothing it uses is covered by pull
request checks. Today that is `actions/attest-build-provenance`, `goreleaser`,
`cosign` and `syft`. Changes to those are verified by reading release notes and
by the next release actually running — not by a green pull request.
