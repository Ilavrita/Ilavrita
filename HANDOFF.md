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
| create, read, vread, update, delete, history-instance | Working, for 38 resource types |
| Everything else under `/fhir/R4` | `501` |

**38 resource types are served**: every type `authz.CarriesClinicalData` classifies as holding
no patient data — directory, terminology, conformance and definitional content. Patient,
Observation and every other clinical type return `404`, held by
`TestEveryServedTypeCarriesNoClinicalData`. That is the test to delete, deliberately and in its
own commit, on the day a login route lands — see §6.

**Authentication exists as a store and not as a route.** `UserStore.Authenticate` verifies an
argon2id password and `AcceptInvitation` sets one, but nothing HTTP calls either: every FHIR
route still answers `401` unless `ILAVRITA_DEV_PRINCIPAL` is set, which prints an unmissable
warning at startup. Do not deploy this anywhere near patient data.

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
- **No login route.** The password path is built and tested — derive, verify, accept an
  invitation, refuse a disabled identity, never cross a realm — but no HTTP handler calls it,
  so `ILAVRITA_DEV_PRINCIPAL` is still the only way a request names anyone. This is the single
  remaining blocker for clinical resource types.
- **No control-plane HTTP surface.** Projects, memberships, policy bindings, client
  applications and links are all writable through stores and reachable through no route.
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

**2. Real user authentication. Half done.** The credential half landed: argon2id derivation
and verification, invitation acceptance that cannot be replayed, a login that refuses a disabled
identity and never crosses a realm, and exactly one statement in the whole storage package that
reads a stored hash. **What is missing is the route**: a handler that takes an address and a
password, calls `UserStore.Authenticate`, and carries the resulting principal onto the request.
Session or token issuance comes with it, and `ILAVRITA_DEV_PRINCIPAL` must stop being a
supported path once it does.

**3. Tenant isolation proven at all three levels.** The mechanisms exist; what is missing is
an authenticated end-to-end test at each level:
   - **Project** — a caller in Project A reaches nothing in Project B.
   - **Linked Projects** — what this document and the code call links is what has been
     discussed as "child projects". There is no inheritance: a link grants only what it
     names, only while active, unexpired and approved by both sides. An organisational
     parent attribute may exist for display and confers nothing.
   - **Super Admin** — authority held only through an active membership in the
     `kind='super'` Project, audited, and never implying clinical data access.

**4. Expose the remaining FHIR resource types.** Currently six non-clinical types are
served; Patient, Observation and the rest of the PRD's initial coverage are withheld
deliberately. **This step depends on step 2.** Serving PHI-bearing endpoints on a build
without authentication is the one ordering mistake that would matter here.

Search is not in this sequence and remains the largest unstarted piece (§6).

## 8. Conventions

One-line Conventional Commits, small and atomic. Comments explain the code in at most
three lines and never narrate a roadmap. Plans go in `ROADMAP.md`, design reasoning in
`docs/adr/` and `docs/design/`.
