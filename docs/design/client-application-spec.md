# Client application and bot specification

This document specifies the part of the control plane that turns a non-human caller into a
first-class Project member: the `client_applications` and `bots` registries, the credential
lifecycle a client application's secret travels through, and the predicate that makes a machine
principal's standing die with the row behind it.

It differs from `user-spec.md` in one way worth stating plainly. That document was written before
its implementation and specified what to build; this one was written alongside the code it describes
and specifies what now holds. Every numbered rule below is a claim a test checks, and where a rule
was settled by something measured rather than reasoned, the measurement is given. Testable rules are
numbered `CAP-n`, following the `SCH-`/`CP-`/`LNK-`/`AUTH-`/`HIST-`/`IDN-`/`REST-` convention already
used across `docs/design/`. A design call the code left open is marked **(synthesis)**.

## 1. Where this sits

The domain types live in `packages/project`, beside `user.go` rather than in a package of their own
— **(synthesis)**. A client application and a user are both principals holding memberships, and
`PrincipalRef` already carries either; a separate package would have bought a name and cost an
import cycle the moment `IssueCredential` needed `project.ID`.

```
packages/project/client.go                          ServiceState, the id namespaces, ClientApplication
packages/project/bot.go                             Bot
packages/project/credential.go                      ClientSecret, CredentialHash, Credential
packages/storage/pocketbase/client_application.go   ClientApplicationStore
packages/storage/pocketbase/bot.go                  BotStore
packages/storage/pocketbase/migrate.go              PrepareSchema and the rebuild
```

`packages/authz` is untouched. `BuildScope` has no branch on principal kind and must not grow one:
the whole human/machine divergence lives in `membershipStatement` (§6).

## 2. Why two registries rather than one table

`project_memberships` carries a column per principal family, and `principal_kind` is a generated
column computed from which one is set. A per-family foreign key is what keeps that generated value
honest. A merged registry with a `kind` column would accept a bot's id in `client_application_id`,
and `principal_kind` would then name the wrong family with every constraint still satisfied.

**CAP-1.** A membership naming a client application or bot that no registry row describes is refused
by the database, not by application code. Before this work, `user_id` carried a foreign key and the
other two columns carried none, so a membership naming a phantom machine principal was accepted and
active, with no row anywhere to suspend or revoke.

**CAP-2.** Both keys are composite on `(project_id, <principal column>)`, so a membership cannot name
a registration belonging to another Project. This is the containment `users` cannot express, because
a user may be server-scoped and a machine principal never is.

**CAP-3.** `AssertMembershipPrincipalKeys` checks the column each key binds, not merely the parent
table it names. A key attached to `client_application_id` alone would satisfy a check that counted
parents, and would lose CAP-2.

## 3. Identifier namespaces

`ux_pm_active_principal` is unique on the generated `principal_id` alone, without `principal_kind`.
Two families sharing an id would therefore be one principal to it.

**CAP-4.** The three namespaces are disjoint by CHECK: `cli_` for a client application, `bot_` for a
bot, and neither prefix for a user.

**CAP-5.** The prefix CHECK is written `substr(id, 1, 4) = 'cli_'` and never
`id LIKE 'cli\_%' ESCAPE '\'`. Measured against SQLite: the `LIKE` form **accepts `'CLI_x'`**,
because SQLite's `LIKE` is ASCII-case-insensitive, while the `substr` form refuses it. A design that
reasoned about this without running it would have shipped the hole.

**CAP-6.** Go refuses an id that is its prefix and nothing else (`cli_`), which the CHECK alone
accepts. The stricter side is the constructor, matching the way `NewMembership` re-checks the
super-admin rule the database also enforces.

## 4. The credential model

### 4.1 What a stored credential is, and is not

A credential row holds a SHA-256 digest and never the plaintext. The plaintext exists exactly once,
as a return value from `ClientApplication.IssueCredential`.

**CAP-7.** `ClientSecret` is a struct with an unexported field, so `project.ClientSecret("hunter2")`
does not compile. This is what makes a single unsalted SHA-256 sound: the type admits nothing but
the 32 bytes `mintClientSecret` drew, so a guesser has nothing to shorten and a work factor would
buy latency rather than safety. It is a deliberate departure from `ClaimToken` and `PasswordHash`,
which are defined string types whose doc comments assert an entropy the type lets any caller violate.

**CAP-8.** Every value that can hold credential material implements `String`, `GoString` and
`MarshalJSON` as redactions — `ClientSecret`, `CredentialHash` and `Credential` itself. `Credential`
must redact independently, because `fmt` cannot call a method on an unexported field and `%v` on the
struct would otherwise spell out what `CredentialHash` carefully hides.

**CAP-9.** A short or failed read from the entropy source mints nothing: `io.ReadFull`, never `Read`.

### 4.2 Lifetime, rotation and revocation

**CAP-10.** A credential's lifetime is bounded at both ends and capped at 90 days, stated in the Go
constant and in the row's own CHECK so neither side can drift. `NOT NULL` alone would be satisfied by
the year 3000, which makes "every secret has a death date" true and meaningless.

**CAP-11.** Rotation is a sequence, not a replacement: `Supersede` on the outgoing credential, then
`IssueCredential` for the incoming one. The supersede lands first, so a torn rotation leaves an
application still authenticating on its outgoing secret until the window closes — recoverable —
rather than one holding two secrets nobody is tracking. `Supersede` can only shorten the life the
credential already had.

**CAP-12.** At most one `active` and one `superseded` credential exist per application, enforced by
two partial unique indexes. A third live secret is a constraint violation rather than a count some
application check reads and then races.

**CAP-13.** Revocation destroys the material rather than labelling it: the state, the NULL hash and
the revocation instant are written in one statement, and the mirrored CHECKs make both a live
credential holding no secret and a revoked one still holding material unrepresentable. A revoked
credential matches nothing because it holds nothing.

**CAP-14.** Revoking a registration destroys every secret issued under it, through a trigger. A
foreign key cannot express this: `ON DELETE CASCADE` fires on deletion, and a revocation is
deliberately not a deletion — the row survives for the audit and the material does not.

**CAP-15.** `Matches` folds liveness into the comparison and uses `subtle.ConstantTimeCompare`, so no
caller can compare a secret without also checking the clock, and a wrong guess cannot be narrowed by
how long the answer took.

## 5. Privilege a machine may not hold

**CAP-16.** Super Admin belongs to a user principal. Administering the install is answerable work,
and a secret answers to no one. Enforced by `CHECK (super_admin = 0 OR user_id IS NOT NULL)` and by
`NewMembership`. The existing `CHECK (super_admin = 0 OR project_kind = 'super')` is untouched; this
narrows the same column on a second, independent axis.

**CAP-17.** A bot holds no administrative standing at all. A bot runs code this server invokes, so
admin on one is a control-plane write reachable from whatever that code is made to do. A client
application may hold project admin; a bot may not.

**CAP-18.** A machine principal carries no profile. Its authority is its AccessPolicy, never a
compartment it occupies: a profile would collect compartment grants with no person in the chain that
leads to them. Easy to relax later, impossible to un-leak.

`Bootstrapper.Claim` needs no guard of its own. It builds the first Super Admin through
`firstSuperAdmin`, which routes into `NewMembership`, and it does so *before* spending the token — so
a machine principal presented to a claim is refused with the install still claimable.

## 6. Standing dies with the registry row

**CAP-19.** `membershipStatement` is a switch over the principal kind in which every arm adds the
liveness predicate for the registry behind it. A kind with no registry compiles no statement and
returns `ErrInvalidPrincipal`, so a fourth principal kind added to `PrincipalKind.Valid` denies
rather than inheriting standing gated by `project_memberships.state` alone.

**CAP-20.** Each registry predicate binds the request's Project a second time rather than comparing
one relation's Project to another's. The isolation therefore rests on the value the request carried
and not on the join, and the subquery is a primary-key seek.

**CAP-21.** Credential state is deliberately **absent** from the predicate. A rotated or revoked
secret must fail at authentication as `401`, not as an empty Scope and `403` — otherwise a rotation
is indistinguishable from a permissions bug in every log line it produces.

## 7. The stores, and the line they draw

**CAP-22.** No projection anywhere in `packages/storage/pocketbase` selects `secret_hash`. Every read
projects `COALESCE(secret_hash, '') <> ''`, exactly as `userColumns` does for `password_hash`, so a
read carries the single bit the domain asks and never the material. A test reads the package's own
source to hold this.

This is the physical line between this step and authentication. **The day a projection selects that
column is the day this server can check a presented secret**, and that commit is the whole diff of
"authentication became possible".

**CAP-23.** A credential a read rebuilt carries a sentinel rather than a digest, and
`IssueCredential` refuses to write it. Otherwise a round trip would store a hash no secret hashes to,
which would answer nothing forever.

**CAP-24.** Every statement goes through `conn(ctx, db)`, so a store called inside a transaction
writes through it rather than deadlocking on SQLite's write lock against a pool of one connection.

**CAP-25.** `UpdateState` reads before it writes, so the lifecycle runs against the persisted state
and an illegal move is named rather than silently matching no row; then every precondition travels in
the `WHERE`, so the decision cannot be stale by the time it lands.

## 8. Reaching a database that already exists

`ApplySchema` applies the schema with `CREATE TABLE IF NOT EXISTS`, and SQLite has no
`ALTER TABLE ADD CONSTRAINT`. The new keys would therefore reach only databases created after they
were declared, and nothing would distinguish the two.

**CAP-26.** `ApplySchema` is unchanged and keeps its doc comment's promise that applying a current
schema changes nothing and reports no error. The assertions cannot live inside it: it is the only
thing that creates the registries, so it would refuse to run on the database that needs them. A new
`PrepareSchema` owns the sequence, and the server calls that.

**CAP-27.** The rebuild is preceded by a pre-flight that names the rows a newly adopted constraint
would reject — dangling principals, ids outside their namespace, super admin or a profile on a
machine principal, admin on a bot. The copy would otherwise abort with a constraint error naming no
row at all. **A membership is never deleted to make a constraint pass.**

**CAP-28.** `ux_pm_link_sourced` is recreated inside the rebuild transaction, before
`PRAGMA foreign_key_check`. It is the parent key `project_membership_policies`' three-column foreign
key resolves against, not merely an index. Measured: without it, `foreign_key_check` **raises from
the query itself** rather than returning rows, so the common idiom `rows, _ := Query(...)` would
swallow the most severe outcome as a pass.

**CAP-29.** A trigger standing on another table whose body names `project_memberships` is dropped
before the rename and restored by the schema replay. Found by testing, not by reasoning: the rename
re-validates such triggers, and the table they name is gone at that moment, so the rename fails
outright. The triggers are found by what they say rather than by name, so one added later is carried
through without editing the rebuild.

**CAP-30.** `PRAGMA foreign_keys = OFF` is issued outside any transaction, on a dedicated connection,
and restored on every path including failure. Measured: the pragma is a **no-op inside a
transaction**, so a rebuild that toggled it after `BEGIN` would run with enforcement still on.

**CAP-31.** The copy carries only the columns `PRAGMA table_xinfo` reports with `hidden = 0`, which
excludes `principal_kind`, `principal_id` and `link_sourced`; a generated column cannot be inserted
into and is recomputed anyway. A column the current declaration does not name would be dropped by the
copy, which is data loss rather than a migration, and is refused.

**CAP-32.** Only the first occurrence of the table name is substituted into the rebuild's
declaration, so the self-referencing `invited_by_membership_id` key keeps naming the final table,
which the new one becomes after the rename.

**CAP-33.** The new table is renamed *into* place rather than the old one being renamed away, which
is what leaves `project_membership_policies`' and `project_link_capabilities`' own foreign key text
intact.

**CAP-34.** A database whose membership table still lacks the keys refuses to serve, in the same
posture `AssertForeignKeysEnforced` already takes: a guarantee that is quietly absent is worse than
one that is loudly missing.

## 9. Rules a test can be written against

**The registries.** A membership naming an unregistered client application or bot is refused
(CAP-1). One naming a registration in another Project is refused (CAP-2). A bot id in the client
application column is refused (CAP-4). `'CLI_x'` is refused (CAP-5).

**The credential.** `ClientSecret` has no exported field (CAP-7). No rendering of a secret, a hash or
a credential carries either (CAP-8). A short read mints nothing (CAP-9). A lifetime past 90 days is
refused on both sides (CAP-10). A supersede cannot extend the secret it retires (CAP-11). A third
live credential is refused by the index (CAP-12). A revoked credential holds no hash and matches
nothing (CAP-13). Revoking a registration destroys its secrets and keeps their rows (CAP-14). An
expired credential matches nothing (CAP-15).

**Privilege.** Super admin on a client application or a bot is refused, in Go and in SQL (CAP-16). A
bot holding admin is refused (CAP-17). A profile on a machine principal is refused (CAP-18).

**Standing.** A suspended or revoked registration holds no membership (CAP-19). A registration in
another Project never answers for this one (CAP-20).

**The store.** No projection selects `secret_hash` (CAP-22). A rebuilt credential cannot be written
back (CAP-23). A create inside a rolled-back transaction leaves nothing (CAP-24).

**The migration.** A legacy table gains the keys and then refuses what it previously accepted (CAP-1,
CAP-26). Every membership and all seven indexes survive (CAP-28, CAP-31). The triggers survive and
still fire (CAP-29). Foreign key enforcement is restored (CAP-30). The rebuild is a no-op on a
current database (CAP-26).

## 10. Gaps observed — flagged, not fixed

- **The overlap window is bounded, not closed.** While a superseded credential lives, two secrets
  authenticate as one principal with one Scope. An attacker holding the old one keeps it until
  expiry unless someone calls `RevokeAllCredentials`. The defence is that rotation *happening* is
  worth more than the window, not that the window is safe.
- **SHA-256 without salt or work factor is correct only while CAP-7 holds.** The struct makes it true
  by construction today; an operator-supplied secret would evaporate the justification and require
  argon2id.
- **`users.id` is not retroactively constrained.** The prefix CHECKs make the namespaces disjoint
  going forward and the pre-flight names any violating row, but rebuilding `users` is a second
  migration this work does not take on. The CHECK that would close it is
  `CHECK (substr(id, 1, 4) = 'usr_')`.
- **`ON DELETE RESTRICT` makes both registries append-only in practice.** A registration that ever
  held a membership can never be deleted; removal is permanently a state change, and the teardown
  story depends on a purge worker under `projects.state = 'deleting'` that no document specifies.
- **The stores take `project.ID`, not `storage.Scope`.** They are control-plane stores like the
  resolvers, and no control-plane HTTP surface exists yet. Until one does and authorizes above them,
  the tenant argument is a parameter a caller supplies rather than an entitlement anyone proved — a
  call that must be revisited, not inherited.
- **Auditable authentication is not delivered.** FR-053 names it. The `superseded` state exists so an
  audit *could* distinguish a rotation-window authentication from a normal one, but nothing writes
  that record: `packages/audit` is a doc comment and there is no authenticator.
- **Bots ship as identity only.** No `code`, `runtime` or `trigger` column and no `Run` method.
  `ROADMAP.md` places Bots in Phase 3; the table lands now because `project_memberships.bot_id` was
  waiting for its foreign key and that key cannot land cheaply later, while execution can.
- **`platform_resource`'s allowlist is corrected in the schema but only asserted on an older
  database.** Two tables are not rebuilt for a document family with no store; the startup guard
  proves the row does not exist rather than making it unrepresentable there.
- **The rebuild has no dry-run.** The pragma is per-connection and not persisted, so a crash cannot
  poison the file, but a death between the drop and the rename needs manual recovery.
