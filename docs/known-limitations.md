# Known limitations

This page is the authoritative statement of what Ilavrita does not do, and it is
kept accurate on purpose: a healthcare server that overstates its capabilities is
worse than one that does little.

The largest thing it does not do is **validate a resource**. A body that is JSON
and names the type the URL does is stored as sent, so this server will faithfully
keep a clinically nonsensical record. That, more than anything else here, is why
patient data does not belong in this build yet.

## What works

- `GET /healthz` and `GET /version`
- `GET /fhir/R4/metadata`, returning a CapabilityStatement generated from the
  routes the server registered, declaring the types and search parameters this
  build actually serves
- The six single-resource interactions — create, read, vread, update, delete and
  instance history — for the types that CapabilityStatement declares, with
  `ETag`, `Last-Modified`, `Location` and `If-Match` honoured
- Search, as `GET /{type}?...` and `POST /{type}/_search`, answering a
  `searchset` Bundle
- Project isolation and authorization on every one of those interactions: each
  is decided against an `AccessPolicy` and bounded by a `Scope` the storage layer
  compiles into the query. A policy narrows by compartment, by an element's
  value, and by which elements come back
- An audit trail, written in the transaction that performed the thing it records
- Password login with an optional TOTP second factor, and a login throttle
  counted across the install

A resource type is declared only once the conformance suite covers every status
and header rule for it. A type that is not declared answers `404` on every route,
whatever the storage layer would otherwise accept.

126 of R4's 146 types are declared. The list is not a subset of what the storage
layer can hold — it holds any type name — but of what a policy can authorize: 64
types carry no patient data, so an unrestricted rule may cover them, and 62 are
clinical types this build can place in a compartment from their own content. A
type on neither footing is one no policy could reach, so declaring it would
publish a route every confined caller is refused on.

The twenty that are not declared are there for one of three reasons. `Bundle`,
`OperationOutcome` and `Parameters` are wire formats rather than things to store.
`Appointment`, `Group`, `Person`, `Provenance`, `AuditEvent`, `MessageHeader`,
`Linkage`, `EnrollmentRequest`, `EnrollmentResponse`, `SupplyRequest`,
`PaymentNotice` and `PaymentReconciliation` reach their subject through a nested
element — `Appointment.participant.actor`, `Person.link.target` — and the
derivation reads top-level elements only. `Basic`, `BiologicallyDerivedProduct`,
`DeviceMetric`, `ResearchStudy` and `VerificationResult` are unclassified, which
is the safe answer rather than the finished one: an unclassified type is treated
as carrying patient data, so nothing may cover it outright.

## What is not implemented

Every other `/fhir/R4` route answers `501 Not Implemented` as an
`OperationOutcome`.

| Area | State |
| --- | --- |
| Search | Working, over a declared parameter set; anything outside it is refused, never ignored |
| `_include`, `_revinclude`, chaining, `_sort` | Not implemented; refused rather than ignored |
| History paging and filtering (`_count`, `_since`, `_at`, `_list`) | Ignored; see below |
| Type-level and system-level history | Not implemented |
| Bundle batch and transaction | Not implemented |
| Conditional create, update, delete | Not implemented |
| Conditional read (`If-None-Match`, `If-Modified-Since`) | Not implemented |
| Patch | Not implemented |
| Validation and `$validate` | Not implemented |
| Clinical resource types | Served, reachable only through a compartment a policy names |
| Authentication | Working: password, sessions, TOTP second factor with an administrator recovery path, per-install throttle; see below |
| Audit trail | Working: every interaction and login, in the transaction that did it |
| Binary payloads | Working: bytes kept outside the database, placed by `securityContext` |
| DocumentReference | Working: the document is a `Binary` its attachment names; inlined bytes are refused |
| Subscriptions | Working: `rest-hook`, and `websocket` within one process; see below |
| Reindexing | Not implemented; the index is rebuilt once when an install first gains it |
| Backup, restore | Working: `ilavrita backup`, `verify-backup` and `restore`; see below |
| Structured logging, request correlation | Not implemented |

`packages/config` and `packages/observability` are outlines that document
intended behaviour.

Search parameters are deliberately a short list. Each one is a projection a
write has to maintain and a predicate a read has to compile, so
`packages/search/registry.go` is what this build answers rather than what R4
defines. Adding a type or a parameter means adding both.

## A resource is placed by what it says, and only by that

A resource lands in the compartments its own content names — the patient a
reading is about, the encounter it happened in — derived on write and replaced
on every write that states new content. A confined policy reads through that
projection, so a patient reading their own chart works.

Two consequences worth knowing:

A resource landing in **no** compartment cannot be written by a confined caller.
It is refused with `403` before anything is stored, because a row its own author
could not read back is one stranded under an id nobody was told. A subject-less
Observation, or a `Binary` naming no `securityContext`, is that case.

A confined caller cannot **move** a resource out of the compartments it holds.
Writing `subject: Patient/someone-else` under a grant naming `Patient/mine` is
the same act as creating it there, and is refused the same way.

## Subscriptions reach one process

A `rest-hook` subscription survives anything: the notification is a row, retried
six times over about half an hour, and posted from whichever instance picks it
up. The dialer refuses loopback, private and link-local addresses, because the
subscriber chooses the URL and the server makes the request.

A socket is authorized once, at the connect, and then held — so the session
behind it is asked after again rather than assumed. A logout or an expiry closes
it within thirty seconds, and nothing is delivered over it in between: the
delivery path checks as well. The connection never outlives the session, whatever
the idle bound says.

A `websocket` subscription reaches only the instance the subscriber is connected
to. A notification worked out elsewhere has nobody there to tell, and is recorded
as never delivered rather than retried — retrying would not move it to the
instance holding the socket. Use `rest-hook` where a notification must not be
missed.

## Several replicas share one notification queue

Each replica runs a worker, and both queues — the writes waiting to be fanned out
and the notifications waiting to be sent — are claimed before they are worked
through. A claim is a worker and a lease together, taken in the same statement
that reads the rows, so two replicas reaching the same row do not both come away
with it. A replica that dies gives its rows back when the lease runs out rather
than holding them for good.

Fan-out is idempotent besides: a delivery's identifier is derived from the write
and the subscription it is owed to, so a write fanned out again — by a worker
that died before settling it — finds its deliveries already enqueued rather than
owing every subscriber twice.

**Delivery is still at-least-once.** A worker that dies between posting a
notification and recording that it posted it leaves that notification to be made
again, and no claim can close that window. Every `rest-hook` request carries
`X-Delivery`, which is stable across attempts: a subscriber that must act once
per write acts once per identifier.

Fan-out costs one search per subscription per write, done by a worker outside the
request. It is fine at a handful of subscriptions and is the first thing to
revisit if that number grows.

## A document is a Binary, and a DocumentReference names it

`DocumentReference.content.attachment.data` is refused with `400`. The bytes are
posted as a `Binary` — where this server keeps them outside the row — and the
attachment's `url` names it. R4 gives the attachment that url for exactly this,
and accepting the data member would put in a row the megabytes the Binary route
exists to keep out of one.

Every other type's attachments are still stored in the row: a `Media.content`, a
`DiagnosticReport.presentedForm`, a `Communication.payload`. Nothing bounds them
but the 4 MiB one request body may be. That is a limitation rather than a
decision — what makes `DocumentReference` different is only that R4 gave it a url,
so there is somewhere to send a client instead of refusing with no alternative.

## Backup and restore

`ilavrita backup <directory>` writes an archive: a consistent snapshot of the
database taken with `VACUUM INTO`, the payload directory beside it, and a
manifest naming every file with its SHA-256 and size. `ilavrita verify-backup`
reads every file and compares it with the manifest — a backup nobody checked is
one whose first test is the day it is needed. `ilavrita restore <directory>`
verifies in full and only then puts the files back.

The order is what makes an archive consistent. The database is snapshotted first
and the payloads copied afterwards, so every row in the snapshot names bytes that
already existed and are therefore copied. The cost is an archive that may hold a
payload no row names — a write that landed between the two — which is wasted
space and nothing else. A restore reverses it: payloads first, database last.

A restore refuses a data directory that already holds a database. Restoring over
a live install is deliberate work: move the old one aside first.

**What it does not cover.** PocketBase's own `data.db` and `auxiliary.db` are not
in the archive. They hold no Ilavrita data — every row this server writes is in
`ilavrita.db` — and the runtime recreates them. Nothing is scheduled, retained or
rotated: the archive is a directory, and when to take one is the operator's.
`ILAVRITA_SEALING_KEY` is not in the archive either, which is deliberate and
load-bearing: without it a restored database holds no readable second factor, so
the key has to be kept somewhere the archive is not.

The archive holds everything the database holds — patient data, password hashes,
sealed second factors and the audit trail — and every file in it is written
`0600` under a `0700` directory. It is not encrypted; encrypting it is the
operator's, on the medium it is stored on.

## A payload and its row are two stores

A `Binary`'s bytes live on the disk and the resource describing them lives in the
database, so a write has to settle what a crash between them means. The bytes are
placed inside the transaction that writes the row and flushed — the file, and the
directory entry naming it — before that transaction commits. So a crash can leave
a file no row names, which is wasted space, but never a row naming bytes that are
not there. A transaction that rolls back takes its payload with it, at whichever
boundary decided not to commit.

Two things this does not cover. A drive that lies about its own write cache
defeats it, exactly as it defeats the database. And a payload removed behind the
server's back — an edited store, a backup restored in halves — leaves a row whose
bytes are gone: reading it answers `500` and says so, rather than handing back a
`Binary` that claims to be a PDF and carries nothing.

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

The login route is rate limited: five failed attempts per identity and twenty per client address
in a fifteen-minute window, checked before the password is proved, cleared by a success. The count
lives in the database rather than in process memory, so several instances share one limit. What is
counted against is a digest, never the address itself: a table of who tried to log in and failed
is a list of this install's users and where they were.

That digest is keyed, under a key derived from `ILAVRITA_SEALING_KEY`. An unkeyed one would hide
nothing — an email address and an IPv4 address are both drawn from a space small enough to walk —
so anybody holding the table could hash their way back to every entry in it. Rotating the key
costs at most the counts inside one fifteen-minute window. **A deployment that configured no
sealing key names attempts under a key of zeroes**: the throttle still works, and the table hides
nothing. The server says so at startup.

An identity may enrol a TOTP second factor. It is pending until a code proves it, and replacing
one that is in force needs a code from the one it replaces — moving to a new phone and switching
the factor off are the same request, and the code is what tells them apart. The factor in force
stays in force until the new one is proved.

The secret is sealed with `ILAVRITA_SEALING_KEY`. Unlike a password it cannot be hashed, because
the server computes the same code the phone does; sealing means a leaked database file is not a
list of everyone's second factor. A deployment that configured no key holds no factors rather
than storing them in the clear.

A lost phone is recovered by somebody else. `DELETE /admin/projects/{project}/users/{user}/second-factor`
takes a factor off an identity, and needs standing to administer that Project — the identity must hold
standing there, and an identity holding none is the same answer as one that does not exist. The
identity is signed out everywhere as part of it, because the reset is also what an operator reaches
for when the phone was stolen rather than lost. It is recorded against who did it and to whom.

**An administrator cannot recover their own.** That is the hole the whole design closes: withdrawing
your own factor needs a code from it, so an administrator who could reach around that with their own
session would make a stolen administrator session enough to disable the factor it was meant to
survive. The cost is real — an install with one administrator who loses their phone has no way back
through the API, and an operator has to reach the database.

**What is still missing:**

- **Session refresh.** A session expires and the credential is proved again.

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
