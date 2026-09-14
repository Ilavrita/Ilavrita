# Licence compliance

Ilavrita is distributed under the [AGPL-3.0](../LICENSE) and is scanned by
[FOSSA](https://app.fossa.com/projects/git%2Bgithub.com%2FIlavrita%2FIlavrita)
on every push and pull request.

This page records what the scanner reports and what we concluded, so that a
flagged dependency is either fixed or explained — never silently ignored.

It is an engineering determination, not legal advice.

## How the scan is scoped

[`.fossa.yml`](../.fossa.yml) analyses two manifests: `go.mod` and
`pnpm-lock.yaml`. The vendored PocketBase fork carries its own `go.mod` and an
admin-UI `package.json`; scanning those separately would double-count
dependencies our own `go.mod` already resolves through the replace directive,
and would add upstream UI build tooling that never reaches a release artefact.

## Outstanding findings

### Licence findings — reviewed, no obligation

FOSSA flags copyleft and restrictive licences on four Go modules. All four
**declare permissive licences**; the flags come from licence *texts* bundled
inside each repository, not from the licence under which the module is granted.

| Module | Declared licence | What FOSSA matched |
| --- | --- | --- |
| `modernc.org/libc` | BSD-3-Clause | APSL headers in `*_darwin_*.go`, and GPL text in `testdata/.../crlibm/COPYING` |
| `modernc.org/sqlite` | BSD-3-Clause | Bundled `LICENSE-SQLITE` and `LICENSE-SQLITE_VEC` notices |
| `golang.org/x/text` | BSD-3-Clause | CC-BY-SA notices in bundled Unicode data files |
| `golang.org/x/crypto` | BSD-3-Clause | An OpenSSL/SSLeay reference; no such licence text exists in the module |

Evidence for `modernc.org/libc`, which accounts for most of the findings: its
`LICENSE-3RD-PARTY.md` lists exactly four components — Go (BSD-3-Clause), musl
libc (MIT), go-netdb and NixOS/nixpkgs — and states that "the main project is
licensed under the BSD-3 License". The file mentions neither GPL nor APSL.

The GPL and APSL text that FOSSA finds lives in two places:

- `testdata/` — test fixtures, never compiled into any binary;
- `*_darwin_*.go` — constants transpiled from macOS system headers, which carry
  Apple's notices.

Ilavrita's container images are built for `linux/amd64` and `linux/arm64`, so Go
build constraints exclude every `*_darwin_*` file from the released image. The
macOS binaries published by GoReleaser do compile those files, and the notices
travel with them; they are reproduced in the archive through
[`THIRD_PARTY_NOTICES.md`](../THIRD_PARTY_NOTICES.md).

**Conclusion:** every dependency's effective grant is permissive and compatible
with AGPL-3.0. No source-disclosure obligation beyond the AGPL arises from them.

### Security findings

| Advisory | Package | Severity | State |
| --- | --- | --- | --- |
| [CVE-2023-36308](https://nvd.nist.gov/vuln/detail/CVE-2023-36308) | `github.com/disintegration/imaging@v1.6.2` | Medium | Open, no fixed version |

A crafted TIFF file can crash the decoder. The dependency reaches us through
PocketBase, which uses it for thumbnail generation, and upstream has published no
fixed release.

Ilavrita does not expose image processing through `/fhir/R4`, so no Ilavrita API
route reaches the decoder today. That will change when Binary and
DocumentReference payloads are implemented (FR-032), and the risk must be
reassessed then — untrusted clinical documents are exactly the input this
advisory is about.

Tracked as a release gate for any build that accepts uploaded images.

## Re-running the scan

```bash
export FOSSA_API_KEY=...   # a push-only token is enough for CI
fossa analyze
fossa test
```

CI runs the same two commands from
[`.github/workflows/fossa.yml`](../.github/workflows/fossa.yml) using the
`FOSSA_API_KEY` repository secret.

## When a new finding appears

1. Establish the module's **declared** licence before reacting to a file match.
2. Decide whether the flagged code is compiled into a released artefact.
3. Either remove the dependency, or record the determination in this file.

A finding that is neither fixed nor written down here is not resolved.
