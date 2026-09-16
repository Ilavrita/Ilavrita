# Scope builder specification

This document specifies the one piece of the control plane that does not exist yet: the function
that turns an authenticated principal, addressing one Project, into a `storage.Scope`. Everything it
reads is frozen and already committed — `packages/storage/scope.go`, `packages/storage/repository.go`,
`packages/project/{project,membership,link}.go`, `packages/storage/pocketbase/schema.sql`,
`packages/storage/pocketbase/resource.go` — and `docs/design/phase2-constraints.md`, which this document
treats as binding wherever it speaks and extends only where it is silent. Nothing here is implemented;
this is a specification precise enough that `packages/authz`'s tests can be written straight from it.

Where this document's rules go beyond what phase2-constraints.md or the committed code already states
outright, they are marked **(synthesis)** so an implementer can tell settled fact from this document's
own design decision.

## 1. Where it sits

`packages/authz`. The package already exists (`packages/authz/doc.go`, placeholder only) and its own doc
comment — "Its predicates join query planning; results are never filtered after fetching" — describes
exactly the invariant this builder exists to uphold: `storage.Grant` values it produces become bound SQL
predicates inside `packages/storage/pocketbase` (SCH‑2, SCH‑3), never a post-fetch filter. `authz` is
also, per **CP‑7**, the only package (plus the explicit bootstrap path) permitted to call
`storage.NewScope`. This builder is that call site — the only one.

## 2. Exact Go signature

```go
package authz

// Request names one authorization decision: the Scope being built must permit
// exactly this (Kind, Type, Action) for this principal, in this Project, and
// in any grantor Project the caller explicitly opted into.
type Request struct {
	Principal      project.PrincipalRef
	Project        project.ID   // the home Project: where the principal's membership stands
	LinkedProjects []project.ID // grantor Projects opted into via repeated _project (LNK-8); usually empty
	Kind           storage.Kind
	Type           storage.ResourceType
	Action         storage.Action
}

// Builder resolves a Request into a Scope. It is the sole caller of
// storage.NewScope outside the bootstrap path (CP-7).
type Builder struct {
	memberships MembershipResolver
	projects    ProjectResolver
	policies    PolicyResolver
	links       project.LinkResolver
	now         func() time.Time
}

func NewBuilder(
	memberships MembershipResolver, projects ProjectResolver, policies PolicyResolver,
	links project.LinkResolver, now func() time.Time,
) *Builder

// Authorize is the ordinary per-request path. Every FHIR, platform-resource,
// Bulk Data, GraphQL, subscription and Bot/job call site mints its Scope here
// (AUTH-4) — never by constructing Grants itself.
func (b *Builder) Authorize(ctx context.Context, req Request) (storage.Scope, error)
```

`Request.Kind`/`Type`/`Action` are required, not optional. `Authorize` resolves exactly the triple asked
for, never "everything this principal can do." This mirrors the only place phase2-constraints.md shows a
call shape: LNK‑4's `authz.Authorize(ctx, Request{Kind: KindFHIR, Type: includeType, Action:
ActionSearch})` for an `_include` target the primary query didn't already cover. A caller that needs a
second (type, action) — an `_include`, a `vread` after a `read`, a second resource family in one
handler — calls `Authorize` again with a fresh `Request`. This keeps every call's resolver work bounded
and keeps `Scope.Narrow` a tool `packages/storage` and calling services use on an already-built Scope,
not a step this builder performs on itself.

`LinkedProjects` is plural where LNK‑8's own illustrative snippet writes `LinkedProject: p` (singular)
**(synthesis)** — because LNK‑8 itself specifies `_project` as "the repeatable search parameter," so a
single request can legitimately name more than one grantor. Absent any entry, `Authorize` mints only
home-Project Grants — the mere existence of an effective link never contributes anything unless the
caller names that grantor explicitly.

## 3. Dependencies this builder needs (not yet committed)

None of the following exist in committed code. They are proposed here as the ports `Builder` depends
on; a concrete implementation backed by `packages/storage/pocketbase` is out of scope for this document.

```go
// MembershipResolver finds one principal's standing in one Project, in
// whatever lifecycle state it is currently in. The builder never asks this
// resolver to pre-filter by state — it applies the standing gate itself, once,
// at admission (§4.2), so a resolver that filtered would only hide which
// membership a denial rested on.
type MembershipResolver interface {
	Membership(ctx context.Context, proj project.ID, principal project.PrincipalRef) (project.Membership, bool, error)
}

// ProjectResolver reads a Project's current lifecycle State.
type ProjectResolver interface {
	State(ctx context.Context, id project.ID) (project.State, error)
}

// PolicyResolver resolves one bound policy against one exact (Kind, Type,
// Action) triple. Each returned PolicyRule is one Grant's worth of
// restriction. Zero entries means the policy grants nothing for this triple —
// not a partial match promoted to a wildcard.
type PolicyResolver interface {
	Rules(ctx context.Context, ref project.PolicyRef, params json.RawMessage,
		kind storage.Kind, resourceType storage.ResourceType, action storage.Action,
	) ([]PolicyRule, error)
}

// PolicyRule carries no more than storage.Grant itself can: a nil Compartment
// means an explicitly-unrestricted rule (LNK-5), never an absent one — policy
// validation, not this builder, is what must have already rejected the
// unmarked absent case.
type PolicyRule struct {
	Compartment *storage.Compartment
}
```

`project.LinkResolver` (`Inbound(ctx, grantee) ([]Link, error)`) is already committed in `link.go` and
is reused as-is — its doc comment ("Resolution is one hop: an implementation must not walk a chain") is
exactly the bound §5 and §6 depend on.

## 4. The ordered resolution pipeline

### 4.1 Principal

`req.Principal.Valid()` (`Kind.Valid() && ID != ""`). Invalid → deny, with an error: this is a malformed
call, not a legitimate "no access" answer, and the caller built the `Request` wrong.

### 4.2 Membership

`memberships.Membership(ctx, req.Project, req.Principal)`. Not found → deny, **no error** — this is a
routine, expected outcome (the principal simply has no standing here), not a resolver failure. A
resolver error (timeout, corrupt row) → deny, **with** an error.

Found is not standing. Before any grant step runs, the builder admits the membership or denies the whole
request:

```go
// stands reports whether a membership is standing a decision may rest on: the
// one the request named, and live. Both grant steps sit behind it, because the
// link step reads no accessor that gates on standing itself.
func stands(membership project.Membership, req Request) bool {
	return membership.Project() == req.Project &&
		membership.Principal() == req.Principal &&
		membership.HoldsStanding()
}
```

`Membership.HoldsStanding()` (`state == MembershipActive && viaLink == ""`) is the domain's single
definition of live, directly-held standing; `Policies()`, `IsAdmin()` and `IsSuperAdmin()` are each
expressed in terms of it, so authz re-states no part of the state machine and the two cannot drift.

**This admission gate is required, not redundant with `Policies()`.** `Policies()` gates only §4.5. §4.6
reads *no* membership accessor at all — its Grants come from the grantor's policy, not the principal's
bindings — so without this gate any membership row that merely exists unlocks every inbound data link:
a revoked clinician, or a principal whose only standing anywhere was minted by an administrative link,
reads a third Project's clinical data (FR‑052; §6 item 11). The Project and Principal re-checks are the
same defense `reaches()` applies to both link ends and `Matches()` applies to a policy reference — a
resolver answering with a membership nobody asked for contributes nothing.

The gate is deliberately **not** `len(Policies()) > 0`: an active member holding no binding of her own
must still reach a grantor (§4.6), so standing and capability are separate questions.

Beyond that gate, the returned `Membership` is used exactly as committed, with no re-implementation of
its gates: `IsAdmin()`, `IsSuperAdmin()` and `Policies()` already return the empty/false answer for a
membership holding no standing, and the builder must not re-derive those answers from `State()` or
`ViaLink()` at their call sites.

### 4.3 Project state

`projects.State(ctx, req.Project)`, then `state.Admits(req.Action)`. This gates the **data plane only**
— `storage.Action` has no admin-capability member, so this check has nothing to say about an
admin-capability request, which is a separate authorization surface this builder does not serve (§4.4).
`StateSuspended` and `StateDeleting` admit nothing; `StateArchived` admits only
`ActionRead`/`ActionSearch`/`ActionHistory`; anything else (an unrecognised `State`, which `Valid()`
would reject) must deny, never fall through to `StateActive`'s permissive branch.

### 4.4 Admin vs. data capability

`Authorize` only ever answers a data-plane question — one `(Kind, Type, Action)` triple reachable
through `ResourceRepository`/`VersionStore`. Nothing about `AdminCapability`
(`project.quota.write`, `project.lifecycle.write`, `project.settings.write`, `project.membership.read`,
and the never-link-conferrable set) is expressible as a `storage.Grant`, because `Grant`'s only axes are
`(Project, Kind, Type, Action)` plus one `Compartment` — there is no `ResourceType` these capabilities
name and no `Action` that distinguishes "write the quota" from "write the settings" on the same
resource. Minting a `Grant{Kind: KindPlatform, Type: "Project", Action: ActionWrite}` for either would
make the two indistinguishable to `Scope.Allows`, which is exactly the collapse **CP‑8** exists to
prevent generalised into a second axis. See §10 for the consequence stated plainly.

So the fork this step performs is: **admin-capability requests are not this builder's job at all.**
Concretely, `IsAdmin()` and `IsSuperAdmin()` are read only by a separate, not-yet-specified
authorization surface that checks them directly (and, for a link-derived capability, checks
`AdminDelegation.Allows(capability, principal)` against the grantee project's inbound administrative
links) — never by `Authorize`. `Authorize` reads neither `IsAdmin()` nor `IsSuperAdmin()` anywhere in its
own pipeline; §7 states why that is the enforcement mechanism for FR‑052, not merely a convention.

The one place "admin" legitimately mints a `Grant` is Super Admin's own deliberately separate,
audited, cross-Project sweep — **CP‑6**'s mechanism, not this builder's ordinary path:

```go
// AuthorizeSuperAdmin mints one Grant per concrete Project for a verified
// Super Admin performing a deliberate cross-Project operation. It is the only
// place in packages/authz allowed to set GrantSource = SourceSuperAdmin.
func (b *Builder) AuthorizeSuperAdmin(
	ctx context.Context, principal project.PrincipalRef,
	kind storage.Kind, resourceType storage.ResourceType, action storage.Action,
) (storage.Scope, error)
```

**(synthesis, following CP‑6's mechanism text directly.)** It (a) resolves the principal's membership in
the Super Project and denies unless `IsSuperAdmin()` is true, (b) enumerates concrete Project ids — never
a wildcard, never an unpinned predicate — (c) mints one `Grant{Project: p, Source: SourceSuperAdmin,
...}` per Project, each independently, so each carries its own audit record. `Authorize`'s ordinary path
must never produce a `Grant` whose `Source` is `SourceSuperAdmin`; that is a testable partition between
the two entry points (§11).

### 4.5 AccessPolicy (home Project)

For every `binding := range membership.Policies()`:

```go
rules, err := policies.Rules(ctx, binding.Policy(), binding.Parameters(), req.Kind, req.Type, req.Action)
if err != nil {
    return storage.Scope{}, err // fail closed — never skip this binding and continue
}
for _, rule := range rules {
    grants = append(grants, storage.Grant{
        Project: req.Project, Kind: req.Kind, Type: req.Type, Action: req.Action,
        Source: storage.SourceMembership, Compartment: rule.Compartment,
    })
}
```

`binding.Policy().Project()` is structurally always `req.Project` here — `bindPolicies` in
`membership.go` sets `PolicyRef{project: owner, ...}` from the membership's own `cfg.Project`, and
`PolicyRef`'s fields are unexported with no setter, so no binding on this membership can ever name a
foreign Project's policy. This is not a runtime check the builder performs; it is a fact the type system
already guarantees, which is exactly why §5 can state AccessPolicy resolution never widens.

### 4.6 Link grants (grantor Project)

Only runs when `req.LinkedProjects` is non-empty **and the principal cleared §4.2's standing gate** —
a link's reach travels with the grantee Project, but only to a principal who holds live, directly-held
standing there. Nothing later in this step re-checks that, so §4.2 is the only place it is enforced.
`now := b.now()`, then `links, err := b.links.Inbound(ctx, req.Project)` — one call, one hop,
`req.Project` is the grantee.

For each `grantor := range req.LinkedProjects`:

1. Find the entry in `links` with `link.Grantor() == grantor`. Absent → contributes nothing for this
   grantor (the caller asked for a link that does not exist, is not effective, or does not reach into
   this Project — `Inbound` already filtered to `Effective(now)`).
2. `share, ok := link.Share(now)` — `ok` is false for an administrative link or a link that is not
   effective; both cases contribute nothing (FR‑052; §6).
3. `share.Covers(req.Type)` — false unless `req.Type` is in the link's closed type list; contributes
   nothing otherwise (LNK‑3).
4. `req.Action` must be `ActionRead`, `ActionSearch`, or `ActionHistory` — a link never confers write or
   delete (schema `project_link_types.action` CHECK; no domain constructor can produce one either).
5. `grantorState, err := b.projects.State(ctx, grantor)`; `grantorState.Admits(req.Action)` must hold.
   **(synthesis)** — phase2-constraints.md states this rule for the home Project (§4.3) but not
   explicitly for the grantor; it follows directly from `State.Admits`'s own doc comment ("a
   precondition checked at request admission"), applied symmetrically: a suspended or deleting grantor
   authorizes no cross-Project reach even while its `project_links` row still says `active`.
6. `rules, err := policies.Rules(ctx, share.Policy(), nil, req.Kind, req.Type, req.Action)` —
   `share.Policy()` is always `PolicyRef{project: grantor, ...}` by construction (`NewDataLink` binds it
   that way), which is LNK‑6 enforced at the type level, not by this lookup. `nil` params: `DataShare`
   carries no `Parameters` field (§9) — a link's restriction cannot be templated per grantee-side
   principal the way an in-Project `PolicyBinding` can.
7. For each returned rule: `grants = append(grants, storage.Grant{Project: grantor, Kind: req.Kind, Type:
   req.Type, Action: req.Action, Source: storage.SourceLink, Compartment: rule.Compartment})`.

A grantor's policy yielding zero rules for this triple means the link contributes nothing for it, even
though the type is in `share.Covers`'s list — the type list is a ceiling the grantor's own policy still
has to fill in **(synthesis, following LNK‑6's "always loaded... by construction" plus LNK‑5's rejection
of an unmarked absent restriction — an absent rule is not a rule, and nothing here may treat it as an
implicit wide-open one).**

`DataShare` carries no principal list (unlike `AdminDelegation.Principals()`), so this step never filters
by which specific member is asking — a data link's reach is scoped to "any principal with active
standing in the grantee Project," not to named individuals. This is a property of the committed type, not
a decision this document makes; see §9 if narrower targeting is ever wanted.

### 4.7 Scope assembly

`return storage.NewScope(grants...), nil` where `grants` is whatever §4.5 and §4.6 accumulated. An empty
slice is a valid, ordinary result: `NewScope()` denies, which is the correct answer for "no binding, no
link, or nothing matched."

## 5. Narrow-only steps vs. the one widening step

Steps 1–4 (§4.1–§4.4) are pure gates: each can turn a potential Grant into no Grant, and none of them
ever adds one. §4.5 (AccessPolicy, home Project) mints Grants, but every Grant it can produce is pinned
to `req.Project` by the type system, not by a check this builder performs — `PolicyRef.project` is
unexported and is set to the owning Project only by the two places that construct one
(`bindPolicies` for a membership, `NewDataLink` for a link, each binding its own owner). No code path,
however written, can make a home-Project `PolicyBinding` resolve against a foreign Project.

§4.6 (link grants) is the **only** step whose Grants can carry a `Project` other than `req.Project` — the
only place a Scope reaches across the isolation boundary the `project` package's own doc comment names
as absolute ("Reach between Projects is an explicit directed link, never inheritance"). That is the
precise sense in which link grants are the sole widening step: every other Grant-producing path is
structurally incapable of naming a different Project; this one is definitionally about a different
Project (`validateEnds` in `link.go` rejects `grantor == grantee`).

What bounds it, all independently necessary (§4.6 enumerates the mechanism; this is the closed list of
why none of them can be relaxed):

- **One hop only** — `project.LinkResolver.Inbound`'s own contract; no chain-following, ever (LNK‑9).
- **Lifecycle- and approval-gated** — `Effective(now)` requires `LinkActive`, both approvals recorded,
  an activation instant reached, and no expiry passed.
- **Kind-gated** — `Share(now)` returns `ok=false` for an `AdminDelegation`; an administrative link can
  never produce a data Grant (FR‑052, §7).
- **Type-closed** — only resource types the link explicitly lists (LNK‑3); no wildcard.
- **Action-closed** — read, search, history only; never write or delete.
- **Restriction owned by the grantor, never the consumer** — LNK‑6, structurally enforced by
  `PolicyRef.project` at construction, not merely by convention.
- **Capped and non-chaining at the product layer** — at most 16 active `kind='data'` links per grantee,
  chained search across a link rejected by the search parser (LNK‑7); this builder does not enforce the
  cap itself, but relies on it to bound how many entries `Inbound` can ever return.
- **Opt-in only** — a caller must name the grantor explicitly via `req.LinkedProjects`; an effective link
  that nobody asked for contributes nothing (LNK‑8).

## 6. Every condition under which a link contributes nothing

A link contributes zero Grants to a Scope when any of the following holds. Each is independently
sufficient; none require the others:

1. `link.Status() != LinkActive` (proposed, suspended, or revoked).
2. `!link.Approvals().Complete()` — either side's approval is missing, even if `Status()` somehow reads
   `LinkActive` (the schema's own CHECK makes this combination impossible to persist, but the domain
   `Effective` still re-checks it, and this builder relies on `Effective`/`Share`, never on `Status()`
   alone).
3. `link.ActivatedAt()` is zero, or `now` is before it.
4. `link.ExpiresAt()` is set and `now` is not before it.
5. The link is administrative (`Delegation(now)` would succeed, `Share(now)` returns `ok=false`) — an
   administrative link contributes nothing to a data Scope, structurally, regardless of what capabilities
   it delegates (FR‑052).
6. `req.Type` is not in `share.ResourceTypes()` (`Covers` is false).
7. `req.Action` is `ActionWrite` or `ActionDelete` — no link ever confers either.
8. The grantor Project's own `State` does not admit `req.Action` (§4.6 step 5, synthesis).
9. The grantor's bound policy resolves to zero rules for `(req.Kind, req.Type, req.Action)` — the type
   being listed is necessary, not sufficient; the policy still has to say something for this exact
   triple (§4.6 step 6, synthesis).
10. The caller never named this grantor in `req.LinkedProjects` — an effective, fully-approved,
    correctly-typed link contributes nothing to a Scope nobody asked to reach through it (LNK‑8).
11. The requesting principal holds no live standing in the grantee (home) Project — no membership at
    all, one that is invited, suspended or revoked, or one an administrative link minted (`viaLink !=
    ""`). Link reach travels with the Project, but only to principals who have standing there in the
    first place. This is §4.2's `stands` admission gate, and it is the *only* thing that enforces it:
    §4.6 reads no membership accessor, so `Policies()`'s own gate never runs on this path.

## 7. FR‑052: what Project Admin grants, and what it categorically does not

`docs/security.md`'s own stated design intent is the plainest version of this: "Project Admin is
administrative authority over a Project's configuration and membership; it does not by itself grant
clinical data access beyond the member's AccessPolicy." This builder enforces that as a structural fact,
not a runtime check someone could forget:

**What Project Admin grants (outside this builder's own surface):** `IsAdmin()==true`, read directly by
the separate admin-capability authorizer this document does not specify (§4.4), unlocks control-plane
writes over the admin's own Project — creating and revoking memberships, authoring and binding
AccessPolicy, proposing and approving links, editing quota, lifecycle state, and settings. None of this
is a `storage.Grant`.

**What Project Admin does not grant, ever, through this builder:** `Authorize`'s own pipeline (§4.1–§4.7)
never reads `membership.IsAdmin()` or `membership.IsSuperAdmin()` at all — not to add a Grant, not to
skip a check, not to widen a Compartment. An admin who wants FHIR data access needs the exact same
`PolicyBinding` an ordinary member would need; `Membership.Policies()` gates purely on `state ==
MembershipActive && viaLink == ""`, with no reference to the `admin` field anywhere in its
implementation. This is why CP‑8's rule ("no boolean OR between an administrative check and a data-policy
result, anywhere") is trivially true of this builder: there is no admin check in it for a data-policy
result to be OR'd with.

The link-specific half of FR‑052 — the attack that actually broke the earlier design (an administrative
link's `project.membership.admin` capability minting a full membership, including a policy binding, in
the grantor Project) — is closed one level upstream of this builder, in `packages/project` itself:
`NewLinkedMembership`'s signature takes no admin flag and no policy argument at all
("its signature cannot express a privilege," per its own doc comment), so a link-minted `Membership`
cannot hold either no matter what this builder or any caller does. `Membership.Policies()` additionally
returns `nil` whenever `viaLink != ""`, as a second, independent gate. Between the two, even a
hypothetical bug in this builder that read `IsAdmin()` could not manufacture a link-sourced Grant,
because the underlying `Membership` value structurally cannot carry the privilege being asked for.

## 8. What AccessPolicy must express, and what is not expressible

Given `PolicyRule` (§3) can carry no more than `storage.Grant` itself does, an `AccessPolicy` — not yet a
defined Go type anywhere in the repository — must be able to resolve, for a bound `(policy, params)` pair
queried at one exact `(Kind, Type, Action)`, to a set of zero or more entries each naming **at most one**
`Compartment{Type, ID}`. That is the entire expressive surface.

**Expressible today, by minting multiple Grants or multiple rules:**

- Access to more than one resource type: one rule (and one `Grant`) per type — never an implicit list on
  a single `Grant` (LNK‑3's logic for links generalises to policies too, since `Grant.Type` is singular
  everywhere).
- Access to more than one action on the same type: one rule per action.
- Restriction to more than one named subject: one rule per subject, each with its own `Compartment`.
- An explicitly-declared, unrestricted rule (`Compartment: nil`) — but only once policy validation has
  rejected that declaration for any clinical resource type per LNK‑5. `NewUnrestrictedRule` classifies by
  **allowlist**: a FHIR type is unrestrictable only if it is named in the closed directory/terminology/
  conformance/definitional list, so an unclassified type — `Binary`, `MolecularSequence`, or anything a
  later FHIR release adds — is refused rather than assumed safe. A denylist here fails open, and a
  grantor's policy is reachable across a data link (§4.6), so the failure would cross a Project boundary.

**Not expressible, full stop, by `Grant`'s current shape — reject at policy-validation time, never widen
silently to satisfy the request:**

- **Any multi-term or boolean criteria** — "`status = final AND category = vitals`," "belongs to a care
  team," anything beyond one literal `(subject type, subject id)` equality. This is the "Open gap this
  leaves" already named at the top of phase2-constraints.md; this document does not propose closing it.
- **Field-level redaction.** Redacting one field of a FHIR resource's JSON body is a post-fetch content
  transform, not a row predicate — structurally outside what `Grant`/`Scope` gate at all (they authorize
  rows, not fields within a row). AUTH‑5's coupling requirement (redacting a field must also remove its
  search parameter) cannot be enforced by this builder for the same reason: `Grant.Action == ActionSearch`
  is a blanket permit over the whole resource type, with no per-parameter axis to restrict.
- **A dynamic or computed Compartment.** `Compartment.ID` is a `storage.LogicalID` — a literal FHIR id
  fixed at rule-authoring or link-authoring time. "Whichever patient this clinician currently treats" is
  not expressible; only "exactly this one patient" is.
- **A time-bounded policy rule.** Only `Link` carries `ExpiresAt`/`HistoryFrom`; `PolicyBinding` has
  no equivalent. A same-Project AccessPolicy rule with a validity window is not expressible today.
- **A wildcard "every resource type."** `Grant.Type` is one `ResourceType`; there is no sentinel meaning
  "all," consistent with `project.ValidateID` rejecting `"*"` for the same reason elsewhere in this
  codebase — a rule wanting broad coverage must enumerate every type it means, explicitly.
- **Per-grantee-principal parameterization of a link's restriction.** `DataShare` carries no
  `Parameters` field the way `PolicyBinding` does (§9) — a link's rule is the same for every principal
  in the grantee Project; it cannot be templated per member.

## 9. Gaps in the committed contract this document will not paper over

These are properties of the frozen code, discovered while writing this pipeline, that this builder
cannot resolve on its own and that this document deliberately does not invent a workaround for:

1. **`DataShare` names no per-type action.** `project_link_types` (schema.sql) keys on
   `(grantee_project, grantor_project, kind, res_type, action)` — one row per type-and-action pair,
   `action` constrained to `read`/`search`/`history`. `DataShare` (`link.go`) exposes only
   `ResourceTypes() []storage.ResourceType`, with no accessor pairing a type to a specific action; the
   comment "types the link covers... resolver mints one Grant per type" says nothing about action. This
   document's §4.6 resolves the ambiguity by treating every listed type as available for all three
   read-family actions equally (**synthesis** — the honest alternative reading, "not yet safe to build,"
   is also defensible; this is a real open decision, not a detail this document is entitled to settle
   unilaterally). Whichever choice ships, it needs to be made explicitly and tested, not left implicit.
2. **`DataShare` carries no `policy_params`.** The `project_links` table has a `policy_params TEXT NOT
   NULL DEFAULT '{}'` column with no counterpart field on the domain `DataShare` struct or `NewDataLink`
   signature. Combined with (1), a link's persisted row can express more than the domain object that is
   supposed to represent it — either the domain type needs to grow these fields before the builder can be
   implemented against real data, or the schema columns are provisioned for a feature not yet designed.
3. **`AdminCapability` has no representation in `Grant` at all** (§4.4) — this is not a missing accessor
   like (1) and (2), it is a missing dimension: `Grant` has nowhere to put "which capability." Any future
   attempt to route admin-capability checks through `storage.Scope` needs a new mechanism (a
   `Kind`/`Type` invented for it, or an entirely separate authorization type), not a same-shaped `Grant`.
4. **CP‑7's own mechanism text names a `storage.NewGrant` that does not exist.** `scope.go` has no
   `NewGrant` constructor — `Grant`'s fields are exported and it is directly literal-constructible
   (`storage.Grant{Project: "x", ...}`) from any package today. The architecture test CP‑7 calls for must
   therefore grep/analyze for `storage\.NewScope\(` calls (the actual privilege-conferring operation,
   since an inert `[]Grant` cannot become a functioning `Scope` any other way — `Scope`'s own `grants`
   field is unexported), not for a `NewGrant(` call pattern that will never match anything and would give
   the test a false sense of coverage.

## 10. Failure mode: every unknown denies

Stated once, generally, because it recurs at every step above: any value this pipeline cannot positively
recognise is treated as absent, never as the nearest known thing.

- An unrecognised `project.State`, `project.MembershipState`, or `project.LinkStatus` (i.e. `Valid() ==
  false`) must deny whatever step consults it — never fall through to that enum's most-permissive case.
  This mirrors the domain types' own stated philosophy (`State.TransitionTo`'s doc comment: "An
  unrecognised current state fails closed instead of being read as the nearest known one").
- An unrecognised `storage.Kind` or `storage.Action` in a `Request` is a malformed call (§4.1's class of
  error), not a deny-with-no-error case — `Authorize` should refuse to even attempt resolution.
- A resolver error at any step (`MembershipResolver`, `ProjectResolver`, `PolicyResolver`,
  `project.LinkResolver`) must abort the whole `Authorize` call with a non-nil error, never silently skip
  the failing lookup and proceed with a partial Grant set. **Testable invariant:** `err != nil` implies
  `scope.IsEmpty()` on every return from `Authorize` — this builder never returns a non-empty Scope
  alongside an error, so a caller who forgets to check `err` and only checks `scope.IsEmpty()` (already
  the safe default per `Scope`'s own zero-value semantics) is never wrong to do so.
- A `PolicyResolver` returning a rule with an unrecognised field, or any resolver returning a `Grant`
  whose `Kind`/`Type`/`Action` does not exactly equal what was asked for, must be treated as a resolver
  bug and denied, not coerced to fit — this builder never constructs a `Grant` from anything but the
  literal `req.Kind`/`req.Type`/`req.Action` it was called with (§4.5, §4.6), precisely so a resolver
  cannot smuggle in a mismatched Grant through its return value.
- `AUTH‑1`'s mapping is absolute and this builder enforces none of it itself: `Request.Action` must be
  `ActionHistory` for a `vread` or `ListVersions` call, never `ActionRead` — `Authorize` has no way to
  know which storage method a caller intends to invoke next, so an incorrect `Action` in the `Request` is
  entirely the caller's error to make. This needs to be documented next to whichever FHIR-service code
  constructs `Request` values, since `scope.go` is frozen and cannot carry a comment this specific.

## 11. Rules a test can be written against

1. `Authorize` with an invalid `Principal` returns an error and `scope.IsEmpty()`.
2. `Authorize` with no membership in `req.Project` returns `scope.IsEmpty()` and a **nil** error —
   including when `req.LinkedProjects` names an effective link, which is the case a test must cross
   explicitly or the membership gate goes unpinned.
3. `Authorize` against a `StateSuspended` or `StateDeleting` home Project returns `scope.IsEmpty()`
   regardless of any policy binding the membership holds.
4. `Authorize` against a `StateArchived` home Project with `Action: ActionWrite` returns
   `scope.IsEmpty()`; the same call with `Action: ActionRead` does not, given a matching policy binding.
5. `Authorize`'s returned `Scope` never contains a `Grant` with `Source: SourceSuperAdmin` — grep the
   implementation, and property-test that only `AuthorizeSuperAdmin` ever constructs one.
6. For a membership with `IsAdmin() == true` and no policy binding, `Authorize` for any `(Kind, Type,
   Action)` returns `scope.IsEmpty()` — admin standing alone grants nothing on the data plane.
7. For a membership with `viaLink != ""` (link-minted), `Authorize` returns `scope.IsEmpty()` for every
   `(Kind, Type, Action)`, even if the caller manually constructs a `Membership` value with `admin: true`
   or a non-nil `policies` field — `Membership`'s own accessors refuse to surface it either way.
7a. Every membership case must be crossed with a **non-empty** `req.LinkedProjects` and an effective
    inbound link, not only with the link path idle: invited, suspended, revoked, link-minted, and absent
    each return `scope.IsEmpty()`. These are the FR‑052 and dead-membership cases; a suite that exercises
    the two axes only separately proves nothing about either, because §4.5 and §4.6 fail for different
    reasons.
7b. The converse, so the gate cannot be tightened into an over-deny: an **active, directly-held**
    membership carrying **zero** policy bindings still receives the grantor's link Grants. Standing is
    not capability, and `len(Policies()) > 0` is not a legal proxy for it.
7c. A `MembershipResolver` answering with a membership for another principal or another Project — or one
    it simultaneously reports as not found — contributes nothing, mirroring the both-ends re-check
    `reaches()` applies to a link and `Matches()` applies to a policy reference.
8. Every `Grant` `Authorize` mints from §4.5 (AccessPolicy) carries `Project == req.Project`; property
   test: no sequence of `PolicyResolver` responses can make this false.
9. `Authorize` with `req.LinkedProjects` naming a grantor whose inbound link is `LinkProposed`,
   `LinkSuspended`, or `LinkRevoked` contributes zero Grants for that grantor.
10. Same, for a link missing either approval, for one not yet at its activation instant, and for one past
    its `ExpiresAt` — four independent test cases, not one collapsed "inactive" case.
11. `Authorize` with `req.LinkedProjects` naming the grantor of an *administrative* link (no matching
    `DataShare`) contributes zero Grants for that grantor, for every `(Kind, Type, Action)`.
12. `Authorize` with `req.Type` outside `share.ResourceTypes()` contributes zero Grants for that grantor,
    even when the link is fully effective and the grantor's policy would otherwise allow it.
13. `Authorize` with `Action: ActionWrite` or `ActionDelete` and a non-empty `req.LinkedProjects`
    contributes zero link Grants, regardless of what the grantor's policy would say for read actions.
14. An effective, correctly-typed, correctly-actioned link contributes zero Grants when
    `req.LinkedProjects` does not name its grantor — existence of the link is not sufficient.
15. Every `Grant` `Authorize` mints from §4.6 (link grants) carries `Source: SourceLink` and `Project ==`
    the grantor, never `req.Project`.
16. For every resolver returning an error, `Authorize` returns a non-nil error and `scope.IsEmpty()` —
    run once per resolver (`MembershipResolver`, `ProjectResolver`, `PolicyResolver`,
    `project.LinkResolver`), each independently.
17. `Authorize` called twice with different `req.Type` values, against a membership whose policy binds
    only one of them, returns a non-empty Scope for that one and `scope.IsEmpty()` for the other — proof
    that resolution is exact-match, not "the membership has some policy, so allow this too."
