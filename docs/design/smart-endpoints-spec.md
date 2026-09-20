# The SMART endpoints

`docs/design/smart-scope-spec.md` settles what a scope means and how it narrows, and defers the
endpoints as "ordinary once this mapping is settled". They are — but ordinary is not the same as
obvious, and this document records the handful of decisions that could be wrong, so the rest can
be plumbing.

Where a rule here is this document's own decision rather than something SMART, OAuth 2.0 or the
committed code already states, it is marked **(decision)**.

## 1. An access token is a session

There is no second kind of bearer credential. `project.IssueAppSession` mints a `Session` carrying
the launch context, `SessionStore.Resolve` is what every request already goes through, and
`BuildScope` already narrows by what that session records. So the token endpoint's whole job is to
decide *what* to issue, never to invent a new thing to check.

This falls out of the mapping spec's section 7 — scopes intersected at request time, not baked into
a token — and it is why there is no JWT access token here (decision). A signed token would carry a
copy of the standing, and a copy is what goes stale when a policy changes.

## 2. Consent, on a server with no frontend

This server renders no HTML. So the authorization endpoint is two calls, not a page (decision):

- `GET /oauth2/authorize` — with an authenticated session, validates the request and answers with
  the pending authorization: the client, the scopes it asked for, which of them this server would
  grant, and which it refuses and why. It writes nothing.
- `POST /oauth2/authorize` — records the person's approval and answers with the redirect target
  carrying `code` and `state`.

A UI renders the first and submits the second. Nothing about the flow requires that UI to be ours,
and a server that shipped a consent page would be a server whose consent page had to be styled,
translated and kept accessible by whoever deployed it.

**The approval names the scopes.** `POST` carries the exact scopes the person approved, and they
must be a subset of what `GET` reported as grantable. A client cannot widen between the two calls,
and a UI that lets somebody deselect a scope works without this server knowing it happened.

## 3. The authorization code

Single use, 60 seconds, and **stored as a SHA-256 the way a session token is** (decision) — it is a
bearer credential between two requests, and a stolen database should not yield one.

It binds, and the token endpoint re-checks, all of:

| Bound | Why |
| --- | --- |
| `client_id` | a code redeemed by another client is a stolen code |
| `redirect_uri` | the exact string, re-presented at the token endpoint (RFC 6749 §4.1.3) |
| `code_challenge` | PKCE — see below |
| the approved scopes | what the person agreed to, not what the client asked for |
| the launch patient | decided at authorization, not at redemption |
| the membership | pinned here, for the same reason `IssueSession` pins it: the instant a credential was presented is the only honest moment to choose |

Redemption destroys the row rather than marking it used (decision), mirroring how revocation
destroys session material: a code that cannot be replayed because it no longer exists needs no
state anything must remember to read.

## 4. PKCE

**S256 only, required of every client, public and confidential alike** (decision).

`plain` is refused. It is in RFC 7636 for devices that cannot SHA-256, and this is a FHIR server —
a client that cannot hash cannot do TLS either. Requiring it of confidential clients too is
stricter than SMART asks; it costs a client nothing and removes the code-interception class
outright.

## 5. `aud`

The authorization request must carry `aud`, and it must equal this server's FHIR base URL. SMART
requires this and the reason is worth stating: without it, an app tricked into pointing at a
hostile authorization server will happily send that server's token to the real one, or the
reverse. The base URL is `ILAVRITA_BASE_URL` plus the FHIR path, which the server already computes.

## 6. Refresh

A refresh token is its own credential, hashed at rest, and **rotated on every use** (decision):
redeeming one destroys it and mints another. If a destroyed refresh token is presented again, that
is either a replay or a race, and both mean the token leaked — so the whole chain is revoked, along
with the sessions it issued.

Refresh is offered only when `offline_access` or `online_access` was granted, per SMART.

A refresh does **not** re-run consent, and it does not re-read the scopes from the client. It mints
a new session carrying the same launch context, and the narrowing happens at request time against
whatever the policy says *then* — which is the whole reason the mapping spec chose to store scopes
with the session.

## 7. Client authentication

| Client | How it proves itself |
| --- | --- |
| public | PKCE alone; holds no secret |
| confidential | `client_secret_basic`, against the `Credential` this build already mints |
| backend service | `private_key_jwt` — a JWT signed by the client, verified against its registered JWKS |

The first two are built on what exists: `ClientApplication.IssueCredential` already produces a
hashed, expiring, revocable secret, and `Credential.Matches` already compares in constant time.

`private_key_jwt` is what SMART Backend Services requires, and it is what makes a `system/` scope
mean anything. Until it exists, `ParseScope` refuses `system/`, and that refusal stays rather than
being softened to a client secret (decision) — a backend service authenticated by a shared secret
is not the thing SMART named, and calling it that would make the CapabilityStatement lie.

## 8. What the token response says

`scope` reports what was **granted**, which may be less than what was asked for. SMART requires a
client to read it. Every refusal this build makes — `system/`, a search-restricted scope, a type it
does not serve, `c` without `u` — is therefore visible to the app rather than silent.

## 9. Order of work

1. ~~Redirect URIs and public/confidential on `ClientApplication`~~ — **built**
2. ~~The authorization code: domain type, table, store~~ — **built**
3. ~~`GET`/`POST /oauth2/authorize`~~ — **built**
4. ~~`POST /oauth2/token`, `authorization_code` grant~~ — **built**
5. ~~Refresh tokens and the `refresh_token` grant~~ — **built**
6. `private_key_jwt` and the `client_credentials` grant, which is what unblocks `system/` —
   **partly built**: `project.JWKS` parses and holds a registration's public keys, and nothing
   uses it yet. What remains is in section 12.
7. ~~`.well-known/smart-configuration`~~ — **built**

## 10. What the standalone launch does today

A public or confidential client registers its redirect addresses, sends a person to
`GET /oauth2/authorize`, which answers with what it would grant and what it refuses and why; the
person's UI posts the approval; the code comes back on the registered address; the client redeems
it at `POST /oauth2/token` with its verifier, and receives a session narrowed to what was
approved. `authz.Narrow` applies at every request thereafter, against whatever the policy says
then.

An approval naming `offline_access` or `online_access` also receives a refresh token, rotated on
every use. A spent one presented again is a copy somebody else is holding, so the grant dies: every
rotation of it, and every session it minted. Revoking only the chain would stop the next refresh
while leaving whatever the replayer already obtained alive until it expired on its own, which is
the half that matters least.

What a backend service cannot yet do is obtain a token at all, because `private_key_jwt` does not
exist and `system/` scopes stay refused. That absence is visible rather than silent:
`grant_types_supported` names `authorization_code` and `refresh_token` only, and a `system/` scope
is reported in `refused` with its reason. A client reading either learns the truth before it
depends on the opposite.

## 11. What a refresh token costs

It is the longest-lived credential this server issues, so its ceiling is the one that matters. It
is thirty days, and it **travels across rotations rather than restarting** (decision): an app
refreshing hourly expires on the same day as one that refreshed once. Without that, the limit
would apply only to apps nobody used, which is precisely backwards.

A spent token's row is kept rather than deleted, and keeps its digest. That digest is no longer a
credential — it authorizes nothing — and exists only to be recognised: a deleted row would answer
"unknown", which is what a guess answers too, and a replay would be indistinguishable from noise.

## 12. What `private_key_jwt` still needs

`project.JWKS` is built: it parses a registration's public key set, refuses a key this server
cannot verify against, refuses one carrying private material, and selects by `kid`. Only RS384 and
ES384 are accepted, stated here rather than read from a token's own header — a verifier that
trusts the header is one an attacker tells which algorithm to use.

**Inline key sets only** (decision). SMART Backend Services lets a client register `jwks` or
`jwks_uri`; a URL would make the token endpoint fetch a client-controlled address on every
authentication, which is an outbound request this server does not otherwise make, an availability
dependency on somebody else's host, and a request-forgery surface aimed at whatever the deployment
can reach. A client pastes its key set instead.

Built since:

- The `jwks` column, its migration, and the `client_assertion_jtis` table
- `project.VerifyClientAssertion` — signature against the registered keys, `iss` and `sub` equal to
  the client id, `aud` equal to the token endpoint, a bounded expiry, and the `jti` and expiry
  handed back so a caller can spend them. The accepted algorithms are a list in that file, checked
  against the header rather than taken from it
- `ClientApplicationStore.SpendAssertion`, where the insert *is* the check: a read that asked
  whether a jti had been seen and an insert that recorded it would leave a window two concurrent
  presentations both passed through
- `authz.ParseScope` accepting `system/`, and `Narrow` treating it as it treats `user/` — because
  what makes it a *system* scope is whose Grants went in, not anything the narrowing does
  differently
- The authorization endpoint refusing `system/` by name. A person cannot approve one: it narrows
  against whoever asks, so one approved by a person would narrow against *their* standing. That is
  the whole safety argument for splitting the two flows

### What the `client_credentials` grant still needs

**A session cannot currently name a machine principal.** `sessions.user_id` carries a foreign key
to `users`, and `Session.Principal()` returns `PrincipalUser` unconditionally. A backend service's
token is a session whose principal is the registration, so the grant needs `sessions` to hold one —
a change to the table every request authenticates through, and not one to make in a hurry.

**A client id does not resolve without a Project.** A client assertion names the client in `iss`
and `sub` and this server in `aud`; it names no Project. The obvious answer — a unique index on
`client_applications (id)`, the way `ux_sessions_token` is tenant-exempt — was tried and is wrong:
this build's own fixtures register the same client id in two Projects, so ids are unique per
Project and not per install. Either the grant carries the Project some other way, or client ids
become install-wide, which is a migration over existing data.

Both are decisions rather than plumbing, which is why the grant is not half-built here.
