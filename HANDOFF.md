# Handoff

Ilavrita as it stands, for whoever picks it up next. Written to be read before the code.

## 1. What actually works

The distinction that matters is not "what code exists" but "what a request can reach".
Plenty of correct code here is not yet reachable, and conflating the two has been the
most expensive mistake on this project so far.

| Surface | State |
| --- | --- |
| `GET /healthz`, `GET /version` | Working |
| `GET /fhir/R4/metadata` | Working, R4-valid, generated from the routes actually served |
| create, read, vread, update, delete, history-instance | Working, for 126 resource types: 64 non-clinical, 62 clinical |
| `GET /fhir/R4/{type}?...`, `POST /fhir/R4/{type}/_search` | Working, over the built-ins plus what the Project defined |
| `SearchParameter` written by a Project | Working: compiled, indexed, and backfilled by a claimed worker |
| `POST /fhir/R4` with a `transaction` Bundle | Working, all-or-nothing |
| `POST /fhir/R4` with a `batch` Bundle | Working, every entry on its own inside a savepoint |
| Conditional create, update, delete | Working, under a grant for searching the type named |
| Conditional read, `Prefer: return=` | Working |
| `POST /fhir/R4/{type}/$validate` | Working: base definitions, required bindings, R4's invariants, declared profiles, dangling references |
| `ilavrita backup`, `verify-backup`, `restore` | Working |
| Migrations, seeds and backfills | Idempotent, and recorded in `super_jobs` against the table |
| Everything else under `/fhir/R4` | `501` |
| `POST /auth/login`, `POST /auth/logout`, `GET /auth/session` | Working |
| `GET`/`POST /oauth2/authorize`, `POST /oauth2/token` | Working: standalone launch, PKCE S256 required, rotating refresh |
| `GET`/`POST /oauth2/consent` | Working: this server's own API for the consent page a deployment supplies |
| `POST /oauth2/token` with `client_credentials` | Working: `private_key_jwt`, RS384 and ES384, which is what `system/` scopes need |
| `GET /oauth2/jwks`, `GET /.well-known/openid-configuration` | Working where a sealing key is configured; `404` where none is |
| `/admin/projects` and the surface beneath it | Working, for a Super Admin or the Project's own admin |

**126 resource types are served** — 64 non-clinical and 62 clinical, out of R4's 146. The two families are reached
differently and that difference is the safety property: a non-clinical type may be granted
outright, and a clinical one only through a compartment. A create is checked against the
compartments the submitted resource itself declares, derived from its own references
(`fhir.Compartments`). Confinement is read one dimension at a time: a grant naming `Patient/x`
governs every Patient compartment the resource lands in and says nothing about the Practitioner or
Encounter it also names, because refusing those would refuse nearly every real observation.

**Authentication works.** `POST /auth/login` proves an argon2id password and issues a session;
`Authorization: Bearer <token>` names the caller on every later request. A FHIR route answers
`401` to anything else. There is no development principal and no environment variable that names
one: a request authenticates or it reaches nothing.

Do not deploy this anywhere near patient data yet. The audit trail, the second factor and its
recovery path, the login throttle, search, backup and restore, validation against a resource's own
definition, and SMART App Launch all exist now. What is missing is a migration story between
releases and **any review of this by somebody other than us** — which is the one that matters,
because every test saying a boundary holds was written by whoever wrote the boundary. Inferno
judges protocol behaviour and found three real defects doing it; it does not judge whether a Scope
leaks.

A `client_application` or `bot` development principal now also needs a registered, active row in
its Project, and an id carrying the `cli_` or `bot_` prefix. An id outside the namespace stops the
process; an unregistered one answers `401` to everything.

## 2. The shape of the system

PocketBase is the runtime, not the product. Everything Ilavrita publishes — routes, error
shapes, versioning, tenancy — belongs to Ilavrita and sits behind its own interfaces.

```
HTTP (apps/ilavrita)
  -> principal -> authz.BuildScope -> storage.Scope
  -> packages/storage interfaces
  -> packages/storage/pocketbase (the only package that may import PocketBase)
  -> SQLite
```

Four rules hold it together. The first is enforced by `depguard`, not by review:

1. Only `packages/storage/pocketbase` imports the PocketBase runtime.
2. No SQL above the storage backend.
3. No PocketBase concept reaches `/fhir/R4`. Its own `/api` and `/_` surface is disabled
   unless `ILAVRITA_EXPOSE_POCKETBASE=true`.
4. Nothing is advertised in the CapabilityStatement without a passing test behind it.

## 3. Decisions that are expensive to relearn

Each of these was reached by having the first answer broken. The reasoning is worth more
than the code.

**A Project is the isolation boundary, and `Scope` leads every storage method.**
`Read(ctx, scope, key)`, not `Read(ctx, key)`. A parameter cannot be forgotten; naming a
Project in a key is not proof of entitlement. Three separate critical findings routed
through that one hole.

**Linked Projects, not a hierarchy.** No parent/child inheritance. With a tree, every read
path added later — search, GraphQL, bulk export, subscriptions — inherits the widening for
free and must remember not to. Links are directed, non-transitive, and grant nothing unless
active, unexpired and approved by both sides. "Child project" is an organisational
attribute that confers no data access.

**Administrative capability cannot mint data grants.** An early design gave an
administrative link the power to create data grants. It held no data itself, which is not
the same thing. FR-052 is a database invariant now, not a naming convention.

**Platform resources live in separate tables from FHIR resources.** With one table and a
`kind` column, a forgotten predicate leaks Login and AccessPolicy rows into a Patient
search. Separate tables make that unrepresentable.

**Super Admin is an FK-guarded column on a membership in a `kind='super'` project.**
Holding it anywhere else is a constraint violation. Bootstrap is a single-use token, never
an env var and never first-signup-wins.

**There is no PocketBase superuser, and the binary cannot make one.** Such an account holds no
Project, no Membership and no Grant, so it sits outside every control this project has, and the
console it unlocks can archive the whole data directory. `main` calls PocketBase's `Execute`
rather than `Start`, so the `superuser` command is never registered, and `refuseSuperusers`
deletes every `_superusers` row at startup — on *every* start, because that table is reachable by
an older binary, a second PocketBase run against the same directory, or a restored archive. Two
tests hold it: one plants rows and starts, one refuses any source file that calls `app.Start`.

## 4. How to verify anything here

**A passing test suite proves nothing on its own.** Mutate the code and confirm the tests
fail. Every claim below was checked this way:

```bash
# drop the project predicate in packages/storage/pocketbase/resource.go
#   -> TestDroppingTheProjectPredicateCrossesProjects fails
# make Scope.Allows return true unconditionally
#   -> TestAssertInScopeRejectsARowFromAnotherProject fails
# remove the !stands(...) gate in packages/authz/scope.go
#   -> TestADeadMembershipReachesNoGrantor fails
# make UserStore use s.db instead of conn(ctx, s.db)
#   -> the rollback test deadlocks for 600s on SQLite's write lock
```

Gates, all of which must pass:

```bash
gofmt -l apps packages
go build ./... && go vet ./... && go test ./...
golangci-lint run
./scripts/verify-openapi.sh   # fails on drift in BOTH directions
./scripts/conformance.sh      # the HL7 validator over what the handlers return
make ci-local                 # the workflows, via act
```

`conformance.sh` needs Docker and is the one gate that does. It is also the only
one that is not this project marking its own homework, which is why it found two
defects a green suite had been reporting as correct.

## 5. Traps that cost real time

None of these are visible from reading the code.

- **PocketBase prints a superuser token.** Its installer mints a live 30-minute superuser
  credential, opens a browser and prints the token to stdout. Disabled via
  `serve.InstallerFunc = nil`. Never re-enable it.
- **PocketBase serves its own `/api` and `/_`.** Registering your routes does not replace
  them. Blocked by a router middleware; `/api/files/…` was the reachable path to the image
  decoder CVE.
- **Ilavrita has its own database file.** PocketBase's init migration owns the table name
  `users`, so the schema goes to `<DataDir>/ilavrita.db`, opened with the runtime's own
  connect function so the pragmas cannot drift.
- **`PRAGMA foreign_keys` is per-connection.** Every composite-FK guarantee is decorative
  without it. `AssertForeignKeysEnforced` refuses to apply the schema otherwise. Checking
  it with a second connection proves nothing.
- **`act` needs the custom runner image.** `actions/setup-go` drops node from `PATH`,
  breaking every later JavaScript action. `make ci-image` fixes it.
- **SQLite's `LIKE` is ASCII-case-insensitive.** `id LIKE 'cli\_%' ESCAPE '\'` accepts
  `'CLI_x'`; `substr(id, 1, 4) = 'cli_'` does not. Every namespace CHECK uses `substr`.
- **`PRAGMA foreign_keys` is a no-op inside a transaction, and `PRAGMA foreign_key_check`
  raises rather than returning rows** when a parent index is missing — so `rows, _ := Query(...)`
  reads the worst outcome as a pass. Both shaped the membership rebuild in `migrate.go`.
- **`ALTER TABLE ... RENAME` re-validates triggers on *other* tables that name the renamed one.**
  The rebuild drops and restores them, or the rename fails outright.
- **TypeScript 7 broke `openapi-typescript`.** Its native compiler does not expose
  `ts.factory`. SDK types are hand-written; drift is caught by `verify-openapi.sh` instead.
- **`modernc.org/libc` ships glibc-derived LGPL-2.1 headers** that compile into the Linux
  image. Do not record a BSD-3-Clause licence conclusion for it. See
  `docs/license-compliance.md`.

## 6. Known gaps

- **Compartment derivation reads top-level elements only.** `Appointment` links through
  `participant.actor`, `Person` through `link.target`, `Provenance` through `target` and
  `agent.who`, so this build cannot place one, so none is served: a clinical resource landing in
  no compartment is reachable by no confined grant. Twelve of the twenty undeclared R4 types are
  there for this reason; `TestEveryPlaceableTypeIsOneThisBuildServes` keeps the two lists honest.
- **Five R4 types are unclassified.** `Basic`, `BiologicallyDerivedProduct`, `DeviceMetric`,
  `ResearchStudy` and `VerificationResult` are on neither list, so they are treated as carrying
  patient data and are not served. That is the safe answer rather than the finished one.
- **A compartment subject is created by naming it.** A `POST /Patient` mints an id no confined
  grant can name in advance, so it is refused; `PUT /Patient/{id}` under a grant naming that
  patient is how one is provisioned. Correct, and surprising the first time.
- **A profile is checked as far as its root.** The R4 base definitions and value sets are
  embedded, seeded at startup and read by the validator, and all 203 of R4's required FHIRPath
  invariants are evaluated by `packages/fhirpath`. A resource naming a profile in `meta.profile`
  has that profile's resource-level invariants applied, and one this install does not hold is
  reported rather than passed. What is *not* applied is a profile's narrowed cardinality, narrowed
  types, narrowed bindings, slicing, or any invariant it attaches below the root — an invariant is
  evaluated with its own element as context, and resolving each one's path through a resource is
  work nobody has done here.
- **No LOINC or SNOMED, at all.** There was a directory — an importer that read a release a
  deployment supplied, two tables it landed in, and `$lookup` / `$validate-code` over it — and it
  was removed deliberately. It bought almost nothing: exactly one of R4's required bindings names
  a LOINC or SNOMED value set, so it never made validation stricter, and against that it cost a
  licensing question per deployment, an out-of-band release file to obtain and keep current, and
  a code path that had to distinguish "this install holds no such system" from "no such code".
  A code in one of those systems is carried and stored; nothing here resolves it or checks it.
- **A resource that nests past six levels of its own kind is unchecked there.** R4 lets an element
  hold its own kind and a snapshot cannot write that out, so the model expands it to a bound. Past
  it the content is not walked at all, because an element with no children in the model would have
  everything inside it refused as undeclared.
- **An install with one administrator who loses their phone has no way back through the API.**
  `DELETE /admin/projects/{project}/users/{user}/second-factor` is the recovery path, and it
  refuses the caller's own factor: an administrator who could reach around the code requirement
  with their own session would make a stolen administrator session enough to disable MFA. Recovery
  is something somebody else does for you, and a single-administrator install has nobody else.
- **The control plane is a working subset, not the whole surface.** It creates Projects,
  invites identities, grants standing and registers client applications. AccessPolicy authoring,
  link management, credential rotation and every list endpoint are still store-only.
- **An install claims itself once, through `POST /auth/claim`.** Startup provisions the Super
  Project and mints one token, written to `claim-token` in the data directory at 0600 — not to the
  log, because a credential in a log is a credential wherever logs are shipped and this one makes
  an administrator. The claim creates the identity as well as the membership, because there is no
  other way to have one. Whether it can succeed is settled *before* anything is written: this is
  the only route an install serves before anybody can authenticate, and one that wrote an identity
  per request would be a way to fill a database from outside. A restart re-arms nothing.
- **Attachments outside DocumentReference land in the row.** `Media.content`,
  `DiagnosticReport.presentedForm` and `Communication.payload` carry bytes into the resource row,
  bounded only by the 4 MiB one request body may be. `DocumentReference` is refused and sent to
  `Binary` because R4 gave its attachment a url; the others have nowhere to be sent.
- **Linked Projects resolve but nothing names a grantor.** `LinkStore` reads them and
  `BuildScope` compiles their Grants; a by-key route names no grantor, so the reach is exercised
  by tests and not yet by any request. The search route is what will name one.
- **Bots are identity only.** The table, the domain type and the foreign key exist so the
  membership column is constrained; what a bot *runs* is Phase 3 and nothing executes one.
- **The release pipeline has never run.** Signing, SBOM and provenance are configured and
  unexercised. Cut `v0.0.1-rc.1` first, deliberately.

## 7. Next step

Agreed direction, in this order. The order is the point: each item removes the reason the
next one is currently unsafe.

**1. Client applications and service accounts (FR-053). Done.** The registries, the domain
types, the credential lifecycle and the foreign keys landed; `docs/design/client-application-spec.md`
is the contract, numbered `CAP-n`. What was actually closed: a membership could name a client
application or bot that no row described, and it resolved to full standing with no lever anywhere
to withdraw it, because only `user_id` carried a foreign key and only a user principal was gated on
its registry. Both are now constrained, in the database and in the resolver. Credentials mint,
rotate and revoke; **nothing authenticates anyone**, which is step 2 and is drawn as a physical
line: no projection in `packages/storage/pocketbase` selects `secret_hash` (CAP-22).

**2. Real user authentication. Done.** `POST /auth/login` proves an argon2id password against
the Project a slug names, resolves the standing that identity holds there, and issues a session
that pins both. A later request carrying `Authorization: Bearer <token>` is served as that
principal; `POST /auth/logout` destroys the material. A wrong password and an unknown address
are the same answer. A revoked membership stops an existing token reaching without waiting for
it to expire.

**2a. Retire `ILAVRITA_DEV_PRINCIPAL`. Done.** It is gone: the constant, the parser, the startup
warning and the struct field. `backend.sessions` is an interface so a test can decide what a token
means, and a backend wired with no session port identifies nobody rather than panicking. The
conformance suite carries a bearer token and is served as a user principal, because a session is
what a password login issues and a password belongs to a person.

**2b. Compartment determination at create. Done.** `fhir.Compartments` derives which subjects a
submitted resource belongs to; `ResourceRecord` carries them; `ResourceStore.Create` authorizes a
confined write against them and projects them into `fhir_resource_compartment`, which nothing had
ever written before. A confined caller that collides with an id in another compartment is answered
`403` rather than `409`, so a create cannot be used to probe which ids exist.

**3. Access policies. Done.** A rule now narrows three ways and none of them can widen it. A
**filter** (`POL-1`..`POL-5`) restricts which resources a Grant reaches by an element's value:
`status = final`, `category.coding.code in (vital-signs, laboratory)`. It runs inside the query,
never over rows already fetched, and the same compiled predicate is what a write is checked
against — so a caller cannot write a resource its own filter would then hide from it. A
**projection** (`POL-6`..`POL-8`) restricts how much of a resource comes back, decided per row
against the Grants that actually reach that row, so a clinician holding one patient in full and
another's status alone cannot read the second in full. A **subject set** (`POL-9`) names a roster
in one rule instead of one rule per member.

Two things fell out of it. A caller that reads part of a resource may not replace all of it: an
update replaces content wholesale, so the ordinary read-modify-write would silently drop what
their own policy withheld. And an update never re-placed a resource in the compartments it
states — the route layer derived them and storage discarded them, so the patient a resource had
moved away from went on reading it. Both are fixed and both have named regression tests.

`docs/design/policy-audit-search-plan.md` carries the numbered rules, the deviations, and the
findings a reimplementation should not have to rediscover.

**3a. Audit. Done.** Every FHIR interaction and every login is recorded, in the transaction that
performed it, by one decorator rather than by each handler — and the answer is held back until
that record commits, so nothing this server told a client is something it cannot account for. An
interaction that writes and then refuses rolls back and is recorded afterwards, because a refusal
that vanished with the rollback would leave only successes in the log. A refused login names
nobody and no Project: recording the user it found would say which of the four checks got that
far, turning the trail into the address oracle the uniform answer exists to prevent.

**3b. Search. Done.** `GET /{type}?...` and `POST /{type}/_search` are the same interaction and
resolve identically. A query is parsed against a registry of what this build actually implements;
anything else is refused with `400` rather than ignored, because a search that silently drops a
criterion returns more than it was asked for and the caller cannot tell. Values are projected
into `fhir_search_index` on write, in the transaction that writes the resource, exactly as
compartments are — and an install that predates the index is backfilled once, or it would come up
answering "no matches" for data that is plainly there. Results are a `searchset` Bundle: `next`
only when a page follows, `total` only when `_total=accurate` asked for one.

Search is its own action. A Scope may let a clinician read any chart they are handed the id of
and search only their own patients, so the read Grant is never compiled into a search.

**4. Binary payloads. Done.** A `Binary` is served, and its bytes live outside the row that
describes them: a row carrying megabytes would make every read of the metadata pay for them and
every backup carry them. Send the document under its own media type with `X-Security-Context`
naming what governs access to it, or send it as a resource with `data` base64-encoded — either
way the bytes land in the same place and the row records what they are. Reading it back answers
the document when the `Accept` names its media type and the resource otherwise; `*/*` answers the
resource, because every other route does.

`securityContext` is what places a Binary in a compartment. R4 puts a Binary in none of its own —
it is bytes, and what they are about is only knowable from whatever points at them — so a Binary
naming nothing lands nowhere and a confined caller cannot write it. That is the right answer for
an unattributed document in a clinical server.

**5. Subscriptions. Done.** A write records one row saying it happened; a worker outside every
request turns that into the notifications it owes. Matching inside the write would make each
write cost as much as the subscription list is long.

What a subscriber is told is decided by running *their own criteria under their own Scope*, so
matching and authorization are one question rather than two that could disagree. A subscription
cannot be a way around a Scope, and standing withdrawn is a subscription that stops delivering
rather than one that goes on delivering as somebody who is no longer there. The queue carries the
resource's key and never its content, so access withdrawn between the write and the notification
is access the notification does not have.

`rest-hook` posts to the URL a subscriber registered, retrying six times over about half an hour.
The address is theirs and the request is this server's, so the dialer refuses loopback, private
and link-local addresses — otherwise registering a subscription would be a way to make this
server reach anything it can, cloud metadata included. `ILAVRITA_ALLOW_PRIVATE_HOOKS=true` opts a
development deployment out.

`websocket` pings whoever is bound, over `GET /fhir/R4/ws`. A subscriber carries its session
token in the `ilavrita.session.<token>` subprotocol, because a browser can set nothing else on a
WebSocket and a token in the URL ends up in every access log. It may bind only to its own
subscription: knowing when somebody else's fires is knowing something about the data behind it.

**PocketBase's realtime is not this.** It is Server-Sent Events, not WebSockets, it lives under
`/api/` which this build blocks, and it knows nothing about a Scope. None of it was reusable.

The socket channel is best-effort by design. A deployment running several replicas has each
subscriber connected to one of them, and a notification worked out on another has nobody there to
tell — recorded as never delivered rather than retried, because retrying would not move it to the
replica holding the socket. `rest-hook` is the channel that survives that.

**A socket does not outlive its session.** Every other route proves a token on each request; a
socket is authorized once and then held, so the session it bound under is asked after again — on a
timer, and before each ping. A logout closes it within thirty seconds and nothing is delivered
over it in between. It holds no token to re-present and must not: a credential kept for the life
of a connection is one a crash dump carries, so what it holds is which session it was.

Every type this build advertises is already classified: 64 are non-clinical, so an unrestricted
rule may cover them, and the other 62 all derive compartments, so a confined rule can reach them.
`TestEveryAdvertisedClinicalTypeCanBePlaced` is what keeps that true. Classifying is therefore
what it costs to advertise the *next* type, not a gap in the ones already served — and the
classification is the whole of the work: `TestInteractionLifecycle` runs all six interactions
against every advertised type, so a type with a wrong compartment element fails the suite rather
than reaching a deployment.

**6. Second factors and the login throttle. Done.** An identity may enrol a TOTP factor at
`POST /auth/mfa`; it is pending until a code proves it, so nothing can put a factor between a
person and their account except their own phone. The code is carried with the password rather
than asked for afterwards — a server that answered "now the code, please" would be saying the
password was right, and would say it to anyone who guessed an address that exists. A refused
code, a wrong password and an unknown address are one answer.

**Replacing a factor needs a code from the one it replaces.** This was a hole and is worth
stating plainly: re-enrolling and switching the factor off are the same request, so without a
code, a stolen session alone was enough to disable MFA — exactly what a second factor exists to
survive. The rule was already applied to withdrawal and was simply missing next to it.

The factor in force stays in force until the new one is proved, so moving to a new phone never
leaves the account without one. Enrolling where nothing is in force still needs no code: an
unproved factor protects nobody, and asking for one would strand whoever's first attempt went
wrong.

The cost of that rule is that **a lost phone has no self-service way back**. That is the right
trade — a self-service bypass is the hole — but the administrator path that should sit beside it
does not exist yet.

The secret is sealed with `ILAVRITA_SEALING_KEY` (32 bytes, base64). Unlike a password it cannot
be hashed: the server computes the same code the phone does, so whatever holds it holds the
factor. Sealing means a leaked database file is not a list of everyone's second factor. A
deployment that configured no key holds no factors rather than storing them in the clear.

Login attempts are now counted across the install rather than within one process — three replicas
would otherwise allow three times the guesses the limit states. The keys are digests: a table of
who tried to log in and failed is a list of this install's users and where they were.

**7. Bundle transaction. Done.** `POST /fhir/R4` performs a `transaction` Bundle as one act,
inside the commit boundary the audit decorator already opens around the route — so an entry that
fails takes every row, audit record and queued notification with it.

Two things about it are worth knowing before changing it. **Every identity is settled before any
entry is performed**: R4's order is deletes, then creates, then updates, so resolving references
as the entries ran meant an entry created early could not name one updated late, which is the
ordinary case. And **entries are given a throwaway slot for the resource they settle on**, so they
do not overwrite the one the decorator uses for the transaction's own audit row — otherwise that
row names whichever entry happened to run last and claims the transaction was about it.

Every entry is decided and audited on its own. A Bundle is not a way to perform an interaction the
caller could not have performed one at a time, and `TestEveryEntryIsDecidedOnItsOwn` is what keeps
that true. Bundles are bounded at 200 entries because one commit holds this process's single
pooled connection for its whole length.

**8. Conformance against an outside implementation. Done.** `./scripts/conformance.sh` runs the
HL7 FHIR validator — the engine Inferno runs — over the bytes each handler returns, and fails on
anything it calls an error. It found two defects immediately, both of which every test here had
been asserting as correct: a `lastModified` written as an HTTP-date where R4 declares an
`instant`, and an element written as `{}` where R4 has no empty object.

That is the lesson worth keeping: a suite written by whoever wrote the server cannot catch the
two of them being wrong together. Ask something that did not.

Two findings are accepted rather than fixed and both are named with a reason in
`scripts/conformance.py`; one of them, `org-1`, is the real gap that this build checks no
FHIRPath invariant, and the fixture is left invalid so it stays visible.

**9. Custom search parameters and reindexing. Done.** A `SearchParameter` stored in a Project is
compiled into the same shape the built-in registry produces, projected into `search_parameter` in
the transaction that writes the resource, and read back by every seam that decides what a
parameter means — what a write projects, what a query may name, what the statement advertises,
what a subscription may watch. They read one set, so a Project cannot have a parameter it can
search by but not index.

Two things are worth knowing before changing it. **The expression is not FHIRPath.** There is no
engine here, so only an expression naming one element compiles and everything else is refused —
approximating would produce a parameter that matched something other than what it says, and an
empty page is what a correct search looks like. **A token's halves come from the datatype**, not
from the definition, so a `CodeableConcept` is read one level in at its `coding`.

Reindexing is a backlog, not part of the write: a Project's whole Organization table is not work
to do inside the request that defined the parameter, because one pooled connection per process
means that request is every other request waiting. Defining enqueues one row per type — one row,
however many parameters name it — and `reindexer` claims and walks it. The claim has a lease, so a
replica that dies returns the work; rebuilding an index that is already right changes nothing,
which is what makes that safe.

**10. FHIRPath invariants, declared profiles and reference integrity. Done.** `packages/fhirpath`
evaluates the subset of FHIRPath R4's invariants are written in — measured rather than guessed:
thirty functions cover 197 of the 205, eight more cover the rest. All 203 the model holds are
parsed, and `TestEveryInvariantR4StatesCanBeRead` is what keeps that true.

Two rules in that package are worth keeping. **An expression it cannot read is an error**, never
an empty result and never true: an invariant reported as passing because nobody could evaluate it
is worse than one nobody checked, because it looks checked. And **`Holds` fails only on an explicit
false** — an expression that produced nothing decided nothing, and refusing on that would refuse
resources nobody showed were wrong.

Writing it found eleven types whose own fixtures violated R4, exactly the eleven the HL7 validator
had been flagging, and the conformance run's accepted findings fell from 33 to 15 because the
invariant class is now enforced rather than waived.

Best-practice constraints are not applied. R4 marks them with an extension it puts on exactly
those, and `dom-6` on every resource ever written is noise that buries the rest.

**11. The REST behaviour R4 expects, most of it. Done.** Conditional create, update and delete;
conditional references and `ifNoneExist` inside a transaction; conditional read answering `304`;
`Prefer: return=`; `_since` on a history with `_at` and `_list` refused rather than ignored; and
`batch`.

Two things in that are worth knowing before changing them. **A condition runs under a grant for
searching the type it names**, not for writing it — naming a resource by a condition is reading
it, and running the search under the write grant compiles a Scope that authorizes no search and
matches nothing, which reads exactly like a condition nobody had met. That bug was written and
caught here twice, once for conditions and once for reference integrity.

And **a batch entry runs inside a savepoint**, which is what `WithinSavepoint` is for. The request
already holds a transaction, so rolling the whole of it back for one bad entry would make it a
transaction and rolling nothing back would leave half an entry behind.

**Type-level and system-level history are not done, and not for want of effort.** `version_seq` is
per resource, so ordering a type's whole history by it interleaves by version number rather than
by time and a cursor built from one means nothing across resources. It was written, found to page
wrongly, and reverted rather than shipped looking like a feed. Doing it properly means ordering by
`last_updated` with a tiebreaker and a compound cursor.

Search parameters are deliberately a short list — `packages/search/registry.go` is the
whole of what this build answers, and adding one means adding a projection a write maintains and
a predicate a read compiles.

**12. SMART App Launch, all three ways in. Done.** A standalone launch, backend services, and an
identity token. `docs/design/smart-scope-spec.md` holds how a SMART scope becomes a
`storage.Scope` and `docs/design/smart-endpoints-spec.md` holds the endpoint decisions; what
follows is only what costs time to relearn.

**The consent screen is not this server's.** `/oauth2/authorize` validates the shape and redirects
to whatever `ILAVRITA_CONSENT_URL` names, because a consent page has to be styled, translated and
kept accessible by whoever deploys it. That page reads `GET /oauth2/consent` for what would be
granted and what is refused and why, and posts the approval to `POST /oauth2/consent`. The pair is
this server's own API, which is why it is not under the authorization endpoint: that endpoint
belongs to the app and this pair belongs to the page. `scripts/consent` is a test-only page that
approves everything without asking anybody, and says so on every start.

**The authorization endpoint answers GET and POST alike**, because SMART requires both and an app
picks which. A form request answers `303` rather than `302`, since the page being redirected to is
a `GET` and only `303` requires that change of method.

**A backend service's Project is decided by its signature, not claimed.** A client id is unique
within a Project rather than across the install, so every registration bearing the claimed id is
fetched and the assertion verified against each one's keys; two that verify is refused as
ambiguous rather than resolved by picking one. Each `jti` is spent once and kept until it expires.

**An identity token is RS256** — narrower than the RS384/ES384 a client assertion is verified
with, because SMART requires RSA SHA-256 of this token by name. They are separate choices about
separate tokens and neither implies the other. The signing key is minted on first need, sealed
under the same configured secret as everything else, and one row is active at a time — a partial
unique index makes a second active key unrepresentable, and `EnsureActive` re-reads after
inserting so a process that lost the race signs with the key that landed rather than the one it
minted.

**13. Conformance against Inferno. Done, and it found three things.** `scripts/seedlaunch` seeds a
Project, an identity, a policy and a registration through the real stores; with `scripts/consent`
that is enough to drive Inferno's SMART App Launch kit end to end. Everything passes except the
TLS checks, which a plaintext local run cannot satisfy.

It was worth running. The token endpoint answered without `Cache-Control: no-store`, which RFC
6749 section 5.1 requires of any response holding a token. The authorization endpoint read only a
query, where SMART requires a form as well — and the Go suite had never sent one, because it was
written against the same reading of the specification that wrote the handler.

The third is the one to remember. **An approval carrying `offline_access`, `online_access` or
`launch/patient` produced a token that failed every FHIR request with a server fault**, because
those scopes name no resource type and `authz.ParseLaunch` tried to read each as a restriction. It
had been that way since refresh was built. Nothing caught it because every test holding such a
token asserted a *refusal* — and a `500` satisfies "the write was refused" exactly as well as a
`403` does. `authz.NarrowsNothing` is the closed list that carries them and ignores them, and a
scope absent from it still fails the launch rather than being dropped, because a dropped
restriction is a widening. **A negative assertion cannot tell a refusal from a fault. Where the
point is that something is permitted, assert the success.**

## 8. Conventions


One-line Conventional Commits, small and atomic. Comments explain the code in at most
three lines and never narrate a roadmap. Plans go in `ROADMAP.md`, design reasoning in
`docs/adr/` and `docs/design/`.
