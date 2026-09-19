# Security model

To report a vulnerability, follow [SECURITY.md](../SECURITY.md). This page
describes the controls, and says for each one whether it is enforced today.

> **Do not put patient data in this build.** Not because these controls are
> absent — most are enforced and tested — but because nothing here has had an
> external security review, and because a resource is still not checked against a
> profile or a terminology. See [known-limitations.md](known-limitations.md) for
> the authoritative list of what is missing.

## Assumption

Any deployment may hold protected health information, even one created for a
demo. The design assumes PHI is present rather than treating it as a special
case, because a control that is optional is a control that is missing when it
matters.

## Controls

**Project isolation. Enforced.** Every resource, version, search index, payload
and audit record belongs to exactly one Project, and the identifier is derived
server-side rather than read from a client-supplied FHIR field. It is enforced in
the query layer through composite keys and a compiled `Scope`, not by filtering
results afterwards (FR-026, FR-045, FR-046).

**Authorization. Enforced.** Decided before results are returned, on read,
search, history, payload and administrative paths alike. A policy narrows by
compartment, by an element's value and by which elements come back, and the
narrowing is compiled into the query — so a caller never receives rows they are
not entitled to and then has them removed in memory.

**Privilege separation. Enforced.** Project Admin is administrative authority
over a Project's configuration and membership; it does not by itself grant
clinical data access beyond the member's AccessPolicy. Super Admin is server-wide
authority held through Super Project membership (FR-049 to FR-052). The control
plane is a working subset: it creates Projects, invites identities, grants
standing, registers client applications and recovers a lost second factor. Policy
authoring, link management and the list endpoints are store-only.

**Authentication. Enforced.** Password with argon2id, sessions pinned to a
Project and a standing, an optional TOTP second factor, and a login throttle
counted across the install rather than within one process. There is no
development principal and no environment variable that names one.

**Administrative separation. Enforced.** Administrative routes are separate from
FHIR routes, carry their own policy and are deny-by-default. PocketBase's own
REST API and admin console are disabled unless a deployment opts back in, and are
not part of the product contract (FR-029).

**One way to be an administrator. Enforced.** There is no PocketBase superuser.
Such an account would sit outside the authorization model entirely — no Project,
no Membership, no Grant, nothing narrowed by a compartment and nothing written to
the audit trail — and the console it unlocks can archive the whole data directory,
every Project's record with it. The command that mints one is not registered, a
test refuses any source that calls `app.Start` and would re-register it, and every
`_superusers` row is deleted at startup on every start. Administering this install
is Super Admin, decided per request inside the model like everything else.

**Audit. Enforced.** Every interaction and every login records Project, actor,
action, target, timestamp and outcome, written in the transaction that performed
the thing it records — so an action this server could not account for is one it
does not take. The record is independent of the FHIR `AuditEvent` resource, so a
client cannot edit the record of its own actions, and a trigger refuses any
update to a row.

**Deleted data. Enforced.** Deleted resources stay out of search results while
history is preserved, and historical versions carry the same authorization checks
as current ones — under the history action rather than read, because a read grant
may name the compartment a resource is in today and an older version need not
share it.

**Secrets. Enforced.** Never in source-controlled defaults. `ILAVRITA_SEALING_KEY`
is supplied by environment or secret store; a deployment that configures none
holds no second factors rather than storing them in the clear.

**Logging. Partly.** Resource bodies are not logged: the server logs a fixed
message and an error value, and no log line carries a resource. What does not
exist is **request correlation** — there is no request id connecting an API
failure to a server log, and structured logging is not implemented.

**Transport. Deployment.** TLS terminates at Ilavrita or at a documented trusted
proxy. Nothing in this build enforces it.

## Review before a stable release

A stable release requires a security review covering cross-Project escape,
authorization bypass, injection, unsafe file access, secret handling, dependency
vulnerabilities and accidental PHI logging, with no unresolved critical finding.
**That review has not happened.** The tests are ours.

## What Ilavrita does not claim

Ilavrita provides technical controls. It holds no certification, and running it
does not make an organisation compliant with HIPAA, GDPR, EHDS or any other
regime. Compliance is a property of your deployment, your policies and your
organisation.
