# Roadmap

Phases come from the product requirements. Nothing here is a delivery date; it
is an ordering, and the order matters more than the dates.

The guiding rule is that correctness comes before breadth. A small number of
interactions that behave exactly as FHIR specifies is worth more than a large
number that mostly work.

## Now — scaffold

- [x] Monorepo, build, and the pinned PocketBase fork
- [x] Architecture boundaries expressed as interfaces
- [x] Operational endpoints and an honest CapabilityStatement
- [x] Open-source project setup and release pipeline

## Phase 1 — FHIR core (v0.1)

The release the product requirements define as v0.1.

- [ ] Resource create, read, update, delete
- [ ] Immutable version history and versioned read
- [ ] Optimistic concurrency through ETag and If-Match
- [ ] Typed search indexes: string, token, reference, date, number, quantity, URI
- [ ] Search parser, AST and SearchParameter registry
- [ ] Paginated searchset Bundles
- [ ] Bundle batch and transaction, with proven atomic rollback
- [ ] Structural validation and a baseline `$validate`
- [ ] OperationOutcome on every FHIR failure path
- [ ] Multi-tenant isolation enforced in the query layer
- [ ] Authentication, authorization and an audit trail
- [ ] Binary and DocumentReference payloads, local and S3-compatible
- [ ] Migrations, backup, restore, upgrade and rollback procedures
- [ ] Single binary and container image
- [ ] Reindexing

## Phase 2 — developer platform

OAuth and OIDC, SMART App Launch, policy-based authorization, the TypeScript
SDK, a CLI, subscriptions, terminology integration, implementation-guide
packages, a stronger `$validate`, Bulk Data, and wider search behaviour.

## Phase 3 — application platform

Operator and developer console, Questionnaire and form tooling, a resource
explorer, and reusable application components.

## Phase 4 — enterprise and interoperability

PostgreSQL and cluster backend, horizontal API scale, distributed background
work, an on-premises agent, HL7v2 connectivity and DICOMweb.

## Explicit non-goals for v0.1

FHIR R5, a complete terminology server, production SMART or Bulk Data, FHIRcast,
GraphQL, a workflow or serverless runtime, a no-code builder, multi-node high
availability, PostgreSQL as the primary backend, HL7v2, DICOM, and any
certification claim.

These are not rejected ideas. They are deliberately after a correct core.
