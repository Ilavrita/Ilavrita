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
| create, read, vread, update, delete, history-instance | Working, for 66 resource types: 38 non-clinical, 28 clinical |
| Everything else under `/fhir/R4` | `501` |
| `POST /auth/login`, `POST /auth/logout`, `GET /auth/session` | Working |
| `/admin/projects` and the surface beneath it | Working, for a Super Admin or the Project's own admin |

**66 resource types are served** — 38 non-clinical and 28 clinical, out of R4's ~145. The two families are reached
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

Do not deploy this anywhere near patient data yet: no audit trail, no MFA, no rate limit on the
login route, and no search.

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
make ci-local                 # the workflows, via act
```

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

- **No search.** The largest remaining piece and the PRD's own top risk (R-001).
  `packages/search` is a doc comment. `storage` has no `Search` method.
- **Compartment derivation reads top-level elements only.** `Appointment` and `Provenance` link
  through a nested path (`participant.actor`, `target`), so this build cannot place one, so neither
  is served: a clinical resource landing in no compartment is reachable by no confined grant.
  `TestEveryPlaceableTypeIsOneThisBuildServes` keeps the two lists honest.
- **AccessPolicy carries no filter and no field restriction.** "Share only `status=final`
  Observations" and "share an Observation without its note" are both unrepresentable. A set of
  compartments *is* expressible — `Compile` emits one Grant per rule — but at one rule per
  compartment, so a clinic-wide role's roster is its row count. `docs/design/policy-audit-search-plan.md`
  specifies all three, and why they land before search.
- **A compartment subject is created by naming it.** A `POST /Patient` mints an id no confined
  grant can name in advance, so it is refused; `PUT /Patient/{id}` under a grant naming that
  patient is how one is provisioned. Correct, and surprising the first time.
- **No audit trail and no MFA.** Nothing records that anyone authenticated. The login route is
  rate limited now — five failures per identity, twenty per address, fifteen-minute window,
  checked before argon2id runs — but the counter is per process, so it is a limit rather than a
  guarantee behind more than one instance.
- **The control plane is a working subset, not the whole surface.** It creates Projects,
  invites identities, grants standing and registers client applications. AccessPolicy authoring,
  link management, credential rotation and every list endpoint are still store-only.
- **Clinical resource types are not served** — Patient, Observation and everything else
  `CarriesClinicalData` reports true for. Withheld because serving PHI-bearing types on a build
  whose only principal comes from an environment variable is worse than serving none.
- **`AccessPolicy` can only express compartment restrictions.** "Share only `status=final`
  Observations" is not representable. Written up in `docs/design/authz-spec.md`.
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

Every type this build advertises is already classified: 38 are non-clinical, so an unrestricted
rule may cover them, and the other 28 all derive compartments, so a confined rule can reach them.
`TestEveryAdvertisedClinicalTypeCanBePlaced` is what keeps that true. Classifying is therefore
what it costs to advertise the *next* type, not a gap in the ones already served.

**6. Second factors and the login throttle. Done.** An identity may enrol a TOTP factor at
`POST /auth/mfa`; it is pending until a code proves it, so nothing can lock somebody out of their
own account except their own phone. The code is carried with the password rather than asked for
afterwards — a server that answered "now the code, please" would be saying the password was
right, and would say it to anyone who guessed an address that exists. A refused code, a wrong
password and an unknown address are one answer.

The secret is sealed with `ILAVRITA_SEALING_KEY` (32 bytes, base64). Unlike a password it cannot
be hashed: the server computes the same code the phone does, so whatever holds it holds the
factor. Sealing means a leaked database file is not a list of everyone's second factor. A
deployment that configured no key holds no factors rather than storing them in the clear.

Login attempts are now counted across the install rather than within one process — three replicas
would otherwise allow three times the guesses the limit states. The keys are digests: a table of
who tried to log in and failed is a list of this install's users and where they were.

Search parameters are deliberately a short list — `packages/search/registry.go` is the
whole of what this build answers, and adding one means adding a projection a write maintains and
a predicate a read compiles.

## 8. Conventions


One-line Conventional Commits, small and atomic. Comments explain the code in at most
three lines and never narrate a roadmap. Plans go in `ROADMAP.md`, design reasoning in
`docs/adr/` and `docs/design/`.
