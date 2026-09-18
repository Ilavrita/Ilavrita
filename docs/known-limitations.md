# Known limitations

Ilavrita is a scaffold. This page is the authoritative statement of what it does
not do, and it is kept accurate on purpose: a healthcare server that overstates
its capabilities is worse than one that does little.

## What works

- `GET /healthz` and `GET /version`
- `GET /fhir/R4/metadata`, returning a CapabilityStatement generated from the
  routes the server registered, declaring the types this build actually serves
- The six single-resource interactions — create, read, vread, update, delete and
  instance history — for the types that CapabilityStatement declares, with
  `ETag`, `Last-Modified`, `Location` and `If-Match` honoured
- Project isolation and authorization on every one of those interactions: each
  is decided against an `AccessPolicy` and bounded by a `Scope` the storage layer
  compiles into the query

A resource type is declared only once the conformance suite covers every status
and header rule for it. A type that is not declared answers `404` on every route,
whatever the storage layer would otherwise accept.

## What is not implemented

Every other `/fhir/R4` route answers `501 Not Implemented` as an
`OperationOutcome`.

| Area | State |
| --- | --- |
| Search, of any parameter type | Not implemented |
| `_include`, `_revinclude`, chaining | Not implemented |
| History paging and filtering (`_count`, `_since`, `_at`, `_list`) | Ignored; see below |
| Compartment-restricted policies | Authorable, but deny everything; see below |
| Type-level and system-level history | Not implemented |
| Bundle batch and transaction | Not implemented |
| Conditional create, update, delete | Not implemented |
| Conditional read (`If-None-Match`, `If-Modified-Since`) | Not implemented |
| Patch | Not implemented |
| Validation and `$validate` | Not implemented |
| Clinical resource types | Not served: no policy can authorize creating one, because a create needs an unconfined grant and an unconfined rule may not cover a type carrying patient data |
| Authentication | Working: password login, sessions, logout; see below |
| Audit trail | Not implemented |
| Binary and DocumentReference payloads | Not implemented |
| Reindexing | Not implemented |
| Migrations, backup, restore | Not implemented |
| Structured logging, request correlation | Not implemented |

`packages/search`, `packages/audit`, `packages/files`, `packages/config` and
`packages/observability` are outlines that document intended behaviour.

## Compartments deny rather than restrict

A policy rule may name a compartment, and one that does compiles into a real SQL
predicate. Nothing in the server ever populates `fhir_resource_compartment`,
though: that projection belongs to the search-index layer, which is not
implemented. A compartment-restricted Grant therefore matches no row and reads
as `404`, indistinguishable from absence.

It fails closed, so no data is exposed by it. But a patient reading their own
chart cannot work until the projection is written, and a deployment that
authors such a policy will see refusals rather than a restricted view.

A write is refused outright under one: a row that does not exist yet has no
compartment projection, so the caller could never read back what it wrote, and
answering a committed create with `404` would strand the row under an id nobody
was told. That is a `403` before anything is written.

## Instance history is unbounded

`GET /fhir/R4/{Type}/{id}/_history` returns every version the caller may see, in
one response, with no `_count` and no `next` link. A resource with a long
history serialises entirely into memory. One pooled database connection per
process compounds it: a long read blocks every other request in that process.
Bound history growth operationally until paging is implemented.

## Version numbers count across identity reuse

Deleting a logical id and creating it again bumps an identity epoch, so the new
occupant never serves the previous one's versions. The version counter is not
reset, so the new occupant's first version is numbered after the last one the
previous occupant wrote. FHIR does not require sequential version ids, but the
number does disclose that the id was used before, and how often.

## Authentication

`POST /auth/login` takes a Project slug, an email address and a password, proves the password
with argon2id, resolves the standing that identity holds in that Project, and issues a session
token that pins both. A later request carrying `Authorization: Bearer <token>` is served as that
principal. `POST /auth/logout` destroys the session's material; `GET /auth/session` describes the
caller a token names.

A session lives eight hours and may not exceed twelve. An unknown address, a wrong password, a
disabled identity and an identity holding no standing in the Project are the same answer, because
telling them apart tells an attacker which addresses and Projects exist.

**What is still missing:** there is no audit trail, no MFA, and no rate limit on the login route,
so a password can be guessed as fast as argon2id answers. There is no refresh: a session simply
expires and the credential is proved again.

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

## Cross-origin requests

The FHIR surface sends no CORS headers and answers no preflight, so no browser
on another origin can read from it. The runtime's own default — allow every
origin — is withdrawn beneath `/fhir/R4` and left in place everywhere else. A
browser-based client needs a proxy on its own origin until a configurable policy
exists.

## Operational consequence

**Do not put patient data in Ilavrita.** There is no authentication and no audit
trail. Use synthetic data only.
