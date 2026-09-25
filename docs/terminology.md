# Terminology

Ilavrita ships a terminology **importer**, never terminology **content**. SNOMED CT and
LOINC releases are licensed to the organisation that obtains them, so no release file is in
this repository and none is in a release artefact.

An install that holds a licence points Ilavrita at the release it obtained. An install that
holds none still runs, and says so rather than pretending.

## What this repository contains

| | In the repository | Obtained by the deployment |
| --- | --- | --- |
| The importer | Yes | — |
| R4 base value sets and code systems | Yes, embedded | — |
| SNOMED CT | **No** | Under its own NRCeS affiliate licence |
| LOINC | **No** | Under its own Regenstrief licence |

Both licences are free. Free is not the same as redistributable: they are granted to a
named affiliate, which is why an operator obtains their own rather than receiving ours.

- **SNOMED CT in India** — India is a member country, so NRCeS grants an affiliate licence
  at no upfront or recurring cost, commercial use included. Request it from
  [NRCeS](https://www.nrces.in/standards/snomed-ct).
- **LOINC** — free worldwide under the
  [LOINC licence](https://loinc.org/license/), which requires registration and attribution.

Ilavrita is AGPL-3.0. Shipping licensed terminology inside it would place content under a
licence its owner did not grant, which is why the split above is structural and not a
preference.

## Loading a release

Unpack the release and point the server at the directory:

```bash
export ILAVRITA_TERMINOLOGY_DIR=/srv/ilavrita/terminology
```

Every `.json` file beneath it is read. A `CodeSystem` or a `ValueSet` carrying a `url` is
loaded; anything else — a manifest, examples, notes — is skipped rather than refused,
because a release ships those alongside the definitions.

The walk is confined by `os.Root`, so a symlink in an unpacked release cannot read a file
outside the directory named. Symlinks are skipped rather than followed: one pointing
outside would otherwise fail the whole load, and a release nobody can load leaves
validation silently deciding nothing.

A supplied definition wins over the embedded one of the same URL. An install that obtained
a newer release means to use it.

## What validation does in each state

A required binding is the only binding that is a rule. R4's extensible, preferred and
example bindings say a code *should* come from a set, and refusing one would refuse
resources the specification allows.

| Install state | `$validate` on a required binding |
| --- | --- |
| No release loaded, set not embedded | **Unchecked** — the set is not resolved, so no code is judged against it |
| Release loaded, code present in the set | **Passes** |
| Release loaded, code absent from the set | **Fails** |

The first row is the one that matters. A server that cannot decide a code must not answer
as though it did — the same rule that keeps the CapabilityStatement from advertising an
interaction it does not serve. `Terminology.Admits` returns whether the set was resolved
precisely so an unresolved set decides nothing.

This means **two installs can answer `$validate` differently on the same resource**, and
that is correct: one of them knows something the other does not. What an install resolved
is readable through `Terminology.Sets()`.

## Verifying a load

```bash
go test ./packages/conformance/ -run Supplied
```

The tests use a fixture rather than a real release, since no release may be committed here.
They assert the shape: a set resolves, subdirectories are walked, non-terminology files do
not stop the load, and embedded sets survive a supplied release.

## Why this was removed once, and what changed

An earlier build carried a terminology importer and it was deleted deliberately. The
reasoning is in [known limitations](known-limitations.md): exactly one of R4's required
bindings names a SNOMED or LOINC value set, so holding one made validation no stricter,
while costing a licence question per deployment and a release file to keep current.

That reasoning held for base R4 and does not hold for an implementation guide. India's
ABDM guide (`ndhm.in`) binds far more than one element, so an install validating against it
decides much less without a release. The cost is unchanged; the value is not.
