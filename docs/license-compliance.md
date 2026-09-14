# Licence compliance

Ilavrita is distributed under the [AGPL-3.0](../LICENSE) and is scanned by the
[FOSSA GitHub App](https://app.fossa.com/projects/git%2Bgithub.com%2FIlavrita%2FIlavrita)
on every push and pull request.

This page records what the scanner reports and what we concluded, so a flagged
dependency is either fixed or explained — never silently ignored.

It is an engineering determination, not legal advice.

## Current state

The FOSSA checks on pull requests are **red**: 27 licence issues and 1
vulnerability. Of those 28, one is a genuine finding and 27 are not. None can be
cleared from this repository — see [clearing them](#clearing-them).

| Group | Count | Verdict |
| --- | --- | --- |
| Ilavrita's own AGPL-3.0 licence | 2 | Not a finding — this is the project's chosen licence |
| `modernc.org/libc@v1.74.4` | 17 | Reviewed — module is BSD-3-Clause |
| `golang.org/x/text@v0.42.0` | 4 | Reviewed — module is BSD-3-Clause |
| `modernc.org/sqlite@v1.57.0` | 3 | Reviewed — module is BSD-3-Clause |
| `golang.org/x/crypto@v0.57.0` | 1 | Reviewed — module is BSD-3-Clause |
| `github.com/disintegration/imaging@v1.6.2` | 1 | **Genuine.** CVE-2023-36308, no fixed version |

### The two self-flags

FOSSA flags `AGPL-3.0-only` and `AGPL-3.0-or-later` **on Ilavrita itself**. The
project is deliberately AGPL-3.0; this is the policy reporting our own licence
back to us, not a dependency risk.

### The twenty-five dependency flags

Four Go modules are flagged for copyleft and restrictive licences. All four
**declare permissive licences**. The flags come from licence *texts* bundled
inside each repository, not from the licence under which the module is granted.

| Module | Declared licence | What FOSSA matched |
| --- | --- | --- |
| `modernc.org/libc` | BSD-3-Clause | APSL notices in `*_darwin_*.go`; GPL text in `testdata/.../crlibm/COPYING` |
| `modernc.org/sqlite` | BSD-3-Clause | Bundled `LICENSE-SQLITE` and `LICENSE-SQLITE_VEC` |
| `golang.org/x/text` | BSD-3-Clause | CC-BY-SA notices in bundled Unicode data files |
| `golang.org/x/crypto` | BSD-3-Clause | An OpenSSL/SSLeay reference; no such licence text exists in the module |

Evidence for `modernc.org/libc`, which accounts for most of them: its
`LICENSE-3RD-PARTY.md` lists exactly four components — Go (BSD-3-Clause), musl
libc (MIT), go-netdb and NixOS/nixpkgs — and states that "the main project is
licensed under the BSD-3 License". The file mentions neither GPL nor APSL.

The GPL and APSL text FOSSA finds lives in two places:

- `testdata/` — test fixtures, never compiled into any binary;
- `*_darwin_*.go` — constants transpiled from macOS system headers, carrying
  Apple's notices.

Container images are built for `linux/amd64` and `linux/arm64`, so Go build
constraints exclude every `*_darwin_*` file from the released image. The macOS
binaries published by GoReleaser do compile those files, and their notices travel
with them via [`THIRD_PARTY_NOTICES.md`](../THIRD_PARTY_NOTICES.md).

**Conclusion:** every dependency's effective grant is permissive and compatible
with AGPL-3.0. No source-disclosure obligation beyond the AGPL arises from them.

### The one genuine finding

| Advisory | Package | Severity | State |
| --- | --- | --- | --- |
| [CVE-2023-36308](https://nvd.nist.gov/vuln/detail/CVE-2023-36308) | `github.com/disintegration/imaging@v1.6.2` | Medium | Open, no fixed version |

A crafted TIFF file can crash the decoder. The dependency reaches us through
PocketBase, which uses it for thumbnail generation, and upstream has published no
fixed release.

No Ilavrita route reaches the decoder today, because `/fhir/R4` exposes no image
handling. That changes when Binary and DocumentReference payloads are implemented
(FR-032) — untrusted clinical documents are exactly the input this advisory is
about — so it is a **release gate for any build that accepts uploaded images**.

## Clearing them

FOSSA's REST API is read-only for issues. Verified 2026-09-14: every
issue-resolution endpoint returns `404`, and policy writes return
`403 User not permissioned`. The work below has to be done in the FOSSA
dashboard.

1. **The two AGPL self-flags** — resolve as "this is the project's own declared
   licence".
2. **The twenty-five dependency flags** — set a licence conclusion of
   `BSD-3-Clause` on `modernc.org/libc`, `modernc.org/sqlite`, `golang.org/x/text`
   and `golang.org/x/crypto`, citing the evidence above.
3. **CVE-2023-36308** — leave open. It is real, and it should stay visible until
   upstream publishes a fix or the dependency is replaced.

### Why the policy is not being changed

The project is assigned FOSSA's **Standard Bundle Distribution** policy, which is
the right one: Ilavrita ships binaries and container images rather than running
as a hosted service, so distribution-mode obligations apply.

Loosening the policy to permit copyleft would clear these checks and would be the
wrong fix twice over. It would mask a genuinely GPL-licensed dependency arriving
later, and Ilavrita intends to offer a commercial licence alongside the AGPL
(PRD risk R-006) — a copyleft dependency we do not own would block exactly that.

## Running a scan locally

```bash
export FOSSA_API_KEY=...
fossa analyze
fossa test
```

[`.fossa.yml`](../.fossa.yml) scopes this to the two manifests Ilavrita ships. The
CLI reports into a different project from the GitHub App, so prefer the App's
results as the source of truth; CI does not run the CLI, to avoid two projects
disagreeing about the same code.

## When a new finding appears

1. Establish the module's **declared** licence before reacting to a file match.
2. Decide whether the flagged code is compiled into a released artefact.
3. Either remove the dependency, or record the determination here.

A finding that is neither fixed nor written down here is not resolved.
