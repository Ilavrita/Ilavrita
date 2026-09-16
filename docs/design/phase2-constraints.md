# Phase 2 implementation constraints

Derived from `project-model.md`, `linked-projects.md`, `authorization.md`, `platform-resources.md`
and nine adversarial reviews that broke earlier drafts of these designs (five critically). The three
design docs disagree with each other at several load-bearing points; where they do, the resolution
below is the one the adversarial fixes converged on, not a restatement of either original. Every rule
is testable. Every rule names the attack it closes.

## Already settled — do not re-derive

`packages/storage/repository.go` and `packages/storage/scope.go` are frozen and already encode part
of the fix set below. Read them before implementing anything in this file:

- `Grant{Project, Kind, Type, Action, Source, Compartment *Compartment}` — a Grant is scoped to
  exactly one `(Project, Kind, Type, Action)` tuple plus at most one `Compartment{Type, ID}`. There is
  **no** generic multi-term `Constraint`/`ParamTerm` tree. This already closes the cross-type reuse in
  Attack 4 (an `_include` for a different type cannot ride a Grant minted for another) and the
  admin/data OR-confusion in the platform-resources.md "FR-052 as a greppable rule" section (`Allows`
  requires exact `Kind` match, so a `KindPlatform` grant can never satisfy a `KindFHIR` check).
- `Scope{grants []Grant}` is unexported; `NewScope` is the only constructor; `Scope{}` denies. `Allows`
  and `Narrow` can only read or remove Grants, never add them.
- `Read`/`Create`/`Update`/`Delete`/`ReadVersion`/`ListVersions` all take `Scope` as the mandatory
  second parameter. This already supersedes `linked-projects.md`'s Rule 1 ("Key reads never consult
  links"), which Attacks 2 and 3 independently broke — do not implement a by-key path that skips
  `Scope`, and do not resurrect a scope-free `Read(ctx, key)` for Binary or any other by-key surface.
- `Kind` (`fhir` | `platform`) already commits to `platform-resources.md`'s separate-physical-tables
  decision over `authorization.md` §3's shared `resources` table with a `res_kind` discriminator.
  Do not build one `resources` table for both families — `res_kind` as a "route guard" is not load-bearing
  (Attack 5 S1: it is absent from the row's uniqueness key, so `PUT /fhir/R4/AccessPolicy/lab-share`
  and a platform write to the same id collide) and separate tables make the confusion physically
  impossible instead of conventionally forbidden.

**Open gap this leaves:** `Grant.Compartment` is the only structural restriction `storage` can express.
Every design doc's worked examples (a link sharing only `status IN ('final','amended')` Observations, a
policy rule with `criteria.param`) need something `Grant` cannot carry. Before any link or policy rule
ships with a restriction beyond "this type, this action, optionally this one compartment," decide and
document where that restriction is enforced — it must still be a bound predicate in the compiled SQL
(never post-fetch filtering), and it must not be smuggled in as an unauthenticated addition to the
Grant. Until that mechanism exists and is specified, a link or policy rule that needs param-level
restriction must not be representable — reject it at validation time rather than silently dropping the
restriction and serving the wider set.

## Schema

**SCH-1. `project_id` (or the schema's equivalent tenant column) is the leading column of every index
and every primary key on every table that carries Project-scoped or link-derived data, with no
exceptions.**
Closes: Attack 3 S1 (`idx_reference_rev` had `project_id` as column five, not one — the one index where
dropping the project predicate is free because the plan is still a perfect index seek); Attack 5 (the
derived `project_link` lookup table must lead with the consuming project, and its lifecycle columns
must sit inside the index, see LNK-1).
Mechanism: `idx_reference_rev(project_id, target_project, target_type, target_id, param, resource_type,
logical_id)`. Add a generated test that parses every `CREATE TABLE`/`CREATE INDEX` in the migration set
and asserts column one of every primary key and every index on a Project-scoped table is the tenant
column — this must be a schema-derived test, not a hand-maintained list.

**SCH-2. A compiled query may never establish the tenant predicate by comparing two joined relations
to each other (`x.project = y.project`); every relation the compiler adds must bind the tenant literal
itself.**
Closes: Attack 9 — the design's own SQL had `project` appearing five times but only one bound literal;
the other four were relative comparisons that are tautological once a surrogate key (`rid`) is globally
unique, because `t.rid = r.rid` already pins `t` to one resource, making `t.project = c.project` provably
true regardless of the removed literal. Measured: deleting the one anchored predicate turned a
300-row single-tenant result into 2,400 rows across 8 tenants, with a *faster*, not slower, query plan
(SQLite skip-scan), so no latency regression test catches it.
Mechanism: one relation-join constructor, `rel(tbl, alias, grant)` → `JOIN <tbl> <alias> ON
<alias>.rid = <driver>.rid AND <alias>.project = ?` with the literal bound from the arm's
`Grant.Project`. No other code path may add a table to FROM/JOIN/EXISTS. `x.project = y.project` must
not be constructible in the query-builder AST (make the field private and the comparison method
nonexistent, not merely unused). Add a positive test per compiled arm: the count of bound `project`
parameters equals the count of relations joined in that arm.

**SCH-3. The `EXPLAIN QUERY PLAN` guard must assert structurally on the compiler's own emitted alias
and must fail on a skip-scan, not merely on the literal string `SCAN resources`.**
Closes: Attack 9 — SQLite prints the alias (`SCAN r`, not `SCAN resources`), so any aliased query makes
the literal-string guard permanently pass; separately, the leaking plan is `SEARCH t USING COVERING
INDEX ... (ANY(project) AND ...)` — a skip-scan, not a table scan — which is a `SEARCH`, not a `SCAN`,
and passes a scan-only guard even when correctly parsing aliases.
Mechanism: parse `EXPLAIN QUERY PLAN` output structurally (not substring match); fail on any row whose
detail contains `ANY(` (skip-scan = leading index column left unbound) or starts with `SCAN `, matched
against the alias the compiler itself generated. Apply this to every compiled statement — read, vread,
history (instance/type/system), conditional writes, `_include`/`_revinclude` — not only "an authorized
search."

**SCH-4. Create and Update compile to different SQL; neither may be implemented as a shared upsert
whose `WHERE` clause omits the pre-image's authorization predicate.**
Closes: Attack 7 — a single `ON CONFLICT ... DO UPDATE ... WHERE version_seq = ? AND project_id = ?`
statement (no scope predicate) let a principal authorized only to create in their own compartment
silently overwrite an existing resource they could not read, because the DO-UPDATE branch has no
pre-image check.
Mechanism: `Create` (`packages/storage` `ResourceRepository.Create`) compiles to
`INSERT ... ON CONFLICT (project_id, resource_type, logical_id) DO NOTHING RETURNING rid, version_seq`;
zero rows returned is the sole conflict signal and maps to 409, never a fallthrough to update. `Update`
compiles to `UPDATE ... WHERE rid = ? AND project_id = ? AND version_seq = ? AND
<the Scope's Grant.Compartment predicate against the CURRENT row, re-checked after any index rebuild>
RETURNING rid, version_seq`; zero rows affected is 409 (version conflict) or 403 (out of scope) —
distinguish by re-reading only inside the already-scoped transaction, never by a separate unscoped
lookup. `Delete` follows the same shape.

**SCH-5. A create request must not be distinguishable, by status code or response shape, between "id
is free" and "id is taken by a row outside the caller's Scope."**
Closes: Attack 7 — measured `changes()` values (1 vs 1 vs 0) let an ordinary member with a narrow
compartment-scoped policy enumerate the existence of any logical id of any resource type in the
project, because the collision check had no scope predicate.
Mechanism: either (a) the id-taken and id-free-but-reserved cases return the identical response, or
(b) client-supplied logical ids are rejected outright for any resource type the caller holds no
`ActionRead` (or `ActionSearch`) Grant for. Add to `storagetest`: create against an existing id the
caller cannot read must be indistinguishable (response body, status, timing bucket) from create
against a genuinely free id.

**SCH-6. Recreating a logical id that was previously soft-deleted must not let the new owner inherit
read access to versions written before the delete.**
Closes: Attack 6 (Mallory, authorized only in her own `Patient/pat-mallory` compartment, does
`PUT` on a soft-deleted `QuestionnaireResponse` another patient owned; the design's own "update as
create" rule authorizes on the post-image only, the upsert resurrects the same `rid`, and `_history`
then returns all five of the victim's prior versions verbatim) and Attack 8 Attack 3 (a tombstone's
index rows are rebuilt from `NULL` content = zero rows, so *any* principal in the project can read the
tombstoned resource's full pre-delete history, because the only authorization data was destroyed along
with the index rows the delete produced).
Mechanism: add `identity_epoch INTEGER NOT NULL DEFAULT 0` to the resource's current-state and history
tables. Bump it in the `Create`/`Update` branch whenever the pre-image had `deleted = 1`. Every history
read carries `AND identity_epoch = :current`. `Create` must reject (not silently succeed) when the
pre-image is a tombstone whose compartment the caller's Scope could not have read — this requires
resolving the pre-image's authorization fact even on the create path (see HIST-1), not skipping it
because "it's a create."

**SCH-7. The surrogate primary key backing a resource (`rid`) is never reused and is always obtained
from the same statement that mutates the row, never from a separate `last_insert_rowid()` call.**
Closes: Attack 6/7/8 — `last_insert_rowid()` measured wrong (returns an unrelated earlier insert's
rowid) on the `DO UPDATE`/conflict branch of a pooled connection, which would attach history rows and
rebuild index rows against the wrong resource; separately, `rid INTEGER PRIMARY KEY` without
`AUTOINCREMENT` reuses a value freed by deleting the highest row, which a purge worker (project-model.md's
`deleting` state) makes a live path — a cached or paginated `rid` captured before a purge silently
re-points at a different project's row afterward.
Mechanism: `rid INTEGER PRIMARY KEY AUTOINCREMENT` on SQLite, matching the design's own
`GENERATED ALWAYS AS IDENTITY` mapping for Postgres (today the portability table presents this as a
type change; it is a semantics change and must be documented as one). Every write path uses
`RETURNING rid, version_seq`; a lint/architecture test bans `last_insert_rowid()` anywhere under
`packages/storage/pocketbase`.

**SCH-8. `rid` (or any backend surrogate) is never returned to a client — pagination cursors are keyed
on data the client is already authorized to see, or are opaque and integrity-protected.**
Closes: Attack 6/8 — `rid` is one sequence across all tenants; a client decoding two `(last_updated,
rid)` cursors an hour apart gets a server-wide insert-rate oracle, and this directly contradicts the
schema's own comment that it is "never published (FR-003)."
Mechanism: paginate on `(last_updated, logical_id)` scoped within the arm's project, or MAC the cursor
and keep `rid` server-side only.

**SCH-9. `PRAGMA foreign_keys=ON` is asserted at process startup; the server refuses to serve if it is
off.**
Closes: every composite-FK-backed guarantee in this document and in project-model.md (`super_admin`
containment, profile-in-project, policy-in-project) degrades to a convention, not an invariant, the
moment FK enforcement is off on a connection — and it is a per-connection setting in a pooled driver,
not a database-wide one.
Mechanism: on every new connection (PocketBase's pool included), run `PRAGMA foreign_keys` and fail
startup (not just log) if it does not report `1`. This is project-model.md's own named Risk 1; treat it
as a hard gate, not a documented caveat.

## Control plane

**CP-1. A membership whose only standing in a project came from an administrative `project_link` can
never hold `admin = 1` or `super_admin = 1`, and can never hold any policy binding.**
Closes: Attack 1 — an ordinary admin of grantee project O, holding *only* an administrative link's
`project.membership.admin` capability into grantor project C, wrote a full `ProjectMembership` for
herself in C bound to C's own pre-existing permissive AccessPolicy. Every stated CHECK, FK and
two-sided-approval invariant in `linked-projects.md` held throughout — the escalation happens entirely
in the control plane, one hop upstream of every query-planner guarantee. Attack 1's Variant A shows
this composes transitively across a chain of administrative links (C→R→O) because the capability that
defines the admin role is itself writable by that role; Variant B shows a self-targeting guard alone
does not close it (the attacker invites an address she controls instead).
Mechanism: `ALTER TABLE project_memberships ADD COLUMN via_link_id TEXT REFERENCES project_link(id)`,
`CHECK (via_link_id IS NULL OR (admin = 0 AND super_admin = 0))`. `ALTER TABLE
project_membership_policies ADD COLUMN via_link_id TEXT`, `CHECK (via_link_id IS NULL)` — a
link-minted membership can hold zero policy bindings, which already means (per `authorization.md` §8)
it resolves to `Scope{}` on every `/fhir/R4` route. The same `via_link_id` predicate applies to
invitations (`invitation_source`), since an invitation is a deferred membership write and is otherwise
Variant B's route around a self-targeting-only guard.

**CP-2. The set of capabilities an administrative link can confer is a closed allowlist that structurally
excludes every membership-write and policy-write capability; the exclusion is a constraint, not an
application check.**
Closes: Attack 1 and Attack 2's second vector — both show that `project.membership.admin` being
link-conferrable is exactly what turns "provably zero data scopes" (the CHECK on `all_resource_types`
and the rejection of `project_link_type` rows) false: it is one membership write, in the grantor
project, away from a full data grant, and the audit trail names only "granted membership," never the
resulting data access.
Mechanism: link-conferrable = `project.quota.write`, `project.lifecycle.*`, `project.settings.write`
excluding authentication policy and identity-provider registration (those are two more routes into the
project per FR-057), plus read-only membership listing. Never link-conferrable, enumerated and
enforced with a lookup/CHECK against `project_link_capability.capability` at link-write time:
`project.membership.write`, `project.membership.admin`, `project.policy.write`, `project.secret.*`,
identity-provider registration.

**CP-3. `project_link_type` rows are structurally impossible on an administrative link — the exclusion
is a foreign key, not solely the write-time application check.**
Closes: Attack 2 (THIRD) and Attack 3 (S2/summary) — `project_link_type.link_id REFERENCES
project_link(id)` carries no `kind` discriminator, so a second write path (a migration, a bulk import, a
future admin endpoint) can attach data types to an administrative link without violating any FK, even
though the primary admin-console write path checks it.
Mechanism: add `kind TEXT NOT NULL CHECK (kind = 'data')` to `project_link_type`, and a composite FK
`(link_id, kind) REFERENCES project_link(id, kind)`. An administrative link with data scopes becomes
unrepresentable in the schema.

**CP-4. Administrative capability grants target enumerated principals or membership ids, never the
ambient role "the grantee project's admins."**
Closes: Attack 1 Variant A directly — the chain works because the capability is granted to a *role*
whose roster the role itself can write to, via the link.
Mechanism: `project_link_capability` (or its successor once CP-2 lands) names a principal or membership
id, not a role predicate resolved at authorization time.

**CP-5. `super_admin = 1` is confined to the Super Project by a database composite FK plus CHECK, and
that guarantee is regression-tested, not merely believed.**
Every attack that tried to defeat this explicitly failed and said so — this constraint exists to keep
it that way; do not weaken `FOREIGN KEY (project_id, project_kind) REFERENCES projects(id, kind)` +
`CHECK (super_admin = 0 OR project_kind = 'super')` when implementing any of the above.
Mechanism: `storagetest` includes a case that attempts to write `super_admin=1` under every
non-super `project_kind` and asserts an FK violation, run against both backends (SQLite and, once it
exists, Postgres).

**CP-6. There is no query shape that emits zero project predicates; Super Admin cross-project reads
iterate concrete project ids, one audited `Grant` per project, never a single "all projects" grant.**
Closes: a direct contradiction between project-model.md ("There is no `ProjectID(\"*\")`... every query
keeps a literal project predicate, including a Super Admin's") and `authorization.md`'s
`GrantAllProjects` ("the one code path that emits no project column predicate"), which Attack 5's S4
finding names explicitly and resolves in project-model's favor: "the first piece of code that treats a
sentinel as a wildcard is the cross-project leak."
Mechanism: do not implement a `GrantAllProjects`/`SourceSuperAdmin` grant that omits `Grant.Project`.
Super Admin cross-project operations call `ListProjects`, then mint one `Grant{Project: p, Source:
SourceSuperAdmin, ...}` per project, each compiled and audited independently — the fan-out is explicit
Go code (a loop with N audit records), never a single query with an unpinned tenant column.

**CP-7. `NewScope`/`NewGrant`/`SystemScope` construction has zero call sites outside `packages/authz`
(and the explicit dev-wiring/bootstrap path), verified by an automated test in CI, not by an advisory
lint rule alone.**
Closes: Attack 3 S2 — `storage.NewScope`/`storage.NewGrant` are exported, so
`storage.NewScope(storage.NewGrant("CLINIC", SourceLink, nil))` (an unrestricted Grant — nil
Compartment) is one compiling line anywhere in the codebase, and the only proposed guard (`forbidigo`)
is `//nolint`-suppressible and, per the same attack, not even enabled in `.golangci.yml` today.
Mechanism: a `go/analysis`-based or grep-based architecture test, run in CI and failing the build (not
a lint warning), asserting no call site of `storage.NewGrant`/`storage.NewScope`/`authz.SystemScope`
exists outside `packages/authz/**` and the named bootstrap path. Enable `forbidigo` in `.golangci.yml`
as a second, earlier-feedback layer — it is currently absent despite both `linked-projects.md` and
`authorization.md` assuming it runs.

**CP-8. No boolean OR between an administrative check and a data-policy result, anywhere — already
structural via `Grant.Kind`, but regression-guarded.**
The settled `Scope.Allows(project, kind, type, action)` requires exact `Kind` match, so this is already
closed by the type, not merely a convention (`platform-resources.md`'s own stated FR-052 rule). Add the
regression test: grep `packages/authz` and every HTTP handler for the token pattern `.Admin |` or
`| .*Admin` adjacent to a data-scope return, so a future refactor cannot reintroduce
`canRead(p) { return p.Admin || policyAllows(p) }` even by accident.

## Links

**LNK-1. The link-lookup relation the query planner probes carries lifecycle state
(`status`, `expires_at`) in its columns and in its index; a proposed, suspended, revoked or expired
link authorizes nothing.**
Closes: Attack 5 — `authorization.md` §5's derived `project_link` table has five columns
(`granting_project, consuming_project, res_type, action, link_rid`) and no lifecycle column at all, so a
mere *proposal* — before the grantee ever approves — grants everything it names, permanently, because
nothing in the row changes when the grantor later revokes.
Mechanism:
```sql
CREATE TABLE project_link (
  granting_project TEXT NOT NULL, consuming_project TEXT NOT NULL,
  res_type TEXT NOT NULL, action TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('proposed','active','suspended','revoked')),
  expires_at INTEGER, link_rid INTEGER NOT NULL,
  PRIMARY KEY (granting_project, consuming_project, res_type, action));
CREATE INDEX project_link_lookup
  ON project_link(consuming_project, res_type, action, status, expires_at, granting_project);
```
Layer-1 probe: `... AND status='active' AND (expires_at IS NULL OR expires_at > :now)`.

**LNK-2. Every status change to a `ProjectLink` platform row re-derives the query-time link relation as
a full replace (delete-then-reinsert, same transaction), never an append.**
Closes: Attack 5 — because the `ProjectLink` row is itself versioned, revocation produces a *new
version*, not a delete; with no natural key to update on, the derivation "inserts another row or leaves
the first," so the grantor's console can show `status: revoked` while the authorization path is
unchanged.
Mechanism: the `PRIMARY KEY (granting_project, consuming_project, res_type, action)` from LNK-1 turns a
stale duplicate into a constraint violation. The write transaction that changes `ProjectLink.status`
must `DELETE FROM project_link WHERE granting_project=? AND consuming_project=?` and re-`INSERT` from
the new current version's active rows, inside the same transaction. Add a property test asserting the
derived table's active rows always equal the set recomputed from the current `ProjectLink` version —
the same consistency-checker posture `authorization.md` already accepts for `idx_compartment`.

**LNK-3. A data link covering N resource types is represented as N separate `Grant`s (one per type),
never as one `Grant` with an implicit type list — a resource type absent from `project_link_type`
produces no `Grant` for that type.**
Closes: Attack 4 root cause (a `Grant` with no type restriction lets an `_include`/`_revinclude` for an
unrelated type ride the primary query's authorization) and Attack 3 S2 (linked-projects.md's per-link
type list has no equivalent field on `authorization.md`'s `Grant` in the design docs). This is
consistent with, not an extension of, the settled `Grant.Type ResourceType` (singular): the authorizer
mints one `Grant` per `(link, resource type)` pair drawn from `project_link_type`, so the type
restriction lives in *which Grants exist*, not in a field any of them carries.
Mechanism: `authz.Authorize` resolving a link (`LinkResolver.Inbound`) iterates the link's
`project_link_type` rows and mints one `Grant{Project: grantor, Kind: KindFHIR, Type: t, Action: a,
Source: SourceLink, Compartment: ...}` per row, never a single Grant meant to cover multiple types.

**LNK-4. `_include`/`_revinclude` targeting a type outside the primary query's type must be authorized
by checking `Scope.Allows` for that specific target type and action before the include arm is
compiled; a Scope non-empty for the primary type is not sufficient.**
Closes: Attack 4 — the primary break (`GET /fhir/R4/Practitioner?_revinclude=Observation:performer`
returning the project's whole clinical dataset to a principal whose only rule matched `Practitioner`
with no criteria) and its Compartment-restricted variant (a rule scoped to "Practitioner rows about me"
still leaks the practitioner's entire Observation compartment when the Compartment term is applied on
the wrong axis, i.e. to the include arm instead of the primary arm).
Mechanism: for every `IncludeSpec`, call `scope.Allows(targetProject, KindFHIR, includeType,
ActionSearch)` (or, if the target type was never authorized in the original `Authorize` call, issue a
fresh `Authorize(ctx, Request{Kind: KindFHIR, Type: includeType, Action: ActionSearch})`) before
emitting that arm; a false/empty result drops the include silently (FHIR-legal), never an implicit pass.
Add to `storagetest`: for every (primary type, include type) pair, a Scope authorized only for the
primary type must return zero include rows when the principal holds no Grant for the include type.

**LNK-5. A rule (AccessPolicy or link) that matches `(type, action)` with no restriction must not
silently lower to unrestricted; unrestricted must be an explicit, validated declaration, and is
rejected for any clinical resource type.**
Closes: Attack 4 and Attack 9 — both independently found that a criteria-less rule (an ordinary
"read all Observations in my project" or a "staff directory" rule) lowers to a vacuously-true
restriction with no attacker error required, and Attack 9 additionally shows this is the exact value
that moves ownership of the tenant predicate from authz-authored code to search-feature code (see
SCH-2/SCH-3), because a Grant with no Compartment makes the compiler fall through to "drive from the
user's query term" instead of a project-anchored relation.
Mechanism: policy/link validation rejects a rule with absent restriction; require an explicit
declaration (e.g. an `unrestricted: true` marker) for the genuinely-unrestricted case, and reject that
marker at validation time for any type on the clinical-data type list (Observation, Condition,
DocumentReference, MedicationRequest, etc. — the same list the compartment/PHI machinery already
treats as sensitive). A Grant produced from an explicitly-unrestricted rule must still compile against
a project-anchored driving relation (SCH-2), never fall through to a user-query-driven arm.

**LNK-6. A link's `AccessPolicy`/restriction is always resolved against the grantor project, never the
consuming project.**
Closes: Attack 5 S6 — if resolved against the consuming project, project A's admin can write a
same-named, permissive policy in A and the link's restriction lowers through it, a total bypass of the
grantor's intent; the design docs never state which project's policy is authoritative.
Mechanism: any link-sourced restriction is loaded with `project = grant.Project` (the grantor) by
construction — the lookup key is never parameterized by the consuming project. Add the composite FK
mirroring project-model.md's existing idiom: `project_link(access_policy) REFERENCES
access_policies(project_id = grantor_project, id)`.

**LNK-7. Chained search across a link is rejected in Phase 2; active `kind='data'` links per grantee are
hard-capped.**
Retained from `linked-projects.md`; no attack defeated this bound, and Attack 4's own cross-project
amplifier attempt explicitly notes it could not build a working exploit partly because chaining stays
confined. Treat the cap and the chaining rejection as part of the blast-radius argument the rest of this
document depends on, not as optional polish.
Mechanism: `search.Parse` rejects any chained parameter whose target crosses a project boundary;
`project_link_by_grantee` (or LNK-1's replacement) is capped at 16 active `kind='data'` links per
grantee (config, hard ceiling), enforced at approval time.

**LNK-8. Resolve `_project` once, in one direction: it is an authorization *input*, consumed by
`authz.Authorize` before a Scope is minted, and never reaches `ResourceKey` or the search parser as a
raw value.**
Closes: a three-way contradiction Attacks 1, 2 and 3 each flag independently — `authorization.md`
Phase-1 item 8 says "There is no `_project` parameter; the parser rejects any client parameter naming a
project," while `linked-projects.md` requires `_project=<grantor-id>` as the sole client opt-in and
states "absent the opt-in, branches is nil." Both cannot ship; whichever an implementer reads determines
whether Bulk Data, GraphQL, subscriptions, Bots and async jobs silently inherit cross-project reach the
moment any link exists (Attack 3 S5) — the exact ambient-authority regression `linked-projects.md`'s own
"Why" section exists to prevent.
Mechanism: keep `_project` as the repeatable search parameter and provenance carrier (`meta.tag`,
Bundle `self` link) `linked-projects.md` specifies. The handler reads it and passes it into
`authz.Authorize(ctx, Request{..., LinkedProject: p})` as an argument — never assigns it into a
`ResourceKey`, and `search.Parse` continues to reject it (and every other raw project/index-column
parameter) as a *query* parameter. Absent `_project`, `Authorize` mints only home-project Grants. Bulk
Data, GraphQL, subscription and Bot/job code paths must never pass `_project`, which keeps them
own-project-only by construction rather than by each remembering to opt out.

**LNK-9. FR-052's non-transitivity claim is tested over the capability *image*, not only over data
grants.**
Closes: Attack 1 — the design's existing test ("A→B plus B→C yields no A→C data grant") covers data
links only and passes throughout the whole attack chain; the property actually violated is about
control-plane capability composing across administrative links, which was never tested.
Mechanism: add a property test — for `admin_link(C→R)` and `admin_link(R→O)`, (a) no principal gains any
capability over C by virtue of standing in O (control-plane closure), and (b) no sequence of
link-conferred control-plane writes increases any principal's data Scope in the grantor project (data
closure) — both evaluated transitively across a chain, not just directly across one hop.

## Authorization

**AUTH-1. `Grant.Action` values map onto storage interface methods by a fixed, written table — there is
no separate "vread" action in the settled enum (`ActionRead, ActionWrite, ActionDelete, ActionSearch,
ActionHistory`), so an implementer must not guess which one gates `VersionStore.ReadVersion`.**
Closes a vagueness the design docs leave implicit and that several attacks exploit exactly at this
seam (Attacks 6 and 8: version-specific reads authorized under a different, looser check than the
one the current-state read used). Left undecided, an implementer will plausibly gate `ReadVersion`
under `ActionRead`, which is wrong: `ActionRead`'s Grant may have been minted against the *current*
compartment only, and a version predating a compartment change must not be readable just because the
current version is.
Mechanism: `ReadVersion` and `ListVersions` both authorize under `ActionHistory`, never `ActionRead`.
Document this mapping in `packages/authz` next to the `Action` constants themselves, not only in a
design doc, since `scope.go` cannot carry doc comments this specific without becoming a modification of
the frozen file.

**AUTH-2. A Grant's `Compartment` restriction (when non-nil) is enforced identically on the current-state
path and the history path — same predicate, same relation shape, not a current-row check that the
history query inherits by reference.**
Closes: Attacks 6, 7 and 8, which are three independent proofs of the same root cause — see HIST-1
below for the mechanism; stated here because it is the authorization-layer half of the same invariant:
`rows returned under a Grant must satisfy that Grant's Compartment for the row *actually returned*, not
for whatever row shares its `rid`.`

**AUTH-3. The post-read defense-in-depth assertion (every returned row's `project`, and, where the
Grant carries one, `Compartment`, matches some Grant in the Scope) runs on every read and search entry
point uniformly — `Read`, `ReadVersion`, `ListVersions`, `Search` — not on `Search` alone.**
Closes: Attack 3 (the assertion "has nothing to look up" on a scope-free `Read` — resolved by the
frozen contract now always passing a Scope, but the assertion itself must actually be *implemented* on
all four methods, which the frozen interface does not itself guarantee) and Attack 9 (names this "the
one runtime control that would have caught" the tautology break, and notes its absence from
`authorization.md` while `linked-projects.md` specifies it only for `Search`).
Mechanism: one shared post-read check, called from all four `ResourceRepository`/`VersionStore` method
implementations in `packages/storage/pocketbase`, asserting each returned record's `(project, type)`
(and Compartment, if the satisfying Grant carried one) is permitted under the Scope passed in; violation
fails the whole request 500 + writes an audit alarm row, unconditionally.

**AUTH-4. Bulk Data export, GraphQL, subscriptions, Bots and async jobs mint their Scope the same way
an ordinary FHIR request does — through `authz.Authorize` — and never construct a `Scope`/`Grant`
directly, even inside their own package.**
Closes: `authorization.md` risk 8's own named risk ("if any of them grows its own query builder... the
invariant dies quietly") combined with Attack 3 S2 and CP-7 — a second, package-local `NewScope` call
site anywhere is exactly the shape CP-7's architecture test must catch, and this constraint exists so
the test's scope (all packages, not just an assumed handful) is written correctly from day one.
Mechanism: an architecture test asserting `SearchRepository`/`ResourceRepository`/`VersionStore` are the
only paths from any of these surfaces to storage, i.e. no package outside `packages/authz` imports
`packages/storage`'s `NewGrant`/`NewScope` (this is the same test as CP-7; listed again here because it
is the concrete enforcement for `authorization.md` risk 8, which names surfaces beyond what CP-7's
attack citation covers).

**AUTH-5. A policy rule that redacts a field on read must also remove that field's search parameter
from the same principal's allowed parameter set; this coupling is enforced at policy-validation time.**
This is the design author's own named risk in `authorization.md` (§ Risks, item 3), not an attack
finding, but it is load-bearing and easy to miss: field redaction cannot be a SQL predicate (you cannot
redact a field inside opaque JSON in SQL), so it is the one place post-fetch processing is unavoidable —
and if the principal can still *search* on the redacted field, the value is recoverable by binary search
against the index regardless of what the read response shows.
Mechanism: policy validation rejects a policy that redacts field `f` on type `t` for action `read` while
still granting `search` on any parameter resolving to `f` for the same `(t, principal)`. This must be a
validation-time rejection, not a documented caveat implementers are expected to remember.

## History

**HIST-1. `_history`, `vread` and `ListVersions` must authorize each *version* independently against
the Grant's Compartment — never authorize once against the current row and then serve every version
that shares its `rid`.**
Closes: three independently-arrived-at critical findings on the same root cause (Attacks 6, 7, 8):
`idx_compartment`/`idx_token` (or their successor, the Compartment field's backing index) are keyed by
`rid` with no version dimension, and write step 3 destructively rebuilds them from the *current*
content only. So a Compartment restriction — the only structural restriction `Grant` carries — can only
ever be evaluated against the newest version, no matter which version the read actually returns:
- Attack 8's cross-project case: a link scoped to `Observation` in a `final` state (via whatever
  mechanism eventually replaces the param-restriction gap noted above) still returns a `preliminary`
  version through `_history`, because history has no per-version fact to check against.
- Attack 6's Attack 2 and Attack 7's Step 3: a resource whose Compartment changed between versions (a
  wrong-patient documentation correction, or a soft-delete-then-recreate under a different Compartment)
  leaks every prior version's content to whoever currently satisfies the *latest* Compartment, because
  the prior Compartment's index row no longer exists anywhere.
- Attack 8's forgotten-surface note: `history-type` (`GET /fhir/R4/Patient/_history`) and
  `history-system` (`GET /fhir/R4/_history`) have no `rid` to resolve against at all, and are exactly
  the shape a later latency fix is likely to "optimize" by dropping the tenant column from an index's
  leading position (see SCH-1) — they must be enumerated and given a project-led index up front, not
  invented later under a latency ticket.
Mechanism: write a version-dimensioned Compartment relation in the same transaction as the version
itself (the existing `Transactor` contract already requires this class of same-txn write for the
current-state index):
```sql
CREATE TABLE idx_compartment_history (
  project_id TEXT NOT NULL, rid INTEGER NOT NULL, version_seq INTEGER NOT NULL,
  res_type TEXT NOT NULL, comp_type TEXT NOT NULL, comp_id TEXT NOT NULL,
  PRIMARY KEY (project_id, comp_type, comp_id, res_type, rid, version_seq),
  FOREIGN KEY (project_id, rid, version_seq)
    REFERENCES fhir_resource_history(project_id, rid, version_seq)
) WITHOUT ROWID;
```
`_history`/`vread` compile as a join against this relation with the same driving-relation and
project-anchoring rules as current-state search (SCH-1, SCH-2), never as "resolve `rid` under scope,
then fetch history unconditionally." A tombstone version must inherit its pre-image's Compartment
projection, or deletes disappear from `_history` for every Compartment-restricted principal (this is
the version-dimensioned counterpart of SCH-6's `identity_epoch`).
**Fallback if this is judged too expensive for Phase 2:** deny `_history`/`vread` outright to any Scope
whose satisfying Grant carries a non-nil `Compartment` — history is then available only under an
unconstrained Grant. This is a real product restriction, but it is honest; silently serving the
unconstrained answer (today's behavior) is not.

**HIST-2. `history-type` and `history-system` are enumerated in the schema up front, with a project-led
composite index, not added later under a performance ticket.**
Closes: Attack 8's secondary finding — the tempting fix for `WHERE project_id=? AND resource_type=?
ORDER BY last_updated DESC` is an index on `(resource_type, last_updated)`, dropping `project_id` from
the lead "because project is already guaranteed by the row"; combined with SCH-3's plan-guard fix this
is caught, but only if the index and the endpoints exist to be tested against.
Mechanism: `CREATE INDEX ... ON fhir_resource_history(project_id, resource_type, last_updated DESC,
rid, version_seq)` created in the Phase 2 migration alongside `idx_compartment_history`, with
`storagetest` cases for both endpoints from the start.

**HIST-3. `history_from` (a link's floor on how far back a grantee may read) is a bound predicate in the
same compiled statement as every other authorization term on the history path — never a filter applied
in a handler after rows are fetched.**
Closes: Attack 2's Variant A2 and Attack 6's Attack 1 — both show `history_from` living only on the
`ProjectLink` row with nothing on the by-key/history read path to consult it, so every version predating
a link's activation is returned once the by-key path is reachable at all (closed generally by the
frozen Scope-taking contract, but the predicate itself must still be wired into HIST-1's compiled join,
not left as a TODO once the join exists).
Mechanism: `AND h.last_updated >= :branch_history_from` in the same `idx_compartment_history` join as
HIST-1, sourced from the link's `history_from` column, defaulted to `activated_at` per
`linked-projects.md`.

**HIST-4. A cursor over a cross-project (linked) result set is invalidated, not silently narrowed or
widened, if the resolved link set changes between pages.**
Closes: `linked-projects.md`'s own named risk, reinforced by Attack 5's core finding that link state
can change (revoke, expire) mid-session — without this, page 2 of a paginated cross-project search can
silently serve a different, potentially wider, set of projects than page 1 did.
Mechanism: the cursor carries a hash of the resolved link set (project ids + statuses) alongside
HIST-1/SCH-8's opaque position; a mismatch on the next page returns 400, not a silently different
result.

## What to regression-test once, not per-feature

The nine reviews converged on a short list of properties that, if asserted once as `storagetest` cases
covering both backends, catch most of the above by construction rather than by code review:

1. Cross-project denial for every `ResourceRepository`/`VersionStore` method (not `Search` alone).
2. Empty-Scope denial for every method.
3. `scope.Allows` false for a type/action a Grant was never minted for, exercised through
   `_include`/`_revinclude`, not only through direct `Search`.
4. Bound-`project`-parameter count equals joined-relation count, per compiled arm (SCH-2).
5. `EXPLAIN QUERY PLAN` contains neither a bare `SCAN` on the compiler's own alias nor `ANY(` (SCH-3),
   asserted on every compiled statement shape, including history.
6. Recreating a soft-deleted logical id exposes no version written before the delete (SCH-6).
7. Create against an id the caller cannot read is indistinguishable from create against a free id (SCH-5).
8. The derived `project_link` relation's active rows always equal the set recomputed from current
   `ProjectLink` versions (LNK-2).
9. No capability grows across a chain of administrative links that it did not have across one hop (LNK-9).
10. A version whose Compartment differs from its resource's current Compartment is denied to a
    principal who only satisfies the current one (HIST-1).
