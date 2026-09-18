# Known limitations

This page is the authoritative statement of what Ilavrita does not do, and it is
kept accurate on purpose: a healthcare server that overstates its capabilities is
worse than one that does little.

The largest thing it does not do is **check a resource against a profile or a
terminology**. It checks every resource against its own base definition — see
below — so an element nobody declared, a missing required one or a malformed date
is refused. What it cannot tell you is that `"status": "banana"` is not a status,
because a required binding is a ValueSet this build does not hold. That, more
than anything else here, is why patient data does not belong in this build yet.

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
| History paging (`_count`, `_cursor`) | Working; see below |
| History filtering (`_since`, `_at`, `_list`) | Ignored |
| Type-level and system-level history | Not implemented |
| Bundle batch and transaction | Not implemented |
| Conditional create, update, delete | Not implemented |
| Conditional read (`If-None-Match`, `If-Modified-Since`) | Not implemented |
| Patch | Not implemented |
| Validation and `$validate` | Against the base definitions; no profiles and no terminology; see below |
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

## The base definitions are seeded, and not yet used to validate

This build embeds the FHIR R4 specification's own definition bundles — 4.0.1, as
published, gzipped and otherwise unmodified, with their digests recorded in
`packages/conformance/definitions/SOURCE.md`. Startup seeds every
`StructureDefinition` into an install-wide store, and
`GET /fhir/R4/StructureDefinition/{id}` answers with the specification's own.

**Seeding is idempotent and cheap.** A digest of what the build embeds is
compared with what the install last seeded; when they match, startup reads one
row and stops rather than parsing thirty-five megabytes of JSON. When they differ
— a first start, or an upgrade — every definition is written and anything the new
bundles no longer carry is removed, in one transaction: a seed half-done beside a
marker saying it is done is the one state nothing would ever correct.

They belong to no Project. A copy per Project would be the same bytes written as
many times as there are tenants, and there is nothing tenant-specific about what
an `Observation` is. A Project that writes its own `StructureDefinition` under
one of those ids serves its own; the specification's is what is behind it.

The definitions are also what a write is checked against; see below.

The fallback is a decision rather than an inference. A canonical resource belongs
to no Project, so there is no row for a compartment, a filter or a projection to
be evaluated against, and it is answered only for a caller whose Grant narrows
none of those. That matters because the Project's own store returns the same
"not found" for a row that is not there and for one the caller may not see — an
id must not be probeable — so reading the fallback off that answer would be
handing resources out on a guess that is wrong exactly when it matters. A
confined caller gets the 404 they would have got anyway.

## Validation checks a resource against its own definition

`POST /fhir/R4/{type}/$validate` answers an `OperationOutcome` naming each issue
and the element it is about. It answers `200` whichever way it went, as R4 says:
the operation was performed, and the issues are the result. The same rules gate
`POST` and `PUT`, which refuse with `400` carrying the same issues.

**What is checked without a definition**, because these hold for every R4
resource whatever its own definition says: no `null`, no empty string, no empty
array anywhere; an `id` that is the `id` datatype; a relative reference that names
an R4 resource type and an id. A `meta.versionId` or `meta.lastUpdated` a client
sends is a warning — the write path stamps its own, and the warning is how the
client learns theirs was not kept.

**What is checked against the definition**, from the snapshots this build seeds:

- **Elements nobody declared.** An `Observation` carrying `activeIngredient` is
  refused, and the refusal names what the type does declare.
- **Cardinality.** A required element that is absent, and the difference between
  an element written as an array and one written as a value — R4 writes a
  repeating element as an array always, because that is what tells a reader
  whether more may follow.
- **Choice types.** `value[x]` is present as `valueQuantity` or `valueString`,
  under a type the element actually permits, and never as two at once.
- **Primitive syntax.** A `date`, `dateTime`, `instant`, `time`, `code`, `id`,
  `oid`, `uuid` or `base64Binary` that is not one; a number where a string
  belongs and the reverse; an `integer` that is not whole; a `positiveInt` below
  one.

**What is still not checked.** No profiles: only the base definitions are read,
and a `StructureDefinition` that constrains one is stored without being applied.
No terminology: `"status": "banana"` passes, because a required binding is a
ValueSet this build does not hold. No FHIRPath invariants, and no reference that
actually resolves. Those are the gap between this and a validator somebody should
certify against.

An element that holds its own kind — a `Questionnaire` item inside an item, an
`OperationDefinition` parameter's parts — is expanded six levels deep, because a
definition that nests into itself cannot be written out. Past that the content is
**unchecked rather than refused**: an element with no children in the model would
otherwise have everything inside it reported as undeclared, which is a refusal
for a resource that is right.

The strongest evidence that it does not refuse what it should accept is that the
specification validates against itself: every resource the embedded bundles
carry — 212 `StructureDefinition`s, 46 `OperationDefinition`s, five
`CompartmentDefinition`s and two `CapabilityStatement`s — passes, and a test
holds that.

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

## What this server does to itself is written down

A migration, a seed or a backfill belongs to no Project — it is done to the
install — so each one is a **super job**, and `super_jobs` records it against the
table it acted on. "What has been done to `user_second_factors`" is one query
rather than an inference from the shape of the table.

Every job decides for itself whether there is anything to do: a migration looks
at the database, a seed compares a digest of what it would apply. Running one
twice does nothing the second time. **A start that changed nothing writes
nothing** — a server starts far more often than its schema changes, and a row per
start per job would bury the ones that matter. A job that failed is recorded
before the failure is returned, because that run is the one somebody needs to
find afterwards, and nothing revises a row once written.

A fresh install therefore records no migration at all: the schema creates each
table with its columns already in it, and every migration finds nothing to do.

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

## Instance history is paged

`GET /fhir/R4/{Type}/{id}/_history` returns 20 versions by default and at most
200, newest first, with a `next` link when more follow. `_count` and `_cursor`
are the same parameter names a search takes, and a cursor is a version this
server handed back.

A count outside that range, or a cursor that is not a version, is refused with
`400` rather than rounded down — a caller who asked for a thousand and silently
got two hundred cannot tell a short page from the end of the history.

`total` is present only on a history that fits in one page. Counting a longer one
would be a second query over the same rows, and a total that was guessed is worse
than one that is absent.

One consequence worth knowing: which interaction produced a version is read from
where it sits in the history, so only the page holding the oldest version marks
an entry as the `POST` that created the resource. Every entry on a page with more
to come is a `PUT` or a `DELETE`.

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
