# Third-party notices

Ilavrita is distributed under the AGPL-3.0 (see [`LICENSE`](LICENSE)) and includes
third-party software under the licences recorded here.

## Vendored source

| Component | Origin | Licence | Notice |
| --- | --- | --- | --- |
| PocketBase (Ilavrita fork) | [`Ilavrita/pocketbase`](https://github.com/Ilavrita/pocketbase), vendored at `third_party/pocketbase` | MIT | [`LICENSES/PocketBase-MIT.txt`](LICENSES/PocketBase-MIT.txt) |
| FHIR R4 base definitions | [hl7.org/fhir/R4](https://hl7.org/fhir/R4/), vendored at `packages/conformance/definitions` | CC0 1.0 | [`packages/conformance/definitions/SOURCE.md`](packages/conformance/definitions/SOURCE.md) |

The FHIR definition bundles are shipped gzipped and otherwise unmodified, with
their upstream digests recorded, so anybody can re-download them and compare.
FHIR® and HL7® are registered trademarks of Health Level Seven International;
Ilavrita is not produced, endorsed or certified by HL7.

The fork is pinned as a git submodule rather than copied into this tree, so its
history, copyright headers and licence file stay intact and auditable.

## Go dependencies

The authoritative list is [`go.mod`](go.mod) together with [`go.sum`](go.sum).
Their licences are those declared by each upstream project.

## Node dependencies

The authoritative list is [`pnpm-lock.yaml`](pnpm-lock.yaml).

## Generating a software bill of materials

A machine-readable SBOM is produced per release and published as a release asset.

```bash
go tool cyclonedx-gomod mod -json -output sbom.json
```

## Reporting an attribution problem

Missing or incorrect attribution is treated as a defect. Open an issue, or write
to <oss@ilavrita.health> if the matter is sensitive.
