# Branch protection

Protection is defined as JSON in [`.github/rulesets`](../.github/rulesets) and
applied with [`scripts/apply-rulesets.sh`](../scripts/apply-rulesets.sh). Keeping
it in version control means a change to who can push where gets reviewed like any
other change, instead of being clicked into a settings page and forgotten.

```bash
./scripts/apply-rulesets.sh
```

The script is idempotent: an existing ruleset of the same name is updated in
place.

## What each ruleset does

### `main` — the release branch

| Rule | Effect |
| --- | --- |
| Pull request required | 1 approval, code owner review, stale reviews dismissed on push, last push must be approved, review threads resolved |
| Required status checks | Go, Lint, Workspace, Container, and both CodeQL analyses, against an up-to-date branch |
| Linear history | No merge bubbles; history stays readable and bisectable |
| No force push | The release branch cannot be rewritten |
| No deletion | The release branch cannot be removed |

Every release tag is cut from `main`, and
[`.github/workflows/release.yml`](../.github/workflows/release.yml) refuses a tag
whose commit is not an ancestor of `origin/main`. Protection and the release
pipeline enforce the same rule from two directions.

### `dev` — the integration branch

Required status checks, no force push, no deletion. Pull requests are not
required, so integration work and Dependabot merges are not slowed by a review
gate that a single maintainer would only be granting to themselves.

### `release tags` — `v*`

Tags matching `v*` cannot be created by non-admins, updated, or deleted. A
published version identifier must keep pointing at the commit it was built from,
or release provenance means nothing.

## Bypass

Repository admins can bypass all three rulesets. This is deliberate: Ilavrita has
a single maintainer today, and a gate that locks out the only person able to
respond to an incident is a liability rather than a control.

As more maintainers join, tighten this by removing the bypass from `main` first —
it is the ruleset that protects released artefacts.

## Changing protection

Edit the JSON, open a pull request, and run the script once it merges. Do not
edit rulesets in the GitHub UI; the next run of the script would overwrite the
change and the reasoning behind it would be lost.
