# Supply chain

Every Ilavrita release is signed and attested so that you can check an artefact
came from this repository, at a known commit, without trusting the download.

Nothing here requires trusting Ilavrita's word for it — each command below
verifies against a public transparency log.

## What each release publishes

| Artefact | Where | Integrity evidence |
| --- | --- | --- |
| Platform binaries | GitHub release | `checksums.txt`, SBOM per archive |
| Container image | `ghcr.io/ilavrita/ilavrita` | Cosign signature, build provenance, SBOM, all bound to the digest |
| `@ilavrita/sdk` | GitHub Packages | Published from CI only; see the caveat below |
| `@ilavrita/tsconfig` | GitHub Packages | Published from CI only; see the caveat below |

## Verifying a binary

```bash
sha256sum --check --ignore-missing checksums.txt
```

Check the archive you downloaded, not only the ones you did not.

## Verifying the container image

Resolve the digest first, and verify against it. A tag can be repointed at a
different image; a digest cannot.

```bash
DIGEST=$(crane digest ghcr.io/ilavrita/ilavrita:0.1.0)
IMAGE="ghcr.io/ilavrita/ilavrita@${DIGEST}"
```

**Signature** — keyless, so the identity that signed it is checked rather than a
key you were handed:

```bash
cosign verify "$IMAGE" \
  --certificate-identity-regexp '^https://github\.com/Ilavrita/Ilavrita/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

**Build provenance** — which workflow, at which commit, produced it:

```bash
gh attestation verify "oci://${IMAGE}" --repo Ilavrita/Ilavrita
```

**Contents** — the SBOM is attested to the same digest:

```bash
cosign download attestation "$IMAGE" --predicate-type https://spdx.dev/Document
```

If the signature verifies but the identity regex does not match a
`refs/tags/v*` workflow run in `Ilavrita/Ilavrita`, treat the image as untrusted:
that is the case the regex exists to catch.

## Installing and verifying an npm package

The packages are published to **GitHub Packages**, not npmjs.com. Installing
requires authentication even though the packages are public, so point the scope
at the registry and authenticate with a GitHub token:

```
@ilavrita:registry=https://npm.pkg.github.com
//npm.pkg.github.com/:_authToken=${GITHUB_TOKEN}
```

```bash
npm install @ilavrita/sdk
```

> [!NOTE]
> **These packages carry no provenance attestation.** Provenance is an npmjs.com
> registry feature — attestations are stored at `versions[].dist.attestations`
> there and checked with `npm audit signatures`. GitHub Packages stores no
> equivalent, so that command reports nothing useful for these packages.
>
> What you can rely on is that they are published only by
> [`publish-preview.yml`](../.github/workflows/publish-preview.yml) and
> [`release.yml`](../.github/workflows/release.yml) using the workflow's own
> token. There is no long-lived npm credential that could publish them from
> elsewhere.
>
> The container image is unaffected: it keeps its Cosign signature, build
> provenance and SBOM, because those live in the OCI registry rather than npm.

## What is pinned

- **Base images** are pinned by digest in
  [`build/docker/Dockerfile`](../build/docker/Dockerfile). A tag is mutable, so
  tag-only pinning means the image you audited and the image you shipped can
  differ.
- **The PocketBase fork** is a git submodule pinned to a commit, and
  [`scripts/verify-fork.sh`](../scripts/verify-fork.sh) fails the build if it ever
  resolves to something other than `Ilavrita/pocketbase`.
- **Package versions** come from the release tag through
  [`scripts/set-package-versions.sh`](../scripts/set-package-versions.sh), so a
  published package always traces to the tag that produced it.

## Reporting a problem

If verification fails for an artefact you obtained from an official channel,
treat it as a security issue and follow [SECURITY.md](../SECURITY.md) rather than
opening a public issue.
