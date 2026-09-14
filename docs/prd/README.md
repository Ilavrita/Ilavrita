# Product requirements

| Version | Status | Document |
| --- | --- | --- |
| 2.0 | Current | [`ilavrita-prd-v2.0.docx`](ilavrita-prd-v2.0.docx) |
| 0.1 | Superseded, retained | [`ilavrita-prd-v0.1.docx`](ilavrita-prd-v0.1.docx) |

v0.1 is kept rather than deleted: the PRD's own Controls and Records section
requires superseded records to remain recoverable.

## What changed in 2.0

v0.1 defined the first server release. v2.0 defines the whole platform and keeps
that server release as Phase 1, unchanged.

**Unchanged:** Phase 1 goals G-001 to G-010 and its non-goals, the FHIR R4
boundary, the product principles, the PocketBase boundary, licensing direction,
and repository and release requirements. `FR-001` to `FR-044` keep their numbers
and titles, so references to them in code and documentation still resolve.

**Added:** `FR-045` to `FR-095`, covering a control plane the earlier document
did not describe — Projects, ProjectMembership, Project Admin, a privileged Super
Project and Super Admin, AccessPolicy, client applications and service accounts,
project settings, secrets and quotas — plus the developer platform, automation,
interoperability, clinical capabilities and analytics.

**Renamed:** the isolation boundary is now a **Project**, not a tenant. `FR-026`
and `FR-045` require every resource, version, index entry, file reference, audit
record and job to belong to exactly one Project, and storage APIs to require
Project context rather than accept it optionally.
