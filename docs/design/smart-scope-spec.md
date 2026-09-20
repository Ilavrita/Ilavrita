# SMART scopes as Scopes

This document specifies how a SMART App Launch authorization becomes a `storage.Scope`, and
nothing else. It is written before any endpoint because that mapping is the part most likely
to be wrong, and the part a wrong answer is hardest to notice: an OAuth flow that issues a
token is visibly working whether or not the token authorizes what it should.

Everything it reads is committed: `packages/storage/scope.go`, `packages/authz`,
`packages/project/membership.go`, and `docs/design/authz-spec.md`, which this document treats
as binding and extends only where it is silent.

Where a rule here is this document's own decision rather than something SMART or the committed
code already states, it is marked **(decision)**.

## 1. The rule everything else follows from

**A SMART scope narrows. It never widens.** (decision)

An app acting for a user must not reach anything that user cannot reach. The user's standing
already produces a Scope — `authz.BuildScope` compiles their `AccessPolicy` into Grants with
compartments, filters and projections. A SMART scope is an *additional* restriction the app
asked for, on top of that.

So the effective Scope is the **intersection**:

```
effective = narrow(policyScope, smartScopes)
```

never a union, never a replacement. A `user/*.*` scope does not make an app omnipotent; it
makes the app exactly as capable as the person who approved it, which is the most it may ever
be.

This is the whole of the security argument, and it is why the mapping is worth specifying
before the endpoints. An implementation that built Grants *from* the scopes rather than
narrowing the user's own would be one where a scope string is a privilege.

## 2. What a scope says

SMART v1: `{context}/{resource}.{read|write|*}`
SMART v2: `{context}/{resource}.{c|r|u|d|s}+` with an optional `?param=value` restriction.

| Part | Values | What it names |
| --- | --- | --- |
| context | `patient`, `user`, `system` | whose data, and whether a person is involved |
| resource | an R4 type, or `*` | which `storage.ResourceType` |
| access | v1 `read`/`write`/`*`, v2 letters | which `storage.Action` |

This build serves 126 types and five actions (`read`, `write`, `delete`, `history`, `search`),
so every scope resolves to a set of `(Type, Action)` pairs and nothing else. There is no action
SMART names that this build does not have, and none of this build's that SMART cannot name.

### Actions

| SMART | `storage.Action` |
| --- | --- |
| v2 `c` | `ActionWrite` |
| v2 `r` | `ActionRead` |
| v2 `u` | `ActionWrite` |
| v2 `d` | `ActionDelete` |
| v2 `s` | `ActionSearch` |
| v1 `read` | `ActionRead`, `ActionSearch`, `ActionHistory` |
| v1 `write` | `ActionWrite`, `ActionDelete` |
| v1 `*` | all five |

`c` and `u` both map to `ActionWrite` because this build does not separate them: a create and
an update are the same authorization here, and an app granted only `c` would be granted `u` as
well. **That is a widening, and it is refused rather than granted** (decision): a scope naming
`c` without `u`, or `u` without `c`, is not one this server issues. The token response says
which scopes were granted, and SMART requires a client to honour that, so refusing is visible
rather than silent.

`history` has no SMART letter. v1 `read` includes it because v1 `read` is the whole read side;
v2 `r` does not (decision), so a v2 app wanting history asks for v1-style `read` or is refused
the history routes.

## 3. Context

### `patient/`

Narrows to one patient's compartment. The patient comes from the launch context, not from the
scope — the scope says "the patient", the launch says which one.

```go
Grant{ ..., Compartment: &storage.Compartment{Type: "Patient", ID: launchPatient} }
```

Which is exactly the shape a confined `AccessPolicy` already produces, so nothing downstream
changes: the compartment predicate that already bounds a confined grant bounds this one.

**A `patient/` scope with no launch patient authorizes nothing** (decision). Not "everything",
not "the user's own" — nothing. The scope names a patient and there is not one, so there is no
set of resources it describes. This is the single most dangerous place to be permissive.

Where a grant already carries a compartment, the two must agree: a user confined to
`Patient/A` whose app asks for `patient/` with launch patient `B` gets **nothing for that
type**, not `B`, and not `A`. (decision)

### `user/`

No compartment of its own. The user's own policy already says what they may reach — confined
to a compartment, filtered by an element, projected to some fields — and a `user/` scope keeps
all of it and narrows the `(Type, Action)` set.

### `system/`

Backend services, no person. This build has client applications and service credentials
already; a `system/` scope narrows what that client may do, against the client's own standing
rather than a user's. Until client-credentials tokens exist, **`system/` is refused** (decision)
rather than treated as `user/` with nobody.

## 4. `*` and the cost of it

`user/*.read` is not one Grant. It is one Grant per `(type, action)` the user's policy already
authorizes — up to 126 × 3 — and a Scope carrying a thousand Grants compiles into a SQL
statement with a thousand arms.

So `*` is expanded against **what the policy already grants**, never against the served type
list (decision). A user whose policy reaches four types gets twelve Grants from `user/*.read`,
not three hundred and seventy-eight. This falls out of the intersection rule and is worth
stating because an implementation that expanded first and intersected second would build the
large Scope before discarding it.

## 5. v2 search-parameter restrictions

`patient/Observation.rs?category=laboratory` narrows further. `storage.Filter` already exists
and is exactly this: an element, a comparator, values, compiled into a predicate rather than
applied after fetching.

A restriction whose parameter this build does not index is **refused at consent time, not at
request time** (decision). The app is told which scopes it was granted, and a scope silently
dropped is one the app believes it holds.

Where a grant already carries a filter, both apply — `AND`, never replaced.

## 6. Projection

SMART says nothing about which elements come back. A policy's `Projection` therefore survives
intersection untouched: an app cannot ask for more elements than the user's policy returns, and
has no way to ask for fewer.

## 7. What the token carries

A session here is already a `project.Session` with a membership, and every route resolves
standing from it. A SMART access token must resolve to the same thing plus the granted scopes
and the launch context, so the narrowing can be applied per request rather than baked in:
**scopes are stored with the session and intersected at request time** (decision), because a
policy that changes must take effect on the next request rather than at the next token refresh.

## 8. Out of scope for this document

The authorization endpoint, the token endpoint, PKCE, JWKS, refresh, token introspection and
`.well-known/smart-configuration`. All of them are ordinary once this mapping is settled; none
of them is safe before it is.

## 9. What this needs that does not exist

- Client-credentials tokens, for `system/` scopes to mean anything
- ~~A launch-context store: which patient a session was launched for~~ — **built**, as two
  columns on `sessions` rather than a table beside it. See section 10.
- ~~`authz.Narrow(Scope, []SmartScope) Scope`~~ — **built**. `packages/authz/smart.go`, beside
  `BuildScope`, with `ParseScope` for the scope strings themselves. Every rule marked a decision
  above has a test, and each of the five that matter for safety has been confirmed by mutating
  the code until that test fails

## 10. Where the launch context lives

Section 7 says the scopes are stored with the session and intersected at request time. They are
stored **in the session's own row** — `sessions.launch_patient` and `sessions.granted_scopes` —
rather than in a table keyed on the session id (decision).

The reason is the failure mode. A launch context in a second table can be absent, and absent
reads as "no app holds this session", which is the value that skips the narrowing entirely. A
row that failed to write, or a join that was forgotten, would hand an app everything the person
it acts for can reach. In the session's own row there is no such state: the read that proves the
token is the read that returns the grant.

The same reasoning shapes the types.

- `project.LaunchContext` holds the two strings and refuses an empty grant, so "an app's session"
  and "a session granting nothing" cannot be the same value.
- `project.IssueAppSession` takes one as an argument. There is no method that attaches a launch
  context afterwards, so no order of calls mints an app's session that nothing narrows.
- `authz.Launch` has three states, not two: **unstated**, no app, and an app's. `BuildScope`
  refuses an unstated one with `ErrMissingLaunch`, the way it refuses a nil resolver — a caller
  that never considered the question is a wiring mistake, and this mistake widens.
- `BuildScope` applies the narrowing itself rather than returning a Scope for the caller to
  narrow. It is the only function that produces a Scope, so an app's request cannot reach storage
  through a path that forgot.

The scopes are stored as the token response reported them, space-delimited and verbatim, so what
the app was told it holds and what this server narrows by are the same text rather than two
renderings of it. A stored scope this build cannot parse denies the request; it is never read as
a session no app holds.
