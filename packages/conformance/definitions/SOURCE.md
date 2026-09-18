# Where these came from

The three files beside this one are the FHIR R4 specification's own definition
bundles, gzipped and otherwise unmodified. They are shipped as published rather
than filtered, so anybody can re-download them and compare.

| File | Source | SHA-256 of the uncompressed file |
| --- | --- | --- |
| `profiles-resources.json.gz` | <https://hl7.org/fhir/R4/profiles-resources.json> | `d062516a420265da6d248e1d2b5d2a4aa9709c3c637807fe48e278054dffa114` |
| `profiles-types.json.gz` | <https://hl7.org/fhir/R4/profiles-types.json> | `13d3b9c75a23dd575b6c9cffa9fb5efce3871a4cc9bd8c92671ce1a50c19c064` |
| `valuesets.json.gz` | <https://hl7.org/fhir/R4/valuesets.json> | `8d8fd0894624163296aa84e93c47cbead9b805fd83ec22ac71d45b7e25a86758` |

- **FHIR release:** 4.0.1
- **Upstream `Last-Modified`:** Fri, 21 Mar 2025 18:57:07 GMT
- **Retrieved:** 2026-09-18

## Licence

The FHIR specification, these definitions included, is published by Health Level
Seven International under Creative Commons "No Rights Reserved"
([CC0](https://hl7.org/fhir/R4/license.html)).

FHIR® and HL7® are registered trademarks of Health Level Seven International.
Ilavrita is not produced, endorsed or certified by HL7.

## Refreshing them

Re-download both files, check the digests recorded above have changed for the
reason you expect, gzip them in place, and update this file. The seeder digests
what it embeds, so an install picks up the change on its next start and does
nothing on every start after that.

```bash
for f in profiles-resources profiles-types valuesets; do
  curl -sSo "$f.json" "https://hl7.org/fhir/R4/$f.json"
done
shasum -a 256 profiles-resources.json profiles-types.json valuesets.json
gzip -9 profiles-resources.json profiles-types.json valuesets.json
```
