<p align="center">
  <img src="assets/brand/banners/ilavrita_open_healthcare_hero_banner.png" alt="Ilavrita — open healthcare infrastructure" />
</p>

<h1 align="center">Ilavrita</h1>

<p align="center">
  <strong>Open healthcare infrastructure. FHIR-native, self-hostable, edge to enterprise.</strong>
</p>

<p align="center">
  <a href="https://github.com/Ilavrita/Ilavrita/actions/workflows/ci.yml"><img src="https://github.com/Ilavrita/Ilavrita/actions/workflows/ci.yml/badge.svg" alt="CI" /></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-AGPL--3.0-4F46E5" alt="AGPL-3.0" /></a>
<a href="https://app.fossa.com/projects/git%2Bgithub.com%2FIlavrita%2FIlavrita?ref=badge_shield" alt="FOSSA Status"><img src="https://app.fossa.com/api/projects/git%2Bgithub.com%2FIlavrita%2FIlavrita.svg?type=shield"/></a>
  <a href="https://golang.org"><img src="https://img.shields.io/badge/go-1.27-7C3AED" alt="Go 1.27" /></a>
  <a href="https://hl7.org/fhir/R4/"><img src="https://img.shields.io/badge/FHIR-R4%204.0.1-A78BFA" alt="FHIR R4" /></a>
</p>

---

> [!IMPORTANT]
> **Ilavrita is a scaffold today, not a working FHIR server.** The repository,
> build, architecture boundaries and release pipeline exist. The FHIR
> interactions do not. Every `/fhir/R4` route other than `metadata` answers
> `501 Not Implemented`, and the CapabilityStatement declares no supported
> resources — deliberately, because a server must never advertise behaviour it
> has not implemented and tested. Track progress in [ROADMAP.md](ROADMAP.md).


[![FOSSA Status](https://app.fossa.com/api/projects/git%2Bgithub.com%2FIlavrita%2FIlavrita.svg?type=large)](https://app.fossa.com/projects/git%2Bgithub.com%2FIlavrita%2FIlavrita?ref=badge_large)

## Why Ilavrita

Healthcare teams rebuild the same primitives every time: clinical resources,
version history, search, validation, access control, audit, file handling and a
safe way to deploy. Ilavrita exists so that work is done once, in the open.

Many of those teams also need something far smaller than a distributed
application stack. A clinic, a pilot, a device-adjacent service or a local
development environment should not require an external database to run a
standards-compliant FHIR server.

Ilavrita targets both ends: a single binary with SQLite for small deployments,
and an architecture that does not have to be rewritten to reach a clustered one.

## Status

| Capability | State |
| --- | --- |
| `GET /healthz`, `GET /version` | Working |
| `GET /fhir/R4/metadata` | Working, declares no resources |
| FHIR create, read, update, delete, history | Not implemented |
| FHIR search | Not implemented |
| Bundle batch and transaction | Not implemented |
| Multi-tenancy, authorization, audit | Interfaces only |
| PostgreSQL, SMART, Bulk Data, HL7v2, DICOM | Out of scope for v0.1 |

[docs/known-limitations.md](docs/known-limitations.md) is the authoritative list.

## Quick start

Requires Go 1.27+, and Node 22+ with pnpm 10+ for the workspace tooling.

```bash
git clone https://github.com/Ilavrita/Ilavrita.git
cd Ilavrita
make bootstrap
make run
```

`make bootstrap` fetches the pinned PocketBase fork, which the build needs — a
plain clone without submodules will not compile.

```bash
curl http://127.0.0.1:8090/healthz
curl http://127.0.0.1:8090/fhir/R4/metadata
```

## Repository layout

```
apps/ilavrita           the server binary
apps/console            operator console (not implemented)
apps/docs               documentation site (not implemented)
packages/fhir           FHIR R4 types published at the boundary
packages/search         query parsing and planning
packages/storage        the persistence interfaces services depend on
packages/storage/pocketbase   SQLite backend, the only package that may import PocketBase
packages/tenancy        tenant isolation
packages/authz          authorization decisions
packages/audit          security event records
packages/files          Binary and DocumentReference payload storage
packages/config         runtime configuration
packages/observability  logging, correlation, health
packages/sdk-typescript TypeScript client
third_party/pocketbase  the pinned Ilavrita PocketBase fork
```

## Architecture in one paragraph

PocketBase is the runtime foundation, not the product. The FHIR contract —
routes, error shapes, search semantics, versioning and authorization — belongs to
Ilavrita and sits behind its own interfaces. SQLite-specific SQL stays inside the
storage backend, so a PostgreSQL backend can be added later without touching FHIR
behaviour. PocketBase collections, admin endpoints and error formats never
surface through `/fhir/R4`. See [docs/architecture.md](docs/architecture.md).

## Documentation

- [Architecture](docs/architecture.md)
- [Deployment](docs/deployment.md)
- [Security model](docs/security.md)
- [Known limitations](docs/known-limitations.md)
- [Product requirements](docs/prd/ilavrita-prd-v0.1.docx)

## Contributing

Start with [CONTRIBUTING.md](CONTRIBUTING.md). Everyone taking part agrees to the
[Code of Conduct](CODE_OF_CONDUCT.md).

To report a vulnerability, follow [SECURITY.md](SECURITY.md) rather than opening
a public issue.

## Licence

AGPL-3.0-only — see [LICENSE](LICENSE). A commercial option is planned;
see [COMMERCIAL-LICENSE.md](COMMERCIAL-LICENSE.md).

Ilavrita builds on [PocketBase](https://github.com/pocketbase/pocketbase) (MIT),
whose notice is preserved in [LICENSES/PocketBase-MIT.txt](LICENSES/PocketBase-MIT.txt).

> Ilavrita does not make any organisation compliant with HIPAA, GDPR, EHDS or any
> other regime. It provides technical controls. Compliance is a property of your
> deployment, your policies and your organisation.