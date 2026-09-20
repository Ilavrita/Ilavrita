# Known limitations

This page is the authoritative statement of what Ilavrita does not do, and it is
kept accurate on purpose: a healthcare server that overstates its capabilities is
worse than one that does little.

The largest thing it does not do is **check a resource against a profile**. Every
resource is checked against its own base definition and its required bindings —
see below — so an element nobody declared, a missing required one, a malformed
date and a code outside the set it is bound to are all refused. What it cannot
tell you is that a resource fails a profile somebody wrote for it, or a FHIRPath
invariant, or that a reference points at nothing.

That, and the absence of an external security review, is why patient data does
not belong in this build yet.

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
- `POST /fhir/R4`, performing a `transaction` Bundle as one act
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
| Search modifiers | `:missing`, `:exact`, `:contains` and `:not`; every other one is refused by name |
| `_include`, `_revinclude`, chaining, `_has`, `_sort`, `_summary`, `_elements` | Not implemented; refused rather than ignored |
| Parameter types | token, string, reference and date; number, quantity, uri, composite and special are not indexed |
| Date prefixes | `eq`, `gt`, `lt`, `ge`, `le`; `ne`, `sa`, `eb` and `ap` are not |
| History paging (`_count`, `_cursor`) | Working; see below |
| History filtering | `_since` narrows; `_at` and `_list` are refused, never ignored |
| Type-level and system-level history | Not implemented; see below |
| Bundle transaction | Working: all-or-nothing; see below |
| Bundle batch | Working: every entry settles on its own; see below |
| Conditional create, update, delete | Working; see below |
| Conditional read (`If-None-Match`, `If-Modified-Since`) | Working: `304` with the validators and no body |
| `Prefer: return=` | Working: `minimal`, `representation`, `OperationOutcome` |
| Patch | Not implemented |
| XML, and `_format` beyond JSON | Not implemented; anything but JSON is refused with `406` |
| Validation and `$validate` | Against the base definitions, required bindings and R4's invariants; profiles partly; see below |
| Clinical resource types | Served, reachable only through a compartment a policy names |
| Authentication | Working: password, sessions, TOTP second factor with an administrator recovery path, per-install throttle; see below |
| SMART App Launch | Working for the standalone launch: authorize, token, PKCE S256, refresh, discovery; see below |
| SMART Backend Services | Not implemented: `private_key_jwt` is unbuilt, so `system/` scopes are refused rather than granted |
| OpenID Connect | Not implemented: no `id_token`, so `openid`, `fhirUser` and `profile` are refused by name |
| Audit trail | Working: every interaction and login, in the transaction that did it |
| Binary payloads | Working: bytes kept outside the database, placed by `securityContext` |
| DocumentReference | Working: the document is a `Binary` its attachment names; inlined bytes are refused |
| Subscriptions | Working: `rest-hook`, and `websocket` within one process; see below |
| Custom search parameters | Working: a Project stores a `SearchParameter` and searches by it; see below |
| Reindexing | Working for a type whose parameters changed; no operator-triggered reindex |
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

## A transaction is one act

`POST /fhir/R4` with a `transaction` Bundle performs every entry or none of
them. The commit boundary is the one the audit decorator already opens around
the route, so an entry that fails takes with it every row, audit record and
queued notification the entries before it wrote.

Entries run in R4's order — deletes, then creates, then updates — but every
identity is settled before any row is written. Resolving references as the
entries ran would mean an entry created early could not name one updated late,
which is the ordinary case: a Bundle stating a Patient and an Observation about
them usually states them in that order and updates the Patient. A `urn:uuid:`
fullUrl is the placeholder; one no entry claims is left as written rather than
rewritten to nothing, because a reference to an absolute URL outside the Bundle
is a real reference.

**Every entry is decided and audited on its own.** A Bundle is not a way to
perform an interaction the caller could not have performed one at a time, and
not a way to write without a trail: a create inside a transaction is checked
against the same grants and produces the same audit row as the same create sent
alone. The transaction itself is recorded too, as the act the entries belong to,
naming no resource — a row naming whichever entry ran last would say the
transaction was about that resource.

A `PUT` at an identity nothing holds creates it, which is what `update` does on
every other route here and what the CapabilityStatement declares as
`updateCreate`.

What a transaction does not do:

- **`batch` is refused**, not performed as a transaction. Independent
  success and failure per entry is a different promise, and answering it with
  all-or-nothing would be a promise this server breaks
- **An entry states no precondition.** R4 puts `ifMatch`, `ifNoneExist`,
  `ifNoneMatch` and `ifModifiedSince` in the entry's own request; this build
  performs no conditional interaction anywhere, so an entry stating one is
  refused. Dropping it silently would answer a conditional write with an
  unconditional one and say nothing about the difference
- **`GET` entries are refused.** A transaction here writes; reading inside one
  would return a resource the client would have to tell apart from the ones it
  sent
- **At most 200 entries.** One commit holds this process's single pooled
  connection for its whole length, so an unbounded Bundle is every other request
  in the process waiting behind it. A larger Bundle is refused with `400`, not
  truncated

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

## The base definitions and terminology are seeded

This build embeds the FHIR R4 specification's own definition and value set
bundles — 4.0.1, as published, gzipped and otherwise unmodified, with their
digests recorded in `packages/conformance/definitions/SOURCE.md`. Startup seeds
every `StructureDefinition`, `ValueSet` and `CodeSystem` into an install-wide
store, so `GET /fhir/R4/ValueSet/observation-status` answers with the set a
refused code was judged against.

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

The definitions and the value sets are also what a write is checked against; see
below.

The fallback is a decision rather than an inference. A canonical resource belongs
to no Project, so there is no row for a compartment, a filter or a projection to
be evaluated against, and it is answered only for a caller whose Grant narrows
none of those. That matters because the Project's own store returns the same
"not found" for a row that is not there and for one the caller may not see — an
id must not be probeable — so reading the fallback off that answer would be
handing resources out on a guess that is wrong exactly when it matters. A
confined caller gets the 404 they would have got anyway.

## There is no LOINC or SNOMED, and that is deliberate

This build resolves no LOINC or SNOMED CT code. A resource may carry one — the
code is stored and read back exactly as sent — but nothing here says what it
means or whether it exists.

There was a directory: an importer that read a release the deployment supplied,
two tables it landed in, and `$lookup` and `$validate-code` served over it. It
was removed, because it cost more than it bought.

What it bought was almost nothing. **Exactly one of R4's 332 required bindings
names a LOINC or SNOMED value set**, so holding a release never made validation
stricter — the value was the directory, not the checking. What it cost was a
licensing question every deployment had to answer for itself (SNOMED CT is
licensed per country and per affiliate, and LOINC has its own terms), a release
file to obtain out of band and keep current, and a code path that had to tell
"this install holds no such system" apart from "no such code" on every lookup.

**The R4 value sets are untouched by this.** They are CC0, embedded in the build,
seeded at startup, and are what a required binding is still checked against; see
above. Removing LOINC and SNOMED removed a directory, not the validation.

A deployment that needs to resolve these codes should reach a terminology server
that is licensed to serve them. That is a different thing from what this server
does, and pretending otherwise was the mess.

### What this argument does not cover

It is an argument about **R4's own bindings**, and it holds for them. It does not
hold for a profile whose bindings name those systems directly, which is what the
ABDM work needs — so the directory is to come back, in the shape the rest of the
field uses: **the machinery in the repository, the content supplied by whoever
deploys it.**

That is what Medplum does, and it is worth naming because it settles what the
open question was really about. Medplum's repository ships a *generator* that
reads a UMLS release the operator downloads under their own licence; the release
itself is, in their own words, "not included in this repository". Their hosted
service does hold the content, and its terms carry the pass-through notices for
LOINC, SNOMED CT, CPT, RxNorm, RadLex and UCUM. Nobody ships the content in an
open repository. What this build got wrong was removing the operations and the
importer along with it.

So the position is not "no terminology". It is **no bundled terminology** — no
release file in this repository, and no licence this project accepts on a
deployment's behalf — with `$lookup`, `$validate-code` and `$expand` served over
whatever the operator did import, and an install holding no release saying so
rather than guessing. The licensing question stays the deployment's, which is the
one part of the removal that was right.

None of this is built yet. Until it is, the paragraphs above describe the server
as it stands.

## A Project defines its own search parameters

A `SearchParameter` stored in a Project changes what that Project can search
by — no restart, no operator step, and no effect on any other Project. The
built-in registry is the floor every Project stands on and none of them can
move: a custom code that shadows a built-in is refused, because shadowing one
would change what an existing query means for everyone in that Project.

**Only what this build can actually apply.** R4 states where a parameter reads
from as a FHIRPath expression and there is no FHIRPath engine here, so an
expression naming one element — `Organization.telecom` — is compiled and
anything else is refused. `Patient.name.where(use='official')`, a union, an
index, a function: refused, not approximated. The same goes for `number`,
`quantity`, `uri` and `composite`, which this build does not index.

That refusal is the point. A parameter that quietly matched something other than
what it says would come back looking answered, and an empty page is exactly what
a correct search looks like — nobody would find out.

A `SearchParameter` that names no element at all claims nothing, so it is stored
and defines nothing. R4 allows one, and documenting a parameter is not the same
as asking this server to answer it.

**A token's two halves are read from the datatype**, not from the definition: a
`ContactPoint` pairs `value` with `system`, a `CodeableConcept` is read one level
in at its `coding`. A definition that paired one coding's system with another's
code is one nobody could tell was wrong.

**A new parameter is true of what was already stored, shortly.** The index is
built on write, so a parameter defined today describes nothing written
yesterday. Defining one enqueues the types it names, and a worker outside every
request claims a type and walks it — claimed, so several replicas share the work
without two of them rebuilding the same index; claimed with a lease, so a
replica that dies mid-walk returns the work instead of stranding it. Until that
pass runs, a search by the new code is accepted and finds only what has been
written since. Rebuilding an index that is already right costs time and changes
nothing, which is what makes the retry safe.

Removing the `SearchParameter` removes what it defined and the rows it indexed.
A stale row is a match for a restriction that no longer exists.

The statement at `GET /fhir/R4/metadata` advertises what *the caller* may search
by, so an identified caller sees their own Project's additions. A request
nothing identified names no Project, so it is told the built-ins.

## A condition names what an interaction is about

A conditional interaction names its target by a search rather than an id, which
is how a client stays idempotent without holding the ids this server minted.
Without it a pipeline that retried a create makes a second record of the same
thing, and nothing downstream can tell which was meant.

- **Create**: `If-None-Exist: <search>`. A match is answered with what is there
  and nothing is written
- **Update**: `PUT /Type?<search>`. Nothing matching is a create, which is what
  `update` does everywhere else here
- **Delete**: `DELETE /Type?<search>`. Nothing matching is answered as done —
  the client wanted none matching, and there are none
- **Inside a transaction**: an entry may state `ifNoneExist`, and a reference
  written `Patient?identifier=…` is resolved to the resource it identifies
  before anything is stored

More than one match is `412` in every case: the client asked about a resource
and there are several, so nothing can be done that is what they meant. No search
at all is `400`, because without one this would act on every resource of a type.

**A condition runs under a grant for *searching* that type**, not for writing
it. Naming a resource by a condition is reading it, so a caller who may write a
type and not search it cannot address one this way — and a condition that
matched something they cannot read would let them act on it, and learn it
exists, through a door the read route does not open.

A conditional create inside a bundle settles before anything is written, like
every other identity: an entry that matched has an identity, so the other
entries' references point at the resource that is there rather than one made
beside it.

## A batch settles every entry on its own

A batch and a transaction are the same request shape and the opposite promise. A
transaction is for things that are only true together — a Patient and their
Observations. A batch is for a day's unrelated writes, where a client wants the
ones that worked to have worked.

Each entry runs inside a **savepoint**. The request already holds a transaction,
so rolling the whole of it back for one bad entry would make this a transaction,
and rolling nothing back would leave half an entry behind. The savepoint is
named by the store rather than by anything in the request: a name goes into the
statement verbatim, and one a request could choose is one a request could write.

A batch answers `200` whatever its entries did, because the request succeeded.
Which entry did not is the answer, and each failed entry carries an
`OperationOutcome` saying why — a status alone names which entry went wrong and
not what about it.

## What this server does not answer about a history

`_since` narrows a history to what changed after a moment, which is what makes a
type's history worth reading as a feed. `_at` and `_list` are **refused**: a
caller who narrowed a history and was handed the whole of it has no way to tell,
and will read it as the answer to what they asked.

**Type-level and system-level history are not implemented**, and the reason is
worth writing down because it is not effort. A version's sequence number is per
resource — `PRIMARY KEY (project_id, res_type, res_id, version_seq)`, and the
schema says there is no global row id — so ordering a type's whole history by it
interleaves by version *number* rather than by time, and a cursor built from one
means nothing across resources. A feed like that would look like it worked and
silently answer out of order. Doing it properly means ordering by
`last_updated` with a tiebreaker and a compound cursor, which is a different
piece of work from widening the query.

## A modifier is a different search from the bare parameter

`name:exact=Ward` and `name=Ward` are different questions. Answering the second
when the first was asked would come back looking answered, so a modifier this
build does not apply is refused by name.

What it applies:

- **`:missing`** asks whether the element is there at all, which no value can
  ask: a resource that never stated a gender and one that stated an unknown
  gender are different facts about a person. It is a yes or a no, not a value
- **`:exact`** matches the value as written, whole. The index keeps a string
  twice for this — folded for the prefix match a bare parameter does, and as
  written for this one
- **`:contains`** matches anywhere in the value. It is the one match here that
  cannot use an index, which is why R4 marks it optional; a search naming it is
  bounded like every other
- **`:not`** excludes the resource rather than the value. A resource carrying
  two identifiers, one of them the excluded code, is one the client asked not to
  see — negating the comparison instead would return it for the other row

What it refuses, and why each needs something built first: **`:above`** and
**`:below`** walk a code system's hierarchy, **`:in`** and **`:not-in`** resolve
a value set, and **`:text`** and **`:of-type`** match parts of an element this
build does not project. A modifier that means nothing for the kind it was put on
— `:contains` on a token — is refused too.

**Chaining, `_has`, `_include`, `_revinclude`, `_sort`, `_summary` and
`_elements` are not implemented**, and refused rather than ignored. So are the
parameter types beyond token, string, reference and date, and the date prefixes
beyond `eq`, `gt`, `lt`, `ge` and `le`.

## An install is claimed once, by whoever holds the host

An install is created with no members. Every route that resolves standing has
nobody to resolve, `POST /auth/login` needs an identity that already exists, and
nothing creates the first one — so until this, a fresh install could not be
brought into use through its own API at all, and no external tool could
authenticate to it.

Startup provisions the Super Project and mints one claim token. It is written to
`claim-token` in the data directory at `0600`, and **not to the log**: a
credential in a log is a credential in every place logs are shipped to, and this
one makes an administrator of whoever reads it. The data directory is the one
place whoever runs the server already holds.

`POST /auth/claim` spends it, creating the first identity and the Super Admin
membership together. It is deliberately not first-signup-wins — whoever reaches
the port first is not who owns the host.

Two things about how it refuses. **Whether the claim can succeed is settled
before anything is written**, because this is the only route an install serves
before anybody can authenticate and one that created an identity for every
request would be a way to fill a database from outside. And **every failure
answers the same `403`**: a token that is wrong, one that expired and one
already spent are the same fact to whoever is holding the wrong one, and saying
which would say whether this install has been claimed.

A restart re-arms nothing. An install that has been claimed mints no second
token, and one that has not is not re-armed by being restarted.

## An outside implementation judges what goes on the wire

The Go suite checks this server against its own reading of R4 — the reading that
wrote the server. It cannot catch the two of them being wrong the same way, and
twice it did not:

- `Bundle.entry.response.lastModified` carried an HTTP-date where R4 declares an
  `instant`. Both spellings name the same moment; only one is the datatype. The
  `Last-Modified` *header* beside it is an HTTP-date and always was correct
- An element written as `{}` was stored. R4 has no empty object — an element with
  no content is absent — and `Observation.code`, which is required, was being
  satisfied by one

Both passed every test here, because the tests asserted the values the server
produced. `./scripts/conformance.sh` now runs the HL7 FHIR validator — the engine
Inferno itself runs — over the bytes each handler actually returns, and fails on
anything it calls an error.

**145 shapes, covering every type this server serves.** One read-back of each of
the 126 declared types, the four whose routes do something particular with them
— a `Binary` written as raw bytes, a `DocumentReference` whose attachment names
one, a `Subscription` whose criteria was parsed, a `SearchParameter` that was
compiled — every Bundle this server produces, and the refusal bodies a caller
can provoke. The two types somebody originally picked were where both defects
above were found; there was never a reason to think the other hundred and twenty
were different, only that nobody had looked.

Findings are accepted rather than fixed only with a reason, and the run prints
how many each reason covers, because a class quietly absorbing findings is how a
gate stops meaning anything:

- **A media type cannot be verified.** R4 binds these to BCP-13, IANA's registry
  rather than a list of codes, and neither this validator nor the public
  terminology server resolves it
- **A composite SearchParameter's own rule**, which this build never reaches: it
  refuses a composite parameter rather than storing one

There were three. The third was FHIRPath invariants, absorbed because this build
evaluated none — `packages/fhirpath` closed that, and the acceptance went with
it. The list is short on purpose: a class quietly absorbing findings is how a
gate stops meaning anything, so an entry leaves the moment it stops being true.

What the validator cannot judge is asserted in Go beside it: that every issue
code this build can emit is one R4 defines — read from the source, so a code
added later is covered the day it is written — that every search entry is marked
a match and names somewhere a client can read it, that paging holds nothing
twice, and that a history says which interaction wrote each version.

Terminology is disabled for the run. A gate that reaches `tx.fhir.org` answers
differently on a day that server is slow, and this build resolves no terminology
of its own to check against anyway. The one check that would need it cannot be
resolved by the public terminology server either.

**What this is not.** It validates representation, not behaviour. It is also not
wired into CI: it needs Docker and takes minutes rather than seconds, so it is
run deliberately and its result is not a gate anything blocks on yet.

Of Inferno's own test kits, every one layers an implementation guide — US Core,
SMART App Launch, Da Vinci, CARIN — on top of R4. **SMART App Launch is now
partly implemented**, so that kit is the first that could apply at all; what it
would still find missing is named under Authentication below. The others remain
out of reach because this build implements none of their guides.

## Invariants are checked, and that is most of what R4 says

R4 states 203 required rules that cardinality and datatypes cannot express.
"An Organization SHALL have a name or an identifier" is not a fact about either
element — both are optional on their own — and R4 writes those as FHIRPath.

**All 203 are evaluated.** `packages/fhirpath` implements the subset they are
written in, which is measurable rather than guessed: thirty functions cover 197
of them, and the rest need eight more. A refusal cites the rule by the name R4
gives it — `org-1` — and then in the specification's own words, because most
people do not read FHIRPath.

What matters as much as the subset is what happens outside it. An expression
this build cannot read is an error, never an empty result and never true. An
invariant reported as passing because nobody could evaluate it is worse than one
nobody checked, because it looks checked. `validate.Unevaluable()` names the
ones this build cannot apply and a test asserts that list is empty, so a rule
that quietly stopped being checked fails the build rather than the next audit.

**Best practice is not a rule.** R4 marks some constraints as guidance with an
extension it puts on exactly those — "a resource should have narrative for
robust management" is true, and is not something to refuse a write over or to
repeat about every resource that ever arrives. This build reads that marking
rather than inventing a line of its own.

## A declared profile is checked, as far as this build reads one

A resource naming a profile in `meta.profile` is claiming to conform to it, and
R4 says it SHALL. So the claim is answered rather than stored unread.

- A profile this install holds has its **resource-level invariants** applied,
  and a write that fails one is refused
- A profile constraining a different type than the resource is refused: an
  Organization claiming the Patient definition is saying something untrue
- A profile this install does not hold is **reported** — "this server does not
  hold that, so it did not check this resource against it" — and the resource is
  stored. Naming a profile from somewhere else is not a client error; letting
  the claim pass unremarked would be this server's

What is not applied, stated plainly because the word "profile" suggests more
than this does: a profile's narrowed cardinality, narrowed types, narrowed
bindings, slicing, and any invariant it attaches below the root. An invariant
sits on an element and is evaluated with that element as its context, and
resolving each one's path through a resource is work this build has not done.

## A reference that leads nowhere is reported, not refused

R4 permits a reference to name something this server does not hold: the target
may live elsewhere, or may not exist yet. This build depends on that itself — a
confined grant names a compartment before the Patient in it exists, which is how
one is provisioned at all.

So `$validate` reports a relative reference that resolves to nothing, and a
write does not refuse one. `$validate` is where a client asks what this server
makes of a resource, and a reference nobody can follow is a record that reads as
complete and is not.

It is followed **under a read grant for the type the reference names**. Anything
else would make this an oracle: a caller who cannot read a Patient would learn
whether one exists by validating a resource that points at it. Under their own
grant, "not there" and "not yours" are one answer — the same one a read gives
them. A caller holding no such grant is told nothing rather than told it is
missing.

Absolute urls, `urn:` references and contained `#` references are left alone.
The first two name resources that are not this server's to resolve, and `ref-1`
already holds the third.

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

- **Required bindings.** An element bound to a value set holds a code from it.
  `"status": "banana"` is refused, and the refusal names the set so a client has
  somewhere to look. **Only required bindings are rules**: an extensible binding
  says a code should come from the set and R4 permits another, and a preferred or
  example one is a suggestion — refusing those would refuse resources the
  specification allows.

**What is still not checked.** No profiles: only the base definitions are read,
and a `StructureDefinition` that constrains one is stored without being applied.
No FHIRPath invariants, and no reference that actually resolves. Those are the
gap between this and a validator somebody should certify against.

**A value set this build cannot work out decides nothing.** 199 of R4's 203
required value sets resolve from the bundled content, covering 319 of 332
required bindings. The rest name terminologies published elsewhere — IANA media
types, UCUM units, a LOINC answer list — and a code in one of those is unchecked
rather than refused. A set is resolved only when every part of it is: an
exclusion, a filter, a nested value set or a code system whose own definition is
incomplete makes the whole set unresolved, because a set half worked out would
refuse codes that are in it.

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

### An app authorizes through SMART App Launch

`/oauth2/authorize` and `/oauth2/token` serve the standalone launch. A client
registers the addresses a code may be returned to and whether it keeps a secret;
a person approves; the code comes back on a registered address and is exchanged
for an access token.

**The consent screen is not this server's.** `GET /oauth2/authorize` is the
browser endpoint SMART specifies, and what it does is redirect to the page named
by `ILAVRITA_CONSENT_URL`, carrying the request. That page reads
`GET /oauth2/consent` for what would be granted and what is refused, and posts
the approval back. A deployment that configures no such page authorizes nobody,
and says so rather than redirecting nowhere.

The endpoint resolves neither the client nor the redirect address: a browser
arriving from an app carries no session, so the Project is not yet known, and a
client id is unique within a Project rather than across the install. Those checks
happen at `/oauth2/consent`, once somebody has signed in — the Project is
whoever they are.

**An access token is a session.** There is no second kind of bearer credential
here, which is why what an app reaches is narrowed per request against whatever
the policy says then, rather than against a copy taken when the token was minted.
`docs/design/smart-scope-spec.md` is the whole of that argument; the short form
is that a SMART scope narrows and never widens.

PKCE with S256 is required of every client, public and confidential alike.
`plain` is not implemented — a challenge equal to its verifier protects against
nobody who intercepted the code.

An approval naming `offline_access` or `online_access` also receives a refresh
token, rotated on every use. A spent one presented again is a copy somebody else
is holding, so the grant dies: every rotation of it, and every session it minted.

**What SMART App Launch does not do here:**

- **No backend services.** `private_key_jwt` is not implemented, so `system/`
  scopes are refused rather than granted. `packages/project/jwks.go` reads a
  registration's public keys and nothing uses it yet
- **No identity token.** `openid`, `fhirUser` and `profile` are refused by name
  rather than granted, because granting them is a promise: a client that asked
  for `openid` would look for an `id_token` and find nothing
- **No EHR launch.** The `launch` parameter is read as the patient a session is
  launched for, not as an opaque handle an EHR issues and this server resolves.
  The `launch` and `launch/encounter` scopes are refused by name: there is no
  encounter in a token response, and a scope honoured in name only is worse than
  one plainly refused
- **No token introspection or revocation endpoint.** A token expires, or the
  session behind it is revoked through the session routes

Each absence is visible rather than silent: `grant_types_supported` names only
what the token endpoint answers to, and a scope this server will not grant is
reported in `refused` with its reason before anybody depends on it.

**What is still missing:**

- **Session refresh for a password login.** A person's own session expires and
  the credential is proved again. Refresh exists for an app's session, not for
  the login that issued it.

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

**There is no PocketBase superuser, and this build cannot make one.** A
superuser is not one of this server's principals: it holds no Project, no
Membership and no Grant, so nothing it does is narrowed by a compartment, a
filter or a projection, and nothing it does reaches the audit trail. The console
it unlocks can take a backup of the whole data directory, and that archive
carries every Project's clinical record out with it — which is the whole of the
isolation model, undone by one account outside it.

Two things hold that, because either alone is a door. The command that mints one
is never registered, which is why `main` calls PocketBase's `Execute` rather than
`Start` and why a test refuses any source file that calls `app.Start`. And every
row in `_superusers` is deleted at startup, before the server answers anything —
on every start, not once, because the table is reachable by things that are not
this process: an older binary, a second PocketBase run against the same
directory, a restored archive. A start that removed one says so in the log and
records it in `super_jobs`.

Administering this install is Super Admin, which is a standing inside the model:
decided per request, bounded by the same Scope compilation as everything else,
and recorded like any other interaction.

A start that cannot read `_superusers` is a failure, not a warning. Not being
able to tell whether the account exists is not a state to serve clinical data
in.

## Cross-origin requests

The FHIR surface sends no CORS headers and answers no preflight, so no browser
on another origin can read from it. The runtime's own default — allow every
origin — is withdrawn beneath `/fhir/R4` and left in place everywhere else. A
browser-based client needs a proxy on its own origin until a configurable policy
exists.

## Operational consequence

**Do not put patient data in Ilavrita.** There is no authentication and no audit
trail. Use synthetic data only.
