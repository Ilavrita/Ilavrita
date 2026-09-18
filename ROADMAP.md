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
- [ ] Reindexing — an install gaining the index backfills once; there is no
      operator-triggered reindex
- [x] Backup and restore — `ilavrita backup`, `verify-backup` and `restore`, with
      the database snapshot and the payloads taken in an order that makes the
      archive consistent
- [ ] Upgrade and rollback procedures — the schema is prepared at startup,
      including rebuilding a table to adopt a constraint, and every migration,
      seed and backfill is idempotent and recorded in `super_jobs` against the
      table it acted on. Rolling an install *back* to an earlier release is not
      written down
- [ ] Bundle batch and transaction, with proven atomic rollback
- [x] Validation and `$validate` — every resource is checked against its own R4
      base definition, on the operation and on every write alike
- [ ] Terminology — a required binding is not checked, so `"status": "banana"`
      passes
- [ ] Single binary and container image

**Terminology is now the one that matters most.** A resource is checked against
its own definition — an element nobody declared, a missing required one, a
malformed date are all refused — but a code is not checked against the ValueSet
its element is bound to. That is the remaining difference between a server that
keeps a well-formed record and one that keeps a record somebody can rely on.

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
