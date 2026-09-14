# Third-party notices

Ilavrita is distributed under the AGPL-3.0 (see [`LICENSE`](LICENSE)) and includes
third-party software under the licences recorded here.

## Vendored source

| Component | Origin | Licence | Notice |
| --- | --- | --- | --- |
| PocketBase (Ilavrita fork) | [`Ilavrita/pocketbase`](https://github.com/Ilavrita/pocketbase), vendored at `third_party/pocketbase` | MIT | [`LICENSES/PocketBase-MIT.txt`](LICENSES/PocketBase-MIT.txt) |

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
