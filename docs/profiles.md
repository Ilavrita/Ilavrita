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
