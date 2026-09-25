# Implementation guides

Ilavrita checks a resource against the profiles it names in `meta.profile`. A guide is
fetched at test time and never committed: it is somebody else's artefact under somebody
else's terms, the same rule [terminology](terminology.md) follows.

## Loading a guide

```bash
./scripts/ig/fetch.sh                      # defaults to India's ABDM guide
export ILAVRITA_PROFILE_DIR=.ig/ndhm/package
```

`IG_BASE` points the fetcher at another guide. Only a profile shipping a **snapshot** is
read: one shipping a differential alone states its narrowing relative to a base this build
would have to expand, and that expansion is not done here.

## What is checked

| A resource declaring | Result |
| --- | --- |
| No profile | Its base definition only, as before |
| A profile this install holds | Base definition, then the profile's narrowing |
| A profile this install does not hold | **Reported**, not passed |

That last row is the point. A resource claiming conformance to something nobody here can
check is a narrowing nobody checked, and saying so is the difference between a server that
validated and one that was silent.

Applied today: narrowed cardinality — `min` making an element required, `max` making it
single or forbidden — and every constraint the base definition already carried.

**Slicing is not applied.** A slice shares its path with the element it slices, so it is
skipped rather than merged: merging would replace an element's real cardinality with one
branch's. A snapshot names a slice in the element `id` (`Bundle.entry:Claim`), which the
path does not say.

**`mustSupport` is not applied.** It binds the publisher, not the validator: it says a
system that holds the element must send it, not that a resource must carry one.

## The conformance bar

`TestTheGuidesOwnExamplesValidate` holds this build to the examples a guide publishes.
Those examples are what its author says conformant looks like, so anything refused there is
this build disagreeing with them.

```bash
./scripts/ig/fetch.sh && go test ./packages/validate/ -run GuidesOwnExamples -v
```

Against ABDM 6.5.0: **138 examples checked, 0 refused**, which is the same verdict the HL7
validator gives with the guide loaded.

That agreement was not free. An earlier pass refused eleven of them and the reasoning was
wrong: a profile narrowing `0..*` to `0..1` bounds how many entries an array may hold, it
does not turn the array into an object. JSON shape is the base definition's to decide and a
profile never changes it. Checking ourselves against a second implementation is what caught
it — the same reason `scripts/conformance.sh` exists.

No test asserts a number of failures, so a regression shows up as a named example rather
than a count that somebody edits.

## Agreeing with a second validator

```bash
python3 scripts/ig/crosscheck.py
```

Both validators judge the same 138 examples and the disagreement is the finding, reported
by file name. It runs in the nightly conformance workflow.

Ours refusing what HL7's accepts is a bug in ours, and nothing excuses it: that direction
always fails. The other direction is a rule HL7 applies and this build does not, so it is
accepted only against a stated reason, and the run prints how many findings each reason
covered. A finding no reason matches fails the run. Today:

| Findings | Accepted because |
| --- | --- |
| 737 | CI runs the validator offline, so a required binding to an external code system — a MIME type, a currency — cannot be confirmed |
| 33 | Downstream of those: a bundle entry whose only fault is an unconfirmable code matches no slice of its bundle |
| 14 | HL7 applies R4's vital-signs profiles to an Observation whose code is one of theirs. This build applies what `meta.profile` declares and nothing else |
| 9 | This build does not check the context an extension may appear in |

Each was read before it was accepted, not assumed. The 33 were confirmed by validating
every named entry on its own and finding nothing but terminology; the 14 by reading the
codes and the profiles HL7 named. The reasons match on the validator's own wording and on
the profile a finding came from, so a wording change or a slicing finding from anywhere
else reappears as unexplained rather than being absorbed.

Two real gaps are in that table — extension context, and profiles applied by code rather
than declared. Neither is implemented, and the count is how they stay visible.

### The check refuses to pass vacuously

A validator that never loaded the guide accepts everything, which is indistinguishable
from agreeing with us. The service resolves profiles from packages handed to its `/igs`
endpoint and ignores a mounted directory, so getting this wrong is easy — and it was
wrong: the first version of this script mounted a volume, loaded nothing, and reported
agreement on all 138.

So the script proves the guide is applied before it trusts a verdict. It sends a bare
`Patient` against the guide's Patient profile, which makes `identifier` required where the
base leaves it optional, and stops unless the answer names that element. It also stops if
our own test did not run, since a test that never ran refuses nothing and reads as
agreement.

This exists because the eleven-example error above was found by hand. A check run once
catches a bug once; the same check in CI catches the next one.
