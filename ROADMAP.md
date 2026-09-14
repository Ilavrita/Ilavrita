# Roadmap

Phases come from [PRD v2.0](docs/prd/ilavrita-prd-v2.0.docx). Nothing here is a
delivery date; it is an ordering, and the order matters more than the dates.

The guiding rule is that correctness comes before breadth. A small number of
interactions that behave exactly as FHIR specifies is worth more than a large
number that mostly work.

v2.0 defines the whole platform rather than only the first server release. The
capabilities below therefore belong to one product definition and must share a
single security and Project model, even though they ship in phases.

## Now — scaffold

- [x] Monorepo, build, and the pinned PocketBase fork
- [x] Architecture boundaries expressed as interfaces
- [x] Operational endpoints and an honest CapabilityStatement
- [x] PocketBase's own API and admin console disabled by default (FR-029)
- [x] OpenAPI description checked against the running server
- [x] Open-source project setup and release pipeline

## Phase 1 — FHIR core / v0.1

- [ ] Resource create, read, update, delete
- [ ] Immutable version history and versioned read
- [ ] Optimistic concurrency through ETag and If-Match
- [ ] Typed search indexes: string, token, reference, date, number, quantity, URI
- [ ] Search parser, AST and SearchParameter registry
- [ ] Paginated searchset Bundles
- [ ] Bundle batch and transaction, with proven atomic rollback
- [ ] Structural validation and a baseline `$validate`
- [ ] OperationOutcome on every FHIR failure path
- [ ] Project isolation enforced in the query layer
- [ ] Authentication, authorization and an audit trail
- [ ] Binary and DocumentReference payloads, local and S3-compatible
- [ ] Migrations, backup, restore, upgrade and rollback procedures
- [ ] Single binary and container image
- [ ] Reindexing

## Phase 2 — control plane and developer platform

Projects as first-class isolation boundaries; ProjectMembership and
profile-backed identity; Project Admin, Super Project and Super Admin controls;
AccessPolicy and parameterized authorization; client applications and service
accounts; project settings, secrets, quotas, feature flags and linked-project
rules; OAuth and OIDC, SMART App Launch, MFA, external identity providers and
token exchange; the TypeScript SDK, a CLI, GraphQL, subscriptions, terminology,
implementation-guide packages, a stronger `$validate`, Bulk Data, async jobs,
import and export, and expanded search behaviour.

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
