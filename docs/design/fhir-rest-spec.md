# FHIR REST interaction specification

This document specifies the piece of the control plane that does not exist yet: the HTTP handlers
that sit between `apps/ilavrita`'s router and `packages/storage`'s `ResourceRepository` and
`VersionStore`, for the six single-resource interactions — create, read, vread, update, delete,
history-instance — plus the principal resolver those handlers need before they may call
`authz.BuildScope` at all. Everything it reads is frozen and already committed —
`packages/storage/{repository,scope,errors,record,identifier}.go`, `packages/authz/{scope,policy}.go`,
`packages/storage/pocketbase/resource.go`, `packages/fhir/{outcome,capability,release}.go`,
`apps/ilavrita/{fhir,runtime,operational}.go`, `api/openapi.yaml`, `docs/architecture.md`,
`docs/known-limitations.md` — and this document treats all of it as binding wherever it speaks.
Nothing here is implemented; this is a specification precise enough that conformance tests can be
written straight from it, without a second draft.

Where this document makes a design call the committed code and schema leave open, it is marked
**(synthesis)** so an implementer can tell settled fact from this document's own decision. Testable
rules are numbered `REST-n`, a new prefix — the `SCH-`/`CP-`/`LNK-`/`AUTH-`/`HIST-` families in
`phase2-constraints.md` and `IDN-` in `user-spec.md` cover storage, authorization and identity; this
document is the first to speak about the HTTP surface itself, so it gets its own series. Every FHIR
citation is `§n.n.n.n` against the R4 RESTful API chapter, <https://hl7.org/fhir/R4/http.html>.

Two things this document is not: it is not `docs/design/authz-spec.md` (which specifies
`authz.BuildScope` itself — this document only specifies what `Request` a route builds and how); and it
is not a search or Bundle specification — `_history` here means the plain, unparameterized
instance-level history read only, matching `docs/known-limitations.md`'s "Search, of any parameter
type: Not implemented."

## 1. Where this sits

```
client
  |
  v
apps/ilavrita  (route match: method + {resourceType} + {id} + {vid}/_history)
  |  -- principal resolution (§6) --> project.PrincipalRef, project.ID
  |  -- authz.BuildScope (docs/design/authz-spec.md) --> storage.Scope
  v
packages/storage.ResourceRepository / VersionStore   (frozen, packages/storage/repository.go)
  |
  v
packages/storage/pocketbase.ResourceStore            (frozen, packages/storage/pocketbase/resource.go)
```

Six new route handlers are added to `apps/ilavrita`, registered on the same `base :=
routes.Group(fhir.BasePath)` group `registerFHIRRoutes` already builds, ahead of the existing
`base.Any(everythingElse, rejectUnimplemented)` catch-all (route specificity, not order, must decide
this — a router that tries the wildcard first would swallow every new route). `describeCapabilities`
and `rejectUnimplemented` are unchanged; `respondFHIR` is reused and extended (it already centralises
`Content-Type: application/fhir+json` — the new handlers add `ETag`/`Location`/`Last-Modified` via
`request.Response.Header().Set` before calling it, the same way `respondFHIR` itself sets
`Content-Type`).

## 2. Route table

| Interaction | HTTP | Path | Storage call | `Grant.Action` | R4 § |
| --- | --- | --- | --- | --- | --- |
| create | `POST` | `/fhir/R4/{resourceType}` | `ResourceRepository.Create` | `ActionWrite` | §3.1.0.8 |
| read | `GET` | `/fhir/R4/{resourceType}/{id}` | `ResourceRepository.Read` | `ActionRead` | §3.1.0.2 |
| vread | `GET` | `/fhir/R4/{resourceType}/{id}/_history/{vid}` | `VersionStore.ReadVersion` | `ActionHistory` | §3.1.0.3 |
| update | `PUT` | `/fhir/R4/{resourceType}/{id}` | `ResourceRepository.Update`, or `.Create` (§3.5) | `ActionWrite` | §3.1.0.4 |
| delete | `DELETE` | `/fhir/R4/{resourceType}/{id}` | `ResourceRepository.Delete` | `ActionDelete` | §3.1.0.7 |
| history-instance | `GET` | `/fhir/R4/{resourceType}/{id}/_history` | `VersionStore.ListVersions` | `ActionHistory` | §3.1.0.12 |

**REST-1.** vread and history-instance authorize under `ActionHistory`, never `ActionRead`, matching
`AUTH-1` in `phase2-constraints.md` and the doc comments on `ResourceStore.ReadVersion`/`ListVersions`
in `packages/storage/pocketbase/resource.go`. A `Grant{Action: ActionRead}` alone does not authorize
either route: a principal can be readable-current but history-blind, or vice versa, and a test must
assert both directions independently (a Scope with only `ActionRead` gets 403/404 on vread; a Scope
with only `ActionHistory` gets 403/404 on plain read).

**REST-2.** `{resourceType}` is checked against the CapabilityStatement's own declared resource list
(§7) *before* `authz.BuildScope` is ever called, on every one of these six routes, not only create. An
unrecognised type is `404 not-found` (§3.1.0.8's "resource type not supported, or not a FHIR end-point"
generalises to every interaction here), and no Scope is built, no storage call is made, and no
principal-resolution failure can be reported ahead of it — a request for `/fhir/R4/NoSuchType/1` gets
404 the same whether or not a principal is configured. This exists because `storage.ResourceType` is
"just a string" (`packages/storage/identifier.go`): nothing in `ResourceRepository` refuses an
unrecognised type, so the HTTP layer is the only place that can.

## 3. Interactions: status codes and headers

Every rule below assumes `Content-Type`/`Accept` negotiation (§3.7) has already passed and the
principal has already been resolved (§6) — those two checks run first, in that order, and are shared
across all six routes.

### 3.1 create — §3.1.0.8

**REST-3.** `201 Created`, always, on success — never `200`. Body is the created resource
(`Prefer: return=minimal`/`return=OperationOutcome` handling is **(synthesis, deferred)**: honouring
`Prefer` is a client-visible refinement layered on top of this baseline, not required for the
status/header contract itself).

Headers, all required on `201` (§3.1.0.8, §3.1.0.1.3):

| Header | Value |
| --- | --- |
| `Location` | `{baseURL}/{resourceType}/{id}/_history/{versionId}` — always the versioned form; Ilavrita is always a versioning server (`storage.VersionID` is mandatory on every record), so the unversioned `Location` form §3.1.0.8 allows for non-versioning servers never applies here. |
| `ETag` | `W/"{versionId}"` — always `"1"` for a brand-new id; the tombstone-recreate case (§3.5) issues whatever `version_seq` the row reaches, never `"1"` again (`identity_epoch` changes, `version_seq` does not reset — see `packages/storage/pocketbase/resource.go`'s `recreate`). |
| `Last-Modified` | `record.LastUpdated`, HTTP-date (RFC 7231 §7.1.1.1) formatted. |

Structural checks, both `400 invalid` (FR-019), both before `ResourceRepository.Create` is called:

**REST-4.** the request body's own `resourceType` field must equal the URL's `{resourceType}` segment.

**REST-5.** the request body must carry no `id` (create always mints or is handed one by the route, per
FR-002's "assign or validate logical identifiers as defined by the server contract" — **(synthesis)**:
Ilavrita's v0.1 contract is server-assigned ids only for `POST`; a body-supplied `id` on create is a 400,
not silently honoured or silently dropped).

Error mapping specific to create (§5 gives the general table): `storage.ErrDenied` → `403 forbidden`
(§7's capability-level case); `storage.ErrAlreadyExists` → `409 duplicate` (only reachable if the
route's id-minting scheme collides, or under the tombstone race in §3.5).

### 3.2 read — §3.1.0.2

**REST-6.** `200 OK` with the current resource on success.

| Header | Value |
| --- | --- |
| `ETag` | `W/"{versionId}"` of the record actually returned. |
| `Last-Modified` | `record.LastUpdated`, HTTP-date formatted. |

**REST-7.** `storage.ErrNotFound` → `404 not-found`; `storage.ErrDeleted` → `410 deleted`. §3.1.0.2 states
this distinction in exactly these terms ("a `GET` for a deleted resource returns a `410` status code,
whereas a `GET` for an unknown resource returns `404`"), and `packages/storage/errors.go`'s two sentinels
exist for exactly this reason — the two status codes are a direct, one-line translation of the two
errors, not a decision this document invents.

**REST-8.** an out-of-scope row (wrong Project, wrong compartment) and a genuinely absent row are both
`storage.ErrNotFound` from `ResourceStore.Read` — see `packages/storage/pocketbase/resource.go`'s own
comment: "A row outside the Scope is reported as missing, so a caller cannot probe for ids it may not
read." Both are `404` here, identically, by construction: there is no separate code path to give them a
different status even if a handler wanted to (SCH-5's create-side non-enumerability rule and this rule
are the same shape applied to read).

Conditional read (`If-Modified-Since`, `If-None-Match` → `304`, §3.1.0.1.7) is **out of scope** for this
document — it is a caching optimisation layered on top of the `200` path above, not a distinct status
this document's error table needs to reason about, and nothing in `docs/known-limitations.md` demands
it for v0.1.

### 3.3 vread — §3.1.0.3

**REST-9.** `200 OK`. `ETag: W/"{vid}"` where `{vid}` is *the URL's own version id, verbatim* — not the
resource's current version. A test: update a resource twice, then vread version 1; the response `ETag`
must read `W/"1"`, never `W/"3"`, even though the resource's current `ETag` (via plain read) is now
`W/"3"`. `Last-Modified` is that specific version's `LastUpdated`, not the current record's.

**REST-10.** `storage.ErrNotFound` → `404 not-found` (§3.1.0.3: "if a request is made for a previous
version of a resource, and the server does not support accessing previous versions" — here, "does not
support" collapses onto "this Scope/vid combination matched no row," since Ilavrita either has the
version or the vid names nothing reachable). `storage.ErrDeleted` → `410 deleted`, when the *named
version itself* is the deletion — i.e. vreading the tombstone version directly, as opposed to plain
read's `410` which fires on the *current* row being a tombstone regardless of which version was asked
for.

**REST-11.** (synthesis, optional fast path) `{vid}` is validated as a plain unsigned base-10 integer
literal — matching `version_id`'s own construction, `CAST(version_seq AS TEXT)` in
`packages/storage/pocketbase/resource.go` — before it is passed to `VersionStore.ReadVersion` as a
`storage.VersionID`. A syntactically malformed `{vid}` (`"abc"`, `"1.0"`, `"-1"`, leading zeros) can
never match a stored row, so rejecting it as `404 not-found` without a query is an allowed optimisation,
never a required one — a handler that skips this check and lets the query itself return zero rows
produces the identical `404`.

### 3.4 delete — §3.1.0.7

**REST-12.** `204 No Content`, no body, on success (§3.1.0.7's three-way choice of `200`/`202`/`204` is
narrowed here: Ilavrita's delete is synchronous and has no representation worth returning, so `204` is
the fixed answer — **(synthesis)**, choosing among R4's own options rather than adding one).

**REST-13.** `ETag` on the `204` response is optional (§3.1.0.7: "servers MAY include an `ETag`... to
allow version contention management when a resource is brought back to life"). When present, it is
`W/"{versionId}"` of the tombstone version the delete just wrote — the version a subsequent
update-as-create (§3.5) would need to have known about only if it wanted to detect the resurrection race,
which it does not (recreate has its own independent conflict handling). No `Last-Modified` or `Location`
on delete.

**REST-14.** subsequent plain reads of the same id return `410`, per REST-7 — this is the same rule, not
a new one; delete's only job is to make the row a tombstone that `Read` already knows how to report
correctly.

Error mapping specific to delete: `storage.ErrDenied` (no delete grant at all for this Project+type) →
`403 forbidden`. `storage.ErrNotFound`/`ErrDeleted`/`ErrVersionConflict` are governed by the unified
precondition rule, REST-17, shared with update.

### 3.5 update — §3.1.0.4

**REST-15.** `200 OK` if the write replaced an existing live resource; `201 Created` if the write
brought a new or previously-deleted logical id into existence. The discriminator is *which storage
method actually ran*, not a separate decision: `200` ⟺ `ResourceRepository.Update` ran and returned no
error; `201` ⟺ `ResourceRepository.Create` ran (including its internal tombstone-recreate branch) and
returned no error. A handler that gets this right needs no extra state — the two storage calls are
mutually exclusive and the status code is just which one it happened to make.

**REST-16 — the update algorithm.** Because `ResourceRepository.Update` and `.Delete` both require a
non-empty `expect storage.VersionID` (`packages/storage/pocketbase/resource.go` rejects `expect == ""`
before ever building SQL) and neither interface exposes a way to read "current version under my *write*
scope" other than by attempting a write, the handler must always learn the current state through a
`ResourceRepository.Read` call first — even when the client sent no `If-Match` at all:

1. `record, err := repo.Read(ctx, scope, key)`.
2. **`err == nil`** (record exists, live): if `If-Match` was sent and its parsed version disagrees with
   `record.Version`, stop here and return `412` (REST-17) — no write is attempted. Otherwise call
   `repo.Update(ctx, scope, record, record.Version)`.
   - success → `200`.
   - `ErrVersionConflict` → a concurrent writer won a race between this handler's `Read` and its
     `Update`; the client's own belief (present or absent) was correct at check-time, so this is `409`,
     never `412` (REST-17).
   - `ErrDenied` → `403`.
3. **`err == ErrDeleted`** (tombstone): if `If-Match` was sent, return `412` (a client claiming a version
   against a resource this Scope currently sees as deleted can never be corroborated). Otherwise call
   `repo.Create(ctx, scope, newRecordForID(key))`, which recreates over the tombstone.
   - success → `201`.
   - `ErrAlreadyExists` (something else recreated it first) → `409 duplicate`.
   - `ErrDenied` → `403`.
4. **`err == ErrNotFound`** (never existed, or existed outside this Scope): if `If-Match` was sent,
   return `412` — the client asserted a version must match and nothing is even visible, which is at
   least as strong a mismatch as "wrong version." Otherwise call `repo.Create` (update-as-create).
   - success → `201`.
   - `ErrAlreadyExists` → `409 duplicate` (a race: the row appeared between step 1's `Read` and this
     `Create`).
   - `ErrDenied` → `403` — this specifically means no write grant exists for this Project+type at all,
     since `Create`'s own precondition (`packages/storage/pocketbase/resource.go`) already requires a
     matching `ActionRead` grant too, and step 1's `Read` having returned `ErrNotFound` (rather than
     erroring outright) already established the caller could at least attempt to see the id.

**REST-17 — the 412-vs-409 rule (shared by update and delete).** `412 Precondition Failed` fires
if and only if the client sent `If-Match` *and* the state the handler actually observed (absent,
deleted, or a different live version) disagrees with what `If-Match` claimed. Every other version
failure — no `If-Match` was sent at all, or it was sent and *did* agree with what the handler observed,
but a concurrent write then won the race before the handler's own write committed — is `409 Conflict`.
This is one rule applied identically to update's step 2/3/4 above and to delete's equivalent read-then-
write sequence; a test suite needs exactly two scenarios per interaction to cover it: (a) stale
`If-Match` against a live resource → `412`, no storage write attempted; (b) two concurrent unconditional
writers racing the same id → the loser gets `409`.

`If-Match: *` (synthesis) is treated identically to no `If-Match` header: it claims only "some version
currently exists," which the unconditional path already establishes or refutes on its own, so it adds no
information REST-16's algorithm does not already have.

Headers on `200`/`201`, both required (§3.1.0.4):

| Header | Value |
| --- | --- |
| `ETag` | `W/"{versionId}"` of the version the write just produced. |
| `Last-Modified` | that version's `LastUpdated`. |
| `Location` | only on `201`, same form as create's (REST-3). |

**REST-18.** structural checks, both `400 invalid`, both before any storage call: the body's
`resourceType` must equal `{resourceType}`; the body's `id` (if present) must equal `{id}` (FR-019 — the
same structural-consistency family as REST-4/REST-5, extended to the two fields update actually cares
about).

**REST-19.** update-as-create has no `405` case here (synthesis, and a genuine constraint, not a
preference): §3.1.0.4.1 lets a server refuse client-defined ids and answer `405` instead of creating,
but `ResourceRepository.Create` has exactly one code path for "this id does not currently exist as a
live row" and cannot distinguish "the id arrived via `POST` without one" from "the id arrived via `PUT`
naming one the client chose" — both are the same call with the same `record.Key.ID` already set. A
per-type "reject client ids" policy would have to be enforced in the HTTP handler, refusing update-as-
create before ever calling `Create`; nothing in the committed storage layer offers a hook for it, and
this document does not add one. Until such a policy exists, `405` is unreachable on this route.

### 3.6 history-instance — §3.1.0.12

**REST-20.** `200 OK`, body a `Bundle{type: "history"}`, entries ordered newest-first (`ListVersions`
already returns `ORDER BY version_seq DESC` — the Bundle serialiser must not re-sort).

**REST-21.** `storage.ErrNotFound` → `404 not-found`. This is `ListVersions`'s own behaviour
(`packages/storage/pocketbase/resource.go`: "A resource with no visible version is reported as missing"
— `len(records) == 0` is the *only* way `ListVersions` fails not-found; there is no separate "exists but
scope sees nothing" case to special-case, because those collapse to the same empty result). Unlike plain
read, history-instance never returns `410` on its own: a resource whose entire visible history ends in a
delete still has at least one visible version (the tombstone's own history row, which `ListVersions`
does not filter out — only `Read`/`ReadVersion` do, by checking `Deleted` after the fact), so it is `200`
with a Bundle whose newest entry documents the deletion, never `410` for the collection endpoint itself.

**REST-22 — reconstructing `entry.request.method`.** `storage.ResourceRecord` carries no field naming
which HTTP interaction produced a version, so the Bundle serialiser derives it from the record shape
alone, using facts already true of `packages/storage/pocketbase/resource.go`'s write path:

- the **oldest** record in one `ListVersions` result (i.e. the *last* entry, since results are
  newest-first) is always `POST` — every version sequence for one still-current identity epoch begins
  with a `Create` (a fresh id, or a tombstone recreate; `recreate` bumps `identity_epoch`, so an older
  epoch's versions are never mixed into the same result — this is the same epoch fencing SCH-6 exists
  for).
- any record with `Deleted == true` is `DELETE`.
- every other record is `PUT`.

Per §3.1.0.12, a `POST` or `PUT` entry SHALL carry `entry.resource`; a `DELETE` entry has none. This
falls out of the storage layer for free: `writeVersion` stores `content = NULL` exactly when
`Deleted == true` (`packages/storage/pocketbase/resource.go`), so "`entry.resource` present" and
"`Content != nil`" are the same fact, and the serialiser needs no extra branch beyond REST-22's own
three cases.

**REST-23.** `_count`, `_since`, `_at`, `_list` (§3.1.0.12's paging/filter parameters) are **out of
scope**: `docs/known-limitations.md` states "Search, of any parameter type: Not implemented," and
history's own query parameters are a search-shaped mechanism. A request carrying any of them today gets
the same unfiltered `200` Bundle as one without — silently ignoring an unsupported parameter here is
consistent with the rest of this phase, not a new leniency this document invents.

## 4. ETag ⇄ VersionID

**REST-24 — emitting.** Every `ETag` this server ever sends is `W/"` + `string(record.Version)` + `"`,
always weak, always double-quoted, the version id verbatim with no reformatting (`version_id` is already
`CAST(version_seq AS TEXT)` — a bare decimal string with no leading zeros, so there is nothing to
normalise). §3.1.0.1.3 requires the weak form because the byte-for-byte JSON serialisation is not
guaranteed stable across requests even when the content hasn't changed (field ordering, whitespace);
Ilavrita is a JSON-only server today (`fhir.ContentType`), so this matters even before XML is ever
considered.

**REST-25 — parsing (`If-Match`).** Input: the raw header value.
1. Trim surrounding whitespace.
2. Strip a leading `W/` if present — accept it whether present or absent, never require it. R4 mandates
   servers *send* weak tags; it does not forbid a client from sending a bare strong-looking one, and
   since version-id equality here is exact string equality regardless of the weak marker, the two read
   identically.
3. What remains **must** be exactly one `DQUOTE token DQUOTE` (a single quoted value, no comma-separated
   list — `If-Match` only ever needs one instance version). Anything else — unquoted text, multiple
   values, an empty string — is `400 invalid` before any storage call.
4. The unquoted token becomes `storage.VersionID(token)`, used as `expect` in REST-16/REST-17's
   algorithm, or compared to `record.Version` there.

`If-None-Match` (conditional *read*, §3.1.0.1.7, for `304` caching) is a distinct mechanism from
`If-None-Exist` (conditional *create*, §3.1.0.8.1) and from `If-Match` above; this document does not
specify it (§3.2 already notes conditional read is out of scope) but calls the naming collision out
explicitly so an implementer does not conflate the two headers.

## 5. Error mapping

Every storage error maps to exactly one HTTP status and one `OperationOutcome` issue code; the
`diagnostics` string is always a fixed, generic sentence — never the wrapped Go error's own text, which
can carry a resource type, logical id, or Project id (`fmt.Errorf("...: %s/%s in %s", ...)` appears
throughout `packages/storage/pocketbase/resource.go`). `docs/architecture.md`'s "no PocketBase concept
reaches `/fhir/R4`" and FR-007 both already require this; this table is what makes it enforceable rather
than a matter of handler-by-handler discipline.

| Storage condition | HTTP | Issue code | Notes |
| --- | --- | --- | --- |
| body fails to parse as JSON; `resourceType`/`id` mismatch (REST-4/5/18) | 400 | `invalid` | before any storage call |
| malformed `If-Match` (REST-25 step 3) | 400 | `invalid` | before any storage call |
| no principal resolved (§6) | 401 | `login` | before any storage call; never reaches `BuildScope` |
| `storage.ErrDenied` | 403 | `forbidden` | capability-level: no Grant exists for this Project+Kind+Type+Action at all — never row-specific (§5.1) |
| `storage.ErrNotFound` | 404 | `not-found` | absent row, or a row this Scope cannot see — REST-8 |
| `{resourceType}` unrecognised (REST-2) | 404 | `not-found` | before `BuildScope` |
| `storage.ErrDeleted` | 410 | `deleted` | REST-7; needs an enum addition, §5.2 |
| `storage.ErrVersionConflict`, unconditional or raced (REST-17) | 409 | `conflict` | |
| `storage.ErrAlreadyExists` | 409 | `duplicate` | needs an enum addition, §5.2 |
| `storage.ErrVersionConflict`, `If-Match` present and mismatched (REST-17) | 412 | `conflict` | same issue code as the 409 case — the HTTP status is what carries the distinction, not the code |
| unsupported `Content-Type` on a body-bearing request (§3.7) | 415 | `not-supported` | |
| unsupported `Accept`/`_format` (§3.7) | 406 | `not-supported` | |
| unimplemented interaction (existing `rejectUnimplemented`) | 501 | `not-supported` | unchanged |
| `storage.ErrScopeEscape`, any `authz` wiring error (`ErrMissingResolver`, `ErrMissingInstant`), any unwrapped Go error | 500 | `exception` | §5.3 — never leaks the wrapped message |

FHIR profile/business-rule validation (`422`, FR-019–FR-021) is not reachable yet: no storage error this
phase produces corresponds to it, and `docs/known-limitations.md` already states structural/profile
validation is not implemented. This table has no `422` row because nothing on this document's frozen
inputs emits one; adding real `422`s is future work gated on FR-019, not a gap in this table.

### 5.1 403 is capability-level only, never row-specific

**REST-26.** `storage.ErrDenied` is returned by `ResourceStore` in exactly two shapes, both blanket
checks independent of any specific row: `Create`'s own precondition (no write grant for the type at all,
or every write grant is compartment-restricted so a *new* resource's compartment cannot be determined,
or no matching read grant — `packages/storage/pocketbase/resource.go`'s `Create`), and `mutate`'s
zero-grant guard for `Update`/`Delete` (`if len(authorizedGrants(scope, key, change.action)) == 0`,
checked *before* any query runs). Neither case can leak whether a *particular* id exists: they fire
identically for every id of that type in that Project, which is why `403` is safe to return here (it
reveals only "you may never do this to this type," never "this specific id exists and you may not touch
it"). Every row-specific denial (a Grant exists for the type, but this row's Project, compartment, or
version does not match it) instead falls through to `ErrNotFound`/`404` — see REST-8 — precisely to
avoid that leak. A test: two Scopes, one with zero Grants for `Patient` in a Project and one with a
compartment-restricted `Patient` Grant naming a different patient than the one requested; both requests
must reach `404`/`403` in the pattern this rule predicts, never the other way around, and never `403` for
the row-specific case.

### 5.2 Required `api/openapi.yaml` changes

**REST-27.** the `Issue.code` enum in `api/openapi.yaml` today is
`[not-supported, not-found, invalid, security, conflict, exception]` — it has no member for `410`'s
`deleted` or `409`'s `duplicate` (§5's table). Both are official R4 `issue-type` codes
(<https://hl7.org/fhir/R4/valueset-issue-type.html>: `deleted` — "The reference pointed to content...
that has been deleted"; `duplicate` — "An attempt was made to create a duplicate record") and neither
`not-found` nor `conflict` is a faithful substitute (reusing `not-found` for `410` would make `404` and
`410` indistinguishable to a client parsing only the body, defeating REST-7's entire point). Before any
handler in this document ships, `scripts/verify-openapi.sh` will fail until the enum gains both values —
this is a required, not optional, edit to that file, flagged here rather than left for the implementer
to discover from a red CI run.

**REST-28.** `login` (`401`) and `forbidden` (`403`) are likewise official R4 codes distinct from the
enum's existing catch-all `security`, and are more testable than collapsing both onto `security`: a test
asserting "no principal → `login`" and "wrong principal → `forbidden`" can tell the two failure modes
apart from the response body alone, without inspecting the HTTP status. This document's tables (§5, §6)
use `login`/`forbidden`; `api/openapi.yaml`'s enum needs both alongside `deleted`/`duplicate` from
REST-27.

### 5.3 The one rule that matters most: `ErrScopeEscape` never reaches a client body

**REST-29.** `storage.ErrScopeEscape` (`packages/storage/pocketbase/resource.go`) means a row was
returned that the compiled query's own predicates should have excluded — the one internal condition
that is direct evidence the Project-isolation boundary itself may already have failed (R-004, "the worst
possible failure"). Its response to the client is the same generic `500`/`exception` as any other
unexpected error — the wrapped message (which names the escaping row's type, id and Project) must never
appear in `Issue.diagnostics`, matching FR-007/FR-034 exactly like every other internal error. But
unlike an ordinary `500`, it must additionally be logged server-side at a distinct severity above plain
`error` — this document does not name the exact log level (that is `packages/observability`'s contract,
not this one's), but requires that whatever mechanism pages an operator for a security incident, and not
merely a request failure, is the one this condition triggers. A test can assert the client-visible half
of this (generic body, no leaked identifiers) directly; the alerting half needs an
`packages/observability`-level test this document only requires exist, not specifies.

## 6. Authentication: a deny-by-default principal resolver

**THE GAP**, stated in this document's own terms: no code path today turns an HTTP request into a
`project.PrincipalRef`. `authz.BuildScope`'s `Request.Principal` (`packages/authz/scope.go`) has no
current source. Building real authentication (sessions, tokens, OAuth/OIDC per FR-027) is out of scope
here; what this section specifies is the interim resolver every one of §3's six routes needs before any
of them can call `BuildScope` at all, and its default-off posture.

**REST-30.** absent any configuration, **every** route in §2 returns `401 login` before `BuildScope` is
ever invoked and before any storage call is made. This is not "authorization denies everything" (that
would still be `BuildScope` running and returning an empty `Scope`, which is a well-defined, already-
specified outcome producing `403`/`404` per §5.1) — it is "no mechanism exists to say who is asking," a
strictly prior question, so it must be answered strictly first. `GET /fhir/R4/metadata` is the one FHIR
route exempt from this: it names no resource, touches no storage, and R4 practice (and SMART's own
discovery flow) expects a `CapabilityStatement` to be reachable before a client has authenticated at
all — `describeCapabilities` is unchanged by this document.

**REST-31 — the env var.** `ILAVRITA_DEV_PRINCIPAL`, following the `ILAVRITA_*` convention already
established by `.env.example` and `ILAVRITA_EXPOSE_POCKETBASE`. Format (synthesis):

```
ILAVRITA_DEV_PRINCIPAL=<project-id>:<principal-kind>:<principal-id>
```

`<principal-kind>` ∈ `{user, client_application, bot}` (`project.PrincipalKind`, `packages/project/
membership.go`); `<project-id>` must pass `project.ValidateID`. Unset or empty → REST-30's `401` applies
to every request the process serves; nothing else about the resolver runs.

**REST-32 — fail closed on malformed configuration.** The value is parsed once, at process startup, the
same posture `AssertForeignKeysEnforced` (`packages/storage/pocketbase/schema.go`) already takes for a
different precondition: a malformed value (wrong arity, empty segment, unrecognised kind, or a project
id `ValidateID` rejects) makes the process refuse to start, rather than silently falling back to
REST-30's deny-by-default behaviour. A typo that silently degraded to "authentication is on but broken"
would be far more dangerous than one that refuses to boot — a process that won't start is loud in every
environment; a process serving `401`s where an operator expected a working dev principal is the kind of
thing that gets "fixed" by loosening something else instead.

**REST-33 — why default off.** The moment this mechanism exists, every request the process serves as
long as the variable is set is authenticated as one fixed, static identity with no credential check of
any kind — not weak authentication, *no* authentication, dressed as a principal. `ILAVRITA_EXPOSE_
POCKETBASE`'s own justification (`docs/architecture.md`, `.env.example`) is the same shape at smaller
stakes; here the stakes are R-004 itself — a `.env` copied from a development host to a production one
with this variable still set does not weaken isolation, it deletes it, for every Project the fixed
principal's memberships happen to reach. It must be off by default, require an explicit, single,
unambiguous variable (never inferred from "no auth header was configured" or any other absence), and
this document deliberately does not add a second gate (an `environment=development` style co-requirement)
beyond what the task asked for — but an operator combining this with deployment-level network controls
is the kind of defense-in-depth this document assumes exists outside its own scope, the same way
`ILAVRITA_EXPOSE_POCKETBASE` assumes an operator will not expose `/api` to the public Internet just
because the flag makes it possible.

**REST-34 — what it must log.** Exactly once, at process startup, if and only if `ILAVRITA_DEV_
PRINCIPAL` is set and valid: a `warn`-level (or louder) log line naming the exact resolved
`(project id, principal kind, principal id)` and stating plainly that FHIR authentication is disabled for
this process. Not once per request — a per-request log line would either be silently rate-limited into
invisibility by the very tooling meant to catch it, or flood production logs with the one signal an
operator most needs to notice standing out, defeating its own purpose either way. The three identifiers
themselves are not secret (they are configuration, not a credential or PHI, and FR-034 governs resource
bodies, not config values) and belong in the log precisely so a "why does every request in this
environment say it's `dev-user`" investigation has an obvious, greppable answer at the source.

**REST-35 — what the resolved principal feeds into `BuildScope`.** For every request on the six routes
in §2 (never `/fhir/R4/metadata`), when a dev principal is configured: `Request.Principal` = the parsed
`PrincipalRef`; `Request.Project` = the parsed project id — always this same fixed Project, regardless of
any `{resourceType}`/`{id}` in the URL, since nothing about this interim mechanism lets a request name a
different home Project than the one baked into the variable; `Request.LinkedProjects` = `nil` (§6.1);
`Request.Now` = `time.Now().UTC()` at request time (not at process start — `Request.Now` is a decision
instant per request, not a resolver-level constant, matching `authz.Request`'s own doc comment in
`packages/authz/scope.go`). `BuildScope` can still return an empty `Scope` for this principal against a
particular request — that is not `401`, it is the ordinary `403`/`404` path §5.1 already specifies; a
configured dev principal answers only "who is asking," never "what may they do."

### 6.1 By-key interactions never populate `LinkedProjects`

**REST-36.** none of the six interactions in §2 carries any mechanism (query parameter, header, or
otherwise) for a caller to name a linked Project the way search's planned `_project` parameter would
(`docs/design/phase2-constraints.md`, LNK-8: "Resolve `_project` once, in one direction: it is an
authorization *input*"). Every `Request` these routes build therefore always has `LinkedProjects = nil`,
which — per `authz.BuildScope`'s own `requestedGrantors`/`linkGrants` — means `linkGrants` contributes
nothing and only the caller's own home-Project membership Grants apply, ever, to any of these six routes.
Concretely: a Project-A principal can never reach a Project-B resource by key, even by id-guessing,
even when an active `Link` from B into A exists granting exactly that resource type — the *only*
currently-specified path to linked data is search's (unimplemented) `_project` parameter, not any
by-key route. This is a direct, testable consequence of REST-35 plus the frozen `authz.BuildScope`
contract, not a new restriction this document adds — it is called out because it is easy to assume a
`Link` alone is sufficient and miss that by-key routes have no way to invoke it.

## 7. What the CapabilityStatement may advertise

Persistence is generic — `ResourceRepository`/`VersionStore` accept any `storage.ResourceType` string
uncritically (`packages/storage/identifier.go`: `type ResourceType string`, no validation) — so nothing
below the HTTP layer can be the source of truth for "this is a real, supported FHIR resource." That
authority has to live at the boundary, in exactly one place, and `api/openapi.yaml`'s existing comment
already states the intended discipline for it: "A resource appears here only once the matching
interaction is implemented and covered by tests." This section makes that rule precise enough to test.

**REST-37.** a `(resourceType, interaction)` pair may appear in
`CapabilityStatement.rest[].resource[].interaction` if and only if all four hold:

1. a route from §2 dispatches that interaction for that `resourceType`, through the mapping this
   document specifies (§2's table) — not a different, ad hoc mapping for one type.
2. an automated conformance test exercises every status-code and header rule §3 assigns to that
   interaction, for that `resourceType` specifically (a generic "the mapping works for `Patient`" test
   does not license listing `Observation` too).
3. `resourceType` names a real FHIR R4 resource type — checked against R4's own closed type list, not
   merely "any string `storage.ResourceType` will accept." REST-2 is the runtime enforcement of this
   same fact (an unrecognised `{resourceType}` is `404` regardless of what `CapabilityStatement` says);
   this bullet is the authoring-time twin — nothing should ever be added to the advertised list that
   REST-2's own check would then immediately reject.
4. the type is `storage.KindFHIR`. A `KindPlatform` resource (a platform/administrative row —
   `packages/storage/scope.go`'s `Kind` enum) is never a FHIR resource under any circumstance and can
   never be listed here no matter how thoroughly it is tested; `CapabilityStatement` describes the FHIR
   surface only, per `packages/fhir/doc.go`'s own charter ("the FHIR R4 vocabulary Ilavrita exposes at
   its public boundary").

**REST-38.** `CapabilityStatement` is computed once, identically, for every caller — `NewCapabilityStatement`
already fixes `Date` for the process lifetime (`apps/ilavrita/main.go`'s `startedAt`), and this document
does not change that. It states what the *server* can do at all, an upper bound; what one particular
principal may actually do within that is `Scope`'s question, decided per-request by `BuildScope`, never
by narrowing the document itself per caller. A caller's Grants are expected to name only resource types
this document's rule already licenses listing — an `AccessPolicy` rule naming an unlisted type would be
an operator authoring error, not a case this document's runtime enforcement (REST-2) needs to special-
case, since REST-2 already turns any such misconfigured request into `404` regardless of what the policy
intended.

## 8. Checklist: rules a conformance test suite must assert

- create: `201`; `Location` versioned; `ETag` weak-quoted; body/`resourceType` match required (REST-3–5).
- read: `200`/`404`/`410` exactly per REST-6–8; `ETag`+`Last-Modified` on `200`.
- vread: `ETag` always echoes the *requested* `{vid}`, never the current version (REST-9); `404` vs
  `410` per REST-10.
- update: `200` vs `201` is which storage method ran, never a separate flag (REST-15); the five-branch
  algorithm of REST-16 reproduced exactly; `412` vs `409` per the single rule in REST-17, both directions
  tested.
- delete: `204`, no body; optional `ETag`; `410` on an already-deleted id needs no special-casing beyond
  what `Read` already reports (REST-12–14).
- history-instance: `200`/`404` per REST-20–21; `entry.request.method` reconstructed per REST-22's three
  cases with no stored field to read it from directly.
- every storage error in §5's table produces its listed status and issue code, with a diagnostics string
  containing none of the wrapped Go error's own text (REST-29 for the `ErrScopeEscape` case especially).
- `403` never appears for a row-specific denial and `404`/`410` never appear for a capability-level one
  (REST-26).
- no principal configured → `401` on all six routes, `200` still on `/fhir/R4/metadata` (REST-30).
- a malformed `ILAVRITA_DEV_PRINCIPAL` refuses to start the process, never silently falls back to
  deny-by-default (REST-32).
- a Project-A dev principal can never reach a Project-B resource by key even across an active `Link`
  (REST-36).
- `api/openapi.yaml`'s `Issue.code` enum carries `deleted`, `duplicate`, `login` and `forbidden` before
  any of this document's handlers ship (REST-27, REST-28) — `scripts/verify-openapi.sh` is the backstop,
  not the first line of defense.
