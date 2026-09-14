# Security model

To report a vulnerability, follow [SECURITY.md](../SECURITY.md). This page
describes the design.

> Nothing described here is implemented yet. Ilavrita currently enforces no
> authorization, no Project boundary and no audit. Use synthetic data only.

## Assumption

Any deployment may hold protected health information, even one created for a
demo. The design assumes PHI is present rather than treating it as a special
case, because a control that is optional is a control that is missing when it
matters.

## Planned controls

**Project isolation.** Every canonical resource, version, search index, file
reference, audit record and job belongs to exactly one Project, and the
identifier is derived server-side rather than read from a client-supplied FHIR
field. It is enforced in the query layer rather than by filtering results
afterwards (FR-026, FR-045, FR-046).

**Privilege separation.** Project Admin is administrative authority over a
Project's configuration and membership; it does not by itself grant clinical
data access beyond the member's AccessPolicy. Super Admin is server-wide
authority held through Super Project membership, deliberately elevated, fully
audited, and kept out of ordinary clinical workflows (FR-049 to FR-052).

**Authorization.** Checked before results are returned, on read, search, history,
file and administrative paths alike. Search predicates take part in query
planning, so a caller never receives rows they are not entitled to and then has
them removed in memory.

**Administrative separation.** Administrative routes are separate from FHIR
routes and carry their own policy, and they are deny-by-default. PocketBase
administrative endpoints are not part of the FHIR product contract.

**Audit.** Security-relevant events record Project, actor, action, target,
timestamp, outcome and request id. Audit evidence is written by the server and is
independent of the FHIR `AuditEvent` resource, so a client cannot edit the record
of its own actions.

**Logging.** Resource bodies are never logged. A request id connects an API
failure to a server log without putting patient data in it.

**Deleted data.** Deleted resources stay out of normal search results while
history is preserved, and historical versions carry the same authorization checks
as current ones.

**Secrets.** Never in source-controlled defaults. Supplied by environment or
secret store.

**Transport.** TLS terminates at Ilavrita or at a documented trusted proxy.

## Review before a stable release

A stable release requires a security review covering cross-Project escape,
authorization bypass, injection, unsafe file access, secret handling, dependency
vulnerabilities and accidental PHI logging, with no unresolved critical finding.

## What Ilavrita does not claim

Ilavrita provides technical controls. It holds no certification, and running it
does not make an organisation compliant with HIPAA, GDPR, EHDS or any other
regime. Compliance is a property of your deployment, your policies and your
organisation.
