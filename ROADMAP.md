# Roadmap

Phases come from [PRD v2.0](docs/prd/ilavrita-prd-v2.0.docx). Nothing here is a
delivery date; it is an ordering, and the order matters more than the dates.

The guiding rule is that correctness comes before breadth. A small number of
interactions that behave exactly as FHIR specifies is worth more than a large
number that mostly work.

v2.0 defines the whole platform rather than only the first server release. The
capabilities below therefore belong to one product definition and must share a
single security and Project model, even though they ship in phases.

## Done — foundations

- [x] Monorepo, build, and the pinned PocketBase fork
- [x] Architecture boundaries expressed as interfaces
- [x] Operational endpoints and an honest CapabilityStatement
- [x] PocketBase's own API and admin console disabled by default (FR-029)
- [x] OpenAPI description checked against the running server
- [x] Open-source project setup and release pipeline

## Phase 1 — FHIR core / v0.1

- [x] Resource create, read, update, delete
- [x] Immutable version history and versioned read
- [x] Optimistic concurrency through ETag and If-Match
- [x] Search parser and SearchParameter registry
- [x] Paginated searchset and history Bundles
- [x] OperationOutcome on every FHIR failure path
- [x] Project isolation enforced in the query layer
- [x] Authentication, authorization and an audit trail
- [ ] Typed search indexes — string, token, reference and date are in; number,
      quantity and URI are not
- [ ] Binary and DocumentReference payloads — Binary is in, over local disk, with
      the bytes made durable before the row that names them commits.
      `DocumentReference` refuses inlined bytes and sends a client to `Binary`.
      The S3-compatible store is not in
- [x] Custom search parameters — a Project stores a `SearchParameter` and
      searches by it, with no restart and no operator step. Only what this build
      can actually apply: token, string, reference and date, over an expression
      naming one element. Anything else is refused rather than stored and
      silently unanswered
- [x] Reindexing — a definition enqueues the types it names, and a claimed
      worker outside every request walks them, so a parameter is true of what
      was already stored. An operator-triggered reindex of a type nobody
      redefined is not in
- [x] Backup and restore — `ilavrita backup`, `verify-backup` and `restore`, with
      the database snapshot and the payloads taken in an order that makes the
      archive consistent
- [ ] Upgrade and rollback procedures — the schema is prepared at startup,
      including rebuilding a table to adopt a constraint, and every migration,
      seed and backfill is idempotent and recorded in `super_jobs` against the
      table it acted on. Rolling an install *back* to an earlier release is not
      written down
- [x] Bundle transaction — every entry happens or none does, entries decided
      and audited one at a time, and `urn:uuid:` references resolved across
      them before anything is written
- [x] Bundle batch — every entry settles on its own inside a savepoint, and the
      response describes each one including the ones that failed
- [x] Conditional create, update and delete, and conditional references inside a
      transaction — a search names the target, run under a grant for searching
      that type
- [x] Conditional read — `If-None-Match` and `If-Modified-Since` answer `304`
- [x] `Prefer: return=minimal|representation|OperationOutcome`
- [ ] Type-level and system-level history — a version's sequence is per
      resource, so a type's feed needs `last_updated` ordering and a compound
      cursor rather than a wider query
- [ ] Patch — JSON Patch, FHIRPath Patch and XML Patch
- [ ] XML, and `_format` beyond JSON — R4 says a server SHOULD serve both wire
      formats and this one serves JSON
- [x] Validation and `$validate` — every resource is checked against its own R4
      base definition, on the operation and on every write alike, including all
      203 of R4's required FHIRPath invariants
- [x] Declared profiles — `meta.profile` is resolved and a held profile's
      resource-level rules applied; one this server does not hold is reported
      rather than passed. A profile's narrowed cardinality and slicing are not
      applied
- [x] Reference integrity — `$validate` reports a relative reference that leads
      nowhere, under a read grant for the type it names. Writes do not refuse
      one, because R4 permits it and provisioning a compartment depends on it
- [x] Terminology for required bindings — a code is checked against the value set
      its element is bound to, from the specification's own bundled sets
- [x] Bringing an install into use — one claim token, handed over through the data
      directory and spent once, which is what an external tool needs before it
      can authenticate at all
- [x] SMART App Launch, standalone — `/oauth2/authorize`, `/oauth2/token`, PKCE
      S256 required of every client, rotating refresh tokens, and
      `.well-known/smart-configuration` beside them. How a SMART scope becomes a
      `storage.Scope` is specified in `docs/design/smart-scope-spec.md` and the
      endpoints in `docs/design/smart-endpoints-spec.md`
- [ ] SMART Backend Services — `private_key_jwt` and the `client_credentials`
      grant, which is what makes a `system/` scope mean anything. Until then
      `authz.ParseScope` refuses `system/` rather than softening it to a shared
      secret. `packages/project/jwks.go` reads a registration's public keys and
      nothing uses it yet
- [ ] OpenID Connect — an `id_token`, so `openid`, `fhirUser` and `profile` can be
      granted rather than refused by name
- [ ] EHR launch — the `launch` parameter as an opaque handle an EHR issues and
      this server resolves, rather than as the patient id it is read as today
- [ ] ABDM profiles (NRCeS) — 38 core profiles and 42 value sets, which needs
      profile application beyond the root invariants this build checks today,
      and the terminology directory back for SNOMED CT India and LOINC
- [ ] Single binary and container image

**Conditional interactions are now the one that matters most.** Several
resources can be written as one act, and each is checked against its own
definition and required bindings — so what this server keeps is a record a
client can rely on. What it cannot yet do is let a client say *write this only
if it is not already here*, which is how an ingestion pipeline stays idempotent
and how a transaction entry names a resource by identifier rather than by id.

## Phase 2 — control plane and developer platform

Projects as first-class isolation boundaries; ProjectMembership and
profile-backed identity; Project Admin, Super Project and Super Admin controls;
AccessPolicy and parameterized authorization; client applications and service
accounts; project settings, secrets, quotas, feature flags and linked-project
rules; OAuth and OIDC, SMART App Launch, MFA, external identity providers and
token exchange; the TypeScript SDK, a CLI, GraphQL, subscriptions, terminology,
implementation-guide packages, a stronger `$validate`, Bulk Data, async jobs,
import and export, and expanded search behaviour.

Several of these arrived early because Phase 1 needed them: Projects and
memberships, AccessPolicy with parameterized authorization, client applications
and service accounts, a TOTP second factor, and `rest-hook` and `websocket`
subscriptions. What remains in this phase is the rest of the control-plane
surface, OAuth and OIDC, external identity providers, and the developer tooling.

## Phase 3 — automation and application platform

Administrator and developer console, resource explorer, user and project
administration, Bots and functions, cron and event automation, webhook,
WebSocket, email and message subscriptions, Questionnaire and form tooling,
reusable React clinical components, scheduling, intake, charting, diagnostic
orders, medication workflows, care coordination, messaging, billing primitives,
patient matching, and reference application shells.

## Phase 4 — enterprise, interoperability and analytics

PostgreSQL and cluster storage, horizontal API scale, distributed workers and
jobs, server-scoped subscriptions and system operations, enterprise operational
controls, an on-premises Agent and gateway, HL7v2, DICOM and DICOMweb, FHIRcast,
open-table warehouse synchronisation and analytics integrations,
high-availability deployment patterns, and additional assurance packages.

## Explicit non-goals for Phase 1

FHIR R5, a complete terminology server, production SMART or Bulk Data, FHIRcast,
GraphQL, a workflow or serverless runtime, a no-code builder, multi-node high
availability, PostgreSQL as the primary backend, HL7v2, DICOM, and any
certification claim.

These are not rejected ideas. Most of them are Phase 2 to 4 above. They are
deliberately after a correct core.
