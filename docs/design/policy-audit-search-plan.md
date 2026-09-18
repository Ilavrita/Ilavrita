# Access policies, audit and search: the implementation order

Three pieces remain between this build and a usable FHIR server, and they have a
dependency order that is not the order they are usually asked for. This document
records that order, the constraints each piece has to satisfy, and the facts about
the existing code that were expensive to discover — so the next implementation
starts from them rather than rediscovering them.

Testable rules are numbered `POL-n`, `AUD-n` and `SRC-n`, following the
`SCH-`/`CP-`/`LNK-`/`AUTH-`/`HIST-`/`IDN-`/`REST-`/`CAP-` convention.

**Section 2 is implemented.** POL-1 through POL-9 are in the tree, with the
deviations §2.4 records. Audit and search are not.

## 1. Why policies come before search

Search returns sets. Every other interaction names one resource, so a coarse
policy costs one over-broad answer; a coarse policy behind search is a bulk
disclosure surface.

Two things are missing from the model, and one thing is not:

**Not missing.** A set of compartments *is* expressible. `AccessPolicy.Compile`
(`packages/authz/policy.go`) emits one `storage.Grant` per matching rule, so a
policy naming twenty patients in twenty rules compiles to twenty Grants and the
storage layer ORs their predicates. This was previously recorded as a capability
gap and it is not one. It is an O(n) gap: a clinic-wide role needs one rule per
patient, and the row count is the roster.

**Missing: a filter.** "Share only `status=final` Observations" is not
expressible. `Rule` carries a resource type, an action and at most one
compartment, and nothing else.

**Missing: field-level restriction.** "Share an Observation without its `note`" is
not expressible either. Every Grant returns whole resources.

## 2. Access policies

### 2.1 Filters

**POL-1.** A rule may carry zero or one filter, and a filter narrows: a rule with
one authorizes a subset of what the same rule without one authorizes. Nothing a
filter can say may widen a Grant, so a malformed or unreadable filter denies
rather than being dropped.

**POL-2.** A filter names one element path, one comparator and one value set. The
first version supports equality and set membership on a dotted path of up to
four segments — enough for `status`, `category.coding.code` and `class.code`. A
path this server cannot read is POL-1's denial, never a match.

A path crosses whatever shape FHIR chose. `category.coding.code` passes through
two arrays and `class.code` through none, and the author writes neither fact:
each hop normalises what it finds to an array before iterating it. `json_extract`
alone cannot do this — it does not traverse arrays, so a naive dotted path would
have matched nothing for exactly the example this rule names.

**POL-3.** The filter is applied in the storage query and not after it. A row
fetched and then discarded has already been read, and the count of what was
discarded is itself a disclosure. SQLite reads the element with `json_extract`
against `fhir_resource.content`; PostgreSQL uses the equivalent operator, named in
a `sqlite-only:` comment the way the schema already handles portability.

**POL-4.** A filter travels on the Grant, so storage receives it the way it
receives a Compartment: `storage.Grant` gains a `Filter *Filter` field, the arms
compile one more predicate, and `authorizedGrants` is unchanged. A Grant carrying a
filter the backend cannot compile is refused at compile time, not ignored.

**POL-5.** A filter on a write is checked against the submitted resource, the way a
compartment now is (`covers`). Writing a row the filter would then hide is a write
into a hole, refused for the same reason a resource landing in no compartment is.

Schema: `access_policy_rules` gains `filter_path TEXT`, `filter_comparator TEXT`
and `filter_values TEXT` (a JSON array), with a CHECK that the three are all NULL
or all set, mirroring the compartment columns' own paired CHECK.

### 2.2 Field-level restriction

**POL-6.** A rule may name the elements it returns. An unnamed element is absent
from the response rather than empty, so a reader cannot tell a withheld value from
one nobody recorded.

**POL-7.** `id`, `resourceType` and `meta.versionId` are always returned. A
resource that cannot be addressed or version-checked is not usable, and
withholding them buys nothing: the reader already holds the row.

As built, `meta` is returned whole rather than only its `versionId`. It is the
server's own bookkeeping rather than clinical content, and a version id without
the instant beside it is a record a client cannot reason about. This is a
superset of what POL-7 requires, and it is the one place the implementation
returns more than the rule asks for.

**POL-8.** Restriction happens on the way out, in one place, applied to every route
that returns a resource. It is the one rule here that cannot be a query predicate,
so it must not be scattered across handlers.

### 2.3 Compartment sets

**POL-9.** A rule may name several compartment ids, compiling to one Grant each.
This removes the row-per-patient roster without altering what is expressible.

### 2.4 What the implementation added that this document did not ask for

- **A write is checked against the content it submits, for filters as well as
  compartments.** POL-5 said this; what it did not say is that the same
  generated predicate serves both, run against a bound body instead of a stored
  row. A second evaluator written in Go would have been a second rule, and the
  two would have disagreed the first time either changed.
- **Projections are decided per row, not per Scope.** A caller holding one
  patient's chart in full and another's status alone must not read the second
  patient in full because the first Grant exists. Which Grants reach a row is
  worked out against that row's own placement and content, and only those
  Grants have a say in how much of it comes back.
- **A caller that reads part of a resource may not replace all of it.** An
  update replaces content wholesale, so a partial reader performing the ordinary
  read-modify-write would silently drop every element their own policy withheld.
  That is data loss produced by an authorization rule, so the write is refused
  rather than answered. A caller with no read Grant at all is not blind in this
  sense and is unaffected.

## 3. Audit

`packages/audit` is a doc comment. Nothing records that anyone authenticated, read
a record, or changed a policy.

**AUD-1.** An audit record is written in the same transaction as the thing it
describes, or not at all. A separate write is one that can be lost exactly when it
matters.

**AUD-2.** An audit record holds no credential, no token, no password hash and no
resource content. It holds who, what, which resource key, when, and the outcome.
The FHIR `AuditEvent` resource is the shape to grow into; the table is the record
of truth.

**AUD-3.** A failed authorization is audited as loudly as a successful read. A log
recording only what succeeded cannot answer the question an incident asks.

**AUD-4.** Audit rows are append-only, enforced by the trigger pattern the history
tables already use (`fhir_resource_history_no_update`).

**AUD-5.** The login route audits the attempt and its outcome, never the password
and never which of the four checks refused it — the route already answers
uniformly, and the audit must not undo that by recording more than the answer.

Schema: `audit_events(project_id, id, at, principal_kind, principal_id,
membership_id, action, res_type, res_id, outcome, detail)`, tenant column first,
append-only trigger, and an index on `(project_id, at, id)`.

## 4. Search

**SRC-1.** `GET /{type}?...` and `POST /{type}/_search` with a form-encoded body
are the same search and must resolve identically. `POST /_search` is the FHIR form;
the HTTP `QUERY` method is an IETF draft and may be added as an alias, but nothing
may depend on it.

**SRC-2.** A search result is a `searchset` Bundle with `self`, and `next` only
when a further page exists. `total` is present only when it was computed, because
an absent total and a wrong one are very different promises.

**SRC-3.** Every search runs inside the Scope. The compartment predicate the by-key
routes already compile is the same one a search compiles: search adds parameters to
the WHERE clause and removes nothing from it.

**SRC-4.** A parameter this build does not implement is refused, never ignored. A
search that silently drops `status=final` returns more than it was asked for, and
the caller cannot tell.

**SRC-5.** Typed indexes are projected on write, in the transaction that writes the
resource, exactly as `fhir_resource_compartment` now is. The compartment projection
is the pattern: derive from the content, write beside the row, read through a
predicate.

**SRC-6.** `packages/search` holds no SQL. It turns a query into a plan; the backend
executes it. This is what its doc comment already says and what the `depguard`
boundary already enforces.

## 5. What this work learned that the next implementation should not rediscover

- **The compartment table had no writer.** `fhir_resource_compartment` was
  declared, had a predicate compiled against it and a history projection reading
  from it, and nothing ever inserted a row — so every confined Grant read nothing.
  Search meets the same class of problem with index tables: declare the projection
  and the writer together, or the predicate is decorative.
- **A pre-check in the HTTP layer duplicated a storage rule and outlived it.** The
  route layer refused every confined write because a new row had no compartment;
  storage later knew better, and the stale twin silently won. One place decides.
- **Confinement is per dimension.** A Grant naming `Patient/x` governs the Patient
  compartments a resource lands in and says nothing about the Practitioner or
  Encounter it also names. The first version of `covers` required all of them and
  refused nearly every real observation.
- **A create is checked against what the resource declares, not what exists.** There
  is no row to read yet, so the submitted content is the only evidence.
- **`ON CONFLICT` cannot target a partial unique index** without repeating the
  index's predicate, and SQLite refuses the statement rather than ignoring it.
- **SQLite's `LIKE` is ASCII-case-insensitive**, so `substr(id, 1, 4) = 'cli_'` and
  `id LIKE 'cli\_%'` are not the same check.
- **FHIR logical ids disallow underscores.** A fixture id carrying one fails in a
  way that looks like an authorization bug.
- **An update did not re-place a resource in the compartments it states.** The
  route layer derived them and storage discarded them, so after an update the
  patient a resource had moved away from went on reading it and the patient it
  moved to could not. The projection is now replaced by every write that states
  new content, and kept by a delete, which states none.
- **`json_extract` does not traverse arrays.** A dotted path over FHIR needs a
  hop that normalises what it finds to an array first, and `json_each` hands a
  scalar element back as an SQL scalar rather than as JSON — which the next hop
  reads as malformed and fails the whole query on, one odd row refusing every
  row beside it. `json_quote` carries it back.
- **A nested query inside an open cursor deadlocks this pool.** It holds one
  connection, so a per-row query issued while rows are still open waits for a
  connection only closing those rows can release. Collect first, then enrich.
- **`gh auth token --user` is not enough to push as that user.** git tries every
  configured credential helper in order and gh installs one bound to the active
  account, so the helper list has to be cleared in the same command that sets
  the transient one.
