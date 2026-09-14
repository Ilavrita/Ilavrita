# Known limitations

Ilavrita is a scaffold. This page is the authoritative statement of what it does
not do, and it is kept accurate on purpose: a healthcare server that overstates
its capabilities is worse than one that does little.

## What works

- `GET /healthz` and `GET /version`
- `GET /fhir/R4/metadata`, returning a CapabilityStatement that declares no
  supported resources

## What is not implemented

Every other `/fhir/R4` route answers `501 Not Implemented` as an
`OperationOutcome`.

| Area | State |
| --- | --- |
| Create, read, update, delete | Not implemented |
| Version history, versioned read | Not implemented |
| Optimistic concurrency (ETag, If-Match) | Not implemented |
| Search, of any parameter type | Not implemented |
| `_include`, `_revinclude`, chaining | Not implemented |
| Bundle batch and transaction | Not implemented |
| Conditional create, update, delete | Not implemented |
| Validation and `$validate` | Not implemented |
| Project isolation | Interface only, not enforced |
| Authentication and authorization | Not implemented for FHIR routes |
| Audit trail | Not implemented |
| Binary and DocumentReference payloads | Not implemented |
| Reindexing | Not implemented |
| Migrations, backup, restore | Not implemented |
| Structured logging, request correlation | Not implemented |

`packages/storage` defines interfaces with no implementation behind them.
`packages/search`, `packages/tenancy`, `packages/authz`, `packages/audit`,
`packages/files`, `packages/config` and `packages/observability` are outlines
that document intended behaviour.

## Out of scope for v0.1

Not missing — deliberately deferred. FHIR R5, a complete terminology server,
production SMART App Launch, Bulk Data, FHIRcast, GraphQL, a workflow runtime, a
no-code builder, multi-node high availability, PostgreSQL as the primary backend,
HL7v2, and DICOM or DICOMweb.

## Compliance

Ilavrita holds no certification and makes no compliance claim. Running it does
not make an organisation compliant with HIPAA, GDPR, EHDS or any other regime.

## The PocketBase surface

PocketBase serves its own REST API at `/api` and an admin console at `/_`.
Ilavrita does not publish these as product surface (FR-029), and they are
disabled by default. Set `ILAVRITA_EXPOSE_POCKETBASE=true` to restore them for
local development.

They are not a supported interface. Anything built against them will break.

## Operational consequence

**Do not put patient data in Ilavrita.** There is no authorization, no Project
enforcement and no audit trail. Use synthetic data only.
