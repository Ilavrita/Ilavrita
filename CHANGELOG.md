# Changelog

All notable changes to Ilavrita are recorded here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html). While
the major version is `0`, any release may change behaviour.

## [Unreleased]

## [0.0.1] - 2026-09-14

The initial scaffold. This release establishes the repository, the build and the
architectural boundaries. It implements no FHIR interaction.

### Added

- Turborepo monorepo with Go as the primary language.
- Ilavrita PocketBase fork pinned as a submodule at `third_party/pocketbase`,
  wired so that upstream PocketBase is never compiled in.
- `packages/storage`: the persistence interfaces FHIR services depend on, which
  keep a future PostgreSQL backend possible.
- `packages/fhir`: OperationOutcome and CapabilityStatement types for the public
  boundary.
- Package outlines for search, tenancy, authorization, audit, files,
  configuration and observability.
- Server with `GET /healthz`, `GET /version` and `GET /fhir/R4/metadata`. All
  other FHIR routes answer `501 Not Implemented` as an OperationOutcome.
- `@ilavrita/sdk`: TypeScript client reading the CapabilityStatement.
- AGPL-3.0 licensing with PocketBase MIT notices preserved.
- Open-source project setup: contribution, governance, security and support
  documents, issue and pull request templates, and CI.

### Known limitations

No FHIR create, read, update, delete, history, search, or Bundle processing. No
tenancy, authorization or audit enforcement. See
[docs/known-limitations.md](docs/known-limitations.md).

[Unreleased]: https://github.com/Ilavrita/Ilavrita/compare/v0.0.1...HEAD
[0.0.1]: https://github.com/Ilavrita/Ilavrita/releases/tag/v0.0.1
