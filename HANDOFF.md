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
- **Type-level and system-level history are not served.** `version_seq` is per resource, so
  ordering a type's whole history by it interleaves by version number rather than by time, and a
  cursor built from one means nothing across resources. It was written, found to page wrongly, and
  reverted rather than ship looking like a feed. Doing it properly means ordering by `last_updated`
  with a tiebreaker and a compound cursor.
- **Search parameters are a deliberately short list.** `packages/search/registry.go` is the whole
  of what this build answers. Adding one means adding a projection a write maintains and a
  predicate a read compiles.
- **The release pipeline has never run.** Signing, SBOM and provenance are configured and
  unexercised. Cut `v0.0.1-rc.1` first, deliberately.

## 7. Next step

Everything previously listed here is built; §1 says what a request reaches and §6 says where the
edges are. What is left is not more surface.

**1. Review by somebody who did not build this.** The one that matters. Every test asserting a
boundary holds was written by whoever wrote the boundary, so the whole isolation story is
self-certified. Inferno judges protocol behaviour and found three real defects doing it; it does
not judge whether a Scope leaks. A second pair of eyes on `packages/authz` and
`packages/storage/pocketbase` is worth more than any feature below.

**2. A migration story between releases.** Migrations are idempotent and recorded, but nothing
takes an install from one tagged version to the next. This is cheap now and expensive once
somebody is holding data.

**3. Cut `v0.0.1-rc.1`.** Signing, SBOM and provenance are configured and have never executed.
Find out on a prerelease nobody depends on.

**4. TLS.** The one Inferno check still failing.

Everything else — type-level history, more search parameters, bot execution, the rest of the
control plane — is in §6 with its reason. None of it blocks the four above.

## 8. Conventions


One-line Conventional Commits, small and atomic. Comments explain the code in at most
three lines and never narrate a roadmap. Plans go in `ROADMAP.md`, design reasoning in
`docs/adr/` and `docs/design/`.
