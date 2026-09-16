# User domain specification

This document specifies the piece of the control plane that does not exist yet: the `User` domain
type, email normalization, and the storage-backed implementations of the three authorization ports
`packages/authz/scope.go` already declares. Everything it reads is frozen and already committed —
`packages/storage/pocketbase/schema.sql` (the `users` and `project_memberships` tables), `packages/
project/{project,membership,link,bootstrap}.go`, `packages/authz/scope.go` — and this document treats
all of it as binding wherever it speaks. Nothing here is implemented; this is a specification precise
enough that a `UserStore` and the three resolver implementations can be built and tested straight from
it, without writing a second draft.

Where this document makes a design call the committed code and schema leave open, it is marked
**(synthesis)** so an implementer can tell settled fact from this document's own decision. Testable
rules are numbered `IDN-n`, following the `SCH-`/`CP-`/`LNK-`/`AUTH-`/`HIST-` convention already used in
`docs/design/phase2-constraints.md`.

## 1. Where this sits

The domain type lives in a new package, `packages/identity` — **(synthesis)**. `User` is an
authentication identity (email, credential, MFA policy), a different concern from `packages/project`'s
tenancy model (Project, Membership, Link). Nothing in `packages/project` needs to import it:
`project.PrincipalRef` already carries a principal as a bare `PrincipalKind` + `PrincipalID` string, so a
membership never needs to hold a full `User` value. Keeping `identity` separate also keeps
password-hashing concerns out of the package that owns cross-Project isolation.

The three resolver implementations and the `UserStore` implementation live in `packages/storage/
pocketbase`, alongside `resource.go`, the only file with SQL today. This document adds new files there
(`user_store.go`, `membership_resolver.go`, `project_resolver.go`, `policy_resolver.go` — naming is
illustrative); it does not modify `resource.go`, `repository.go`, or `scope.go`.

`packages/authz/scope.go`'s three interfaces (`MembershipResolver`, `ProjectResolver`,
`PolicyResolver`) are **implemented, not altered**. Nothing in this document proposes a signature change
to `packages/authz`.

## 2. The `User` domain type

### 2.1 Column-to-type mapping

| `users` column | Go representation | Notes |
|---|---|---|
| `id` | `identity.UserID` | primary key, unique across every realm |
| `scope` | `identity.Scope` | `server` \| `project` |
| `home_project_id` | `*project.ID` | `nil` iff `scope == ScopeServer` |
| `identity_realm` (GENERATED) | *not stored* — `User.Realm()` computes it | never a settable field (§2.3) |
| `email_normalized` + `email_display` | `identity.Email` | one type, two accessors (§3) |
| `password_hash` | `identity.PasswordHash` | zero value (`""`) means "no credential" |
| `state` | `identity.State` | `invited` \| `active` \| `disabled` (§5) |
| `mfa_required` | `bool` | |
| `created_at`, `updated_at` | *not modeled on `User`* | storage bookkeeping — see §2.4 |
| `version` | `identity.Version` | returned alongside `User` by `UserStore` reads, not embedded (§2.4) |

### 2.2 What the schema already enforces

These need no Go re-validation to hold at the database level, though `NewUser` re-checks the cheap ones
anyway (defense in depth, matching `NewMembership` re-checking the super-admin/project-kind rule the
database also enforces):

1. `id <> ''` and `id` is globally unique (PRIMARY KEY).
2. `scope IN ('server', 'project')`.
3. `(scope = 'project') == (home_project_id IS NOT NULL)` — the paired CHECK.
4. `identity_realm = COALESCE(home_project_id, 'system')` — GENERATED ALWAYS, so Go and SQL cannot
   disagree about which realm a row belongs to (mirrored by `DeriveRealm`, §2.3).
5. `state IN ('invited', 'active', 'disabled')`.
6. `mfa_required IN (0, 1)`.
7. `UNIQUE (identity_realm, email_normalized)` — the load-bearing index this whole document exists to
   feed correctly (§3).
8. `home_project_id REFERENCES projects(id) ON DELETE RESTRICT ON UPDATE RESTRICT` — a user can never
   name a Project that does not exist or gets deleted out from under it.

### 2.3 What only Go enforces

None of the following has a CHECK, trigger, or FK behind it. Each is a real gap a hostile or merely
careless caller can walk through if `identity.NewUser` and `UserStore` do not close it themselves:

- **IDN-1.** `email_normalized` is actually the correct normalization of `email_display` — no
  constraint ties the *content* of the two columns together; a raw `INSERT` could put an arbitrary
  string in either. `identity.ParseEmail` (§3) is the only code path permitted to produce an `Email`
  value, and `UserStore.Create`/writes must persist exactly what it returns, never a re-derived or
  hand-edited pair.
- **IDN-2.** `state = 'invited' ⇒ password_hash IS NULL`, and `state IN ('active', 'disabled') ⇒
  password_hash IS NOT NULL`. Nothing stops a raw `UPDATE users SET state = 'active'` on a row that
  never had a password set. `NewUser` refuses to construct a `User` violating this (§5.2), and
  `UserStore.TransitionState` must re-check it against the *current* row before writing (§6.2).
- **IDN-3.** State transition legality. The CHECK is a membership test against a 3-value enum; it says
  nothing about *sequence*. A raw `UPDATE` can flip `disabled` straight back to `invited`, which §5
  states is never a legal move. `State.CanTransitionTo` is the only place this is enforced.
- **IDN-4.** `scope` and `home_project_id` are immutable after a row is created. There is no
  `users_identity_immutable` trigger analogous to `projects_identity_immutable`. `UserStore` enforces
  this structurally, by exposing no method that can change either field — only `Create` sets them, ever.
- **IDN-5.** Containment between `users` and `project_memberships`: nothing ties a project-scoped
  user's memberships to their own `home_project_id`, or a server-scoped user's memberships to the Super
  Project. This is the sharpest gap given the brief's own framing ("a user in one Project must never
  resolve into another") and is treated separately in §4.3 and §8 rather than folded in here.

### 2.4 Why timestamps and version are not fields on `User`

`project.Membership` and `project.SuperProject` are both built over tables with `created_at`/
`updated_at`/`version` columns, and neither domain type carries them — they are storage bookkeeping,
not identity-meaningful facts a business rule ever reads. `User` follows the same precedent for
`created_at`/`updated_at`: they are not modeled here at all; an admin or audit listing that needs them
queries the row directly rather than through this contract.

`version` is the one deliberate departure, marked **(synthesis)**: unlike `Membership` (rebuilt fresh
from a `Request` on every authorization decision, never itself mutated through a store), `User` is a
long-lived aggregate whose password and lifecycle state change independently of any single request.
Optimistic concurrency needs somewhere to live for that to be safe, and the minimal, precedented place
— mirroring how `storage.ResourceRecord` carries its `Version` — is returned alongside the value rather
than embedded in it, since `User`'s own fields stay pure identity facts.

### 2.5 Exact Go types

```go
package identity

// UserID identifies one row in the identity registry, unique across every
// realm — never reused, and never itself the value uniqueness is checked on.
type UserID string

// Scope separates an identity anchored to no Project from one anchored to
// exactly one. It is fixed at creation and never changes (IDN-4).
type Scope string

const (
	ScopeServer  Scope = "server"
	ScopeProject Scope = "project"
)

// Valid reports whether the scope is one this server recognises.
func (s Scope) Valid() bool {
	return s == ScopeServer || s == ScopeProject
}

// IdentityRealm is the uniqueness scope email_normalized is compared within: a
// Project's own id for a project-scoped user, the literal "system" otherwise.
type IdentityRealm string

// SystemRealm is the realm every server-scoped identity shares, mirroring the
// database's COALESCE(home_project_id, 'system') exactly.
const SystemRealm IdentityRealm = "system"

// DeriveRealm computes the realm the same way the GENERATED column does, so
// Go can never compute a different answer than the row it is about to write.
func DeriveRealm(homeProject *project.ID) IdentityRealm {
	if homeProject == nil {
		return SystemRealm
	}

	return IdentityRealm(*homeProject)
}

// User is one identity: server-scoped (administers the install, owns no
// Project) or project-scoped (belongs to exactly one). Every field is
// unexported; NewUser is the only path a value is built through.
type User struct {
	id           UserID
	scope        Scope
	homeProject  *project.ID
	email        Email
	passwordHash PasswordHash
	state        State
	mfaRequired  bool
}

// UserConfig is the input to NewUser, and the shape a store rehydrates a
// persisted row through — mirroring project.InstanceConfig, which carries
// storage-shaped fields (TokenHash, TokenExpires) the same way.
type UserConfig struct {
	ID           UserID
	Scope        Scope
	HomeProject  *project.ID
	Email        Email
	PasswordHash PasswordHash
	State        State
	MFARequired  bool
}

// NewUser builds an identity, refusing every row the schema's own CHECKs
// would refuse plus every invariant only Go can see (IDN-1..IDN-3).
func NewUser(cfg UserConfig) (User, error) {
	if cfg.ID == "" {
		return User{}, fmt.Errorf("%w: user", ErrMissingID)
	}

	if !cfg.Scope.Valid() {
		return User{}, fmt.Errorf("%w: %q", ErrUnknownScope, string(cfg.Scope))
	}

	if (cfg.Scope == ScopeProject) != (cfg.HomeProject != nil) {
		return User{}, ErrScopeHomeProjectMismatch
	}

	if cfg.HomeProject != nil {
		if err := project.ValidateID(*cfg.HomeProject); err != nil {
			return User{}, err
		}
	}

	if cfg.Email.Normalized() == "" {
		return User{}, ErrMissingEmail
	}

	if !cfg.State.Valid() {
		return User{}, fmt.Errorf("%w: %q", ErrUnknownState, string(cfg.State))
	}

	// IDN-2: the schema allows any (state, password_hash) pairing. This is the
	// one place that pairing is actually checked before a User can exist.
	hasPassword := cfg.PasswordHash != ""
	if (cfg.State == StateInvited) == hasPassword {
		return User{}, ErrInvalidCredentialState
	}

	return User{
		id: cfg.ID, scope: cfg.Scope, homeProject: cfg.HomeProject,
		email: cfg.Email, passwordHash: cfg.PasswordHash,
		state: cfg.State, mfaRequired: cfg.MFARequired,
	}, nil
}

// ID returns the identity's identifier.
func (u User) ID() UserID { return u.id }

// Scope returns whether this identity is anchored to a Project.
func (u User) Scope() Scope { return u.scope }

// HomeProject returns the Project this identity belongs to, if any.
func (u User) HomeProject() (project.ID, bool) {
	if u.homeProject == nil {
		return "", false
	}

	return *u.homeProject, true
}

// Realm returns the uniqueness scope this identity's email is compared
// within, computed the same way the database's GENERATED column is.
func (u User) Realm() IdentityRealm { return DeriveRealm(u.homeProject) }

// Email returns the identity's validated address.
func (u User) Email() Email { return u.email }

// HasCredential reports whether a password has ever been set. It is false
// only in StateInvited (IDN-2).
func (u User) HasCredential() bool { return u.passwordHash != "" }

// State returns the identity's lifecycle position.
func (u User) State() State { return u.state }

// MFARequired reports whether this identity's policy demands a second factor.
func (u User) MFARequired() bool { return u.mfaRequired }
```

`PasswordHash` is never exposed for comparison here — hash verification (bcrypt/argon2id or similar) is
a login-handler concern, out of scope for this document. What matters domain-wise is only *whether* one
is set, which `HasCredential` answers without ever handing the hash to a caller that does not already
have it from its own store read.

```go
// PasswordHash is an opaque credential hash. Its zero value means "no
// credential yet" (IDN-2) — never a real hash, which is never the empty string.
type PasswordHash string

// String redacts, mirroring project.ClaimToken, so a hash cannot reach a log
// line through ordinary formatting.
func (h PasswordHash) String() string {
	if h == "" {
		return ""
	}

	return "[redacted password hash]"
}
```

## 3. Email normalization

This is the one piece of this specification where a wrong call is a security incident, not a bug
report. Read this section as: *for a healthcare identity system, a false duplicate is an inconvenience;
a false collapse is a cross-identity leak.* Every decision below picks the failure that produces a
duplicate account over the one that could let two different mailboxes resolve to one identity.

### 3.1 What normalization protects, and what it does not

`identity_realm` already stops the catastrophic case named in the brief — one Project's user resolving
into another's — structurally, before normalization ever runs: a realm *is* a Project id (or `system`),
so no email-normalization mistake can make a `CLINIC-A` row answer for a `CLINIC-B` lookup. What
normalization actually governs is narrower but still severe for a single-tenant healthcare Project: two
different real people (two clinicians, or a clinician and a patient) inside the *same* realm being
treated as one row, one password, one audit identity. That is the account-takeover-shaped failure this
section exists to close. The account-registration-blocked failure — two spellings of the same mailbox
being treated as two rows — is the failure this section is willing to accept when it cannot be sure.

### 3.2 The exact algorithm

`ParseEmail` is the only function permitted to produce an `Email` value (IDN-1). It is pure: same input,
same output, every time, with no locale or provider lookup.

```go
package identity

// Email is a validated address: the exact value the unique index compares
// (Normalized) and the exact value a human should see (Display). Constructing
// one is the only place normalization happens (IDN-1).
type Email struct {
	normalized string
	display    string
}

// ParseEmail validates raw input and computes both stored forms. It never
// guesses: a construct it cannot be sure is safe to collapse, it rejects
// rather than silently normalizing (§3.1).
func ParseEmail(raw string) (Email, error) { ... }

// Normalized is the exact value ux_users_realm_email compares.
func (e Email) Normalized() string { return e.normalized }

// Display is the address as entered, with only surrounding whitespace and a
// bare trailing domain dot removed — never case-folded or re-encoded, so a
// person always sees their own address spelled the way they typed it.
func (e Email) Display() string { return e.display }
```

Steps, in order, every one of them a rejection point unless stated otherwise:

1. **Reject** an empty string.
2. **Trim** leading and trailing Unicode whitespace (Go: `strings.TrimSpace`). This touches only the
   two ends of the string — never a character in the middle.
3. **Reject** if any remaining character is a control character (`U+0000`–`U+001F`, `U+007F`) or any
   Unicode whitespace/space-separator codepoint. Interior whitespace is never silently stripped — a
   pasted address with an embedded space is refused, not repaired, because stripping it could collapse
   a legitimate RFC 5322 quoted local part onto an unrelated unquoted one.
4. **Reject** unless the string contains **exactly one** `@`. Zero or more than one is a syntax error.
   Split into `local` and `domain` at that character.
5. **Local part** (`local`):
   - Reject empty, or longer than 64 octets (RFC 5321 §4.5.3.1.1).
   - Reject any codepoint outside `ALPHA / DIGIT / "!#$%&'*+-/=?^_\`{|}~" / "."` — pure ASCII
     dot-atom form only. Quoted local parts (`"john smith"@…`) are not supported in v1; reject them
     rather than guess at their meaning.
   - Reject if it starts with `.`, ends with `.`, or contains `..`.
   - **Do not** strip a `+tag` suffix (§3.3, plus-addressing).
   - **Do not** collapse or remove interior `.` characters (§3.3, Gmail-style dot-folding).
   - Lowercase using plain ASCII case folding (`A`-`Z` → `a`-`z`) — safe because step 5's charset check
     already guarantees pure ASCII content.
6. **Domain part** (`domain`):
   - Reject empty.
   - Strip **at most one** trailing `.` (the DNS root dot — `example.com.` and `example.com` name the
     same DNS record, so this is the one domain-side collapse the algorithm performs).
   - Reject if longer than 255 octets after that strip.
   - Split on `.`; reject if any label is empty or longer than 63 octets.
   - Pass the **whole domain, unconditionally** — ASCII or not — through IDNA (`golang.org/x/net/idna`,
     `Lookup` profile, `Transitional(false)`, `VerifyDNSLength(true)`) `ToASCII`. One code path for
     every domain, so an ASCII-lookalike input can never skip the validation a genuinely Unicode domain
     would get. `ToASCII` performs Unicode normalization and lowercasing as part of UTS #46 and rejects
     disallowed codepoints outright rather than substituting a guess. Any error here is a rejection.
   - Reject if the result has fewer than two labels (no bare-TLD domains) — a v1 default; note if an
     internal deployment genuinely needs single-label hostnames.
7. Reject if `local + "@" + domain` exceeds 254 octets total (RFC 5321 §4.5.3.1.3).
8. `Normalized` = the lowercased local part (step 5) + `@` + the IDNA-processed domain (step 6).
   `Display` = the *original-case, original-script* local part and domain as entered, with only the
   step-2 trim and the step-6 trailing-dot strip applied — never lowercased, never punycode-encoded.

Unicode normalization (NFC, inside IDNA's UTS #46 processing) touches **only the domain**, and only
through the standard IDNA mechanism — never a hand-rolled NFKC pass over the whole address, and never
applied to the local part at all, which is restricted to ASCII in v1 specifically to sidestep that class
of bug entirely (§3.3).

### 3.3 The six confusable cases, decided

| Case | Collapse? | Reasoning |
|---|---|---|
| **Case** (`Alice@X.com` vs `alice@x.com`) | **Collapse** (lowercase both parts) | RFC 5321 technically makes the local part case-sensitive, but ~0% of real mail providers honor it — every mainstream provider delivers case-insensitively. Standard practice (Auth0, Okta, GitHub) folds case for the identity key. Safe here specifically because (a) only one row can exist per normalized value, so a second registration with different casing is refused, never merged into someone else's live session; (b) delivery always uses `Display`, the literal casing entered at creation, never the folded value; (c) login still requires the password — email match alone grants nothing. The realistic risk of the *opposite* choice (treating case as distinct) is worse: an offboarded employee's `Alice@corp.example` account survives revocation of `alice@corp.example` as an untouched shadow identity — a genuine compliance gap, not merely a duplicate-account nuisance. |
| **Surrounding whitespace** | **Collapse** (trim only) | Never a real distinguishing feature of a mailbox — pure input hygiene from copy-paste. Interior whitespace is a different case (item above, §3.2 step 3): rejected outright, never stripped, because collapsing it *could* merge a legitimate quoted local part into an unrelated unquoted one. |
| **Unicode NFKC / compatibility folding** | **Do not apply** (not attempted anywhere) | NFKC folds things like full-width Latin (`Ａ`→`A`), Roman numerals, superscripts and ligatures — and does **not** fold across scripts, so it does not even solve the homoglyph problem it is often reached for. Applying it to the local part risks silently merging two different real SMTPUTF8 mailboxes that a human would never consider related. The only Unicode transform this algorithm performs is IDNA's UTS #46 processing, applied to the domain only, which is the standards-defined mechanism for that one field — never a general NFKC pass over the whole address. |
| **Homoglyphs** (Cyrillic `а` vs Latin `a`) | **Must NOT collapse** | Cross-script confusable detection (Unicode TR39, IDNA's single-script label restrictions) is a materially harder problem than normalization and is out of scope for v1. The algorithm makes no attempt at it: `аpple.com` (Cyrillic а) and `apple.com` encode to two different punycode strings and are — correctly — two different, unrelated identities. Treating them as equal would be the takeover risk itself: whoever controls the confusable-but-different domain could "become" a user at the real one. |
| **Plus-addressing** (`alice+billing@x.com`) | **Must NOT collapse** | Whether `+` denotes a provider-level alias to the same mailbox (Gmail) or a genuinely distinct, separately-owned mailbox (most other mail systems, including many hospital mail platforms that provision structured `+`-tagged addresses per department) is provider-specific and this server cannot know it from the string alone. Stripping it universally creates the sharpest concrete misdelivery scenario in this whole document: if `alice+ops@corp.example` registers first and normalization strips the tag, a later invite genuinely addressed to `alice@corp.example` resolves as "already exists," and any password-reset/claim flow keyed off that identity reaches `alice+ops@corp.example`'s inbox — an address a completely different person may control. This is preserved verbatim, unconditionally, with no allowlist of "known alias-supporting domains" in v1. |
| **Trailing dots** | **Domain: collapse** (strip one trailing `.`). **Local part: reject**, never strip. | A trailing dot on a domain is DNS's own root-dot notation — `example.com.` and `example.com` are the same DNS name by definition, so stripping it loses no information and creates no ambiguity. A trailing dot in the *local part* (`alice.@x.com`) is invalid RFC 5321 syntax outright; silently repairing it would treat malformed input as if it had been typed correctly, which this algorithm never does — it is rejected at §3.2 step 5. |

### 3.4 The governing rule, stated plainly

**IDN-6.** Where the algorithm cannot be certain, from mail-routing semantics that hold for *every*
provider, that two strings name the same mailbox, it keeps them distinct — accepting a possible
duplicate account over a possible cross-identity collapse. The two collapses this algorithm performs
(case folding, and the two whitespace/dot hygiene strips) are each true for literally every SMTP
implementation in existence; the two it refuses (plus-addressing, dot-folding in the local part) are
each true for exactly one well-known provider and false, or unknowable, for everyone else.

### 3.5 Worked cases (rules a test can be written against)

| Input | `Normalized` | `Display` | Outcome |
|---|---|---|---|
| `"  Alice@Example.COM  "` | `alice@example.com` | `Alice@Example.COM` | accepted |
| `alice+billing@corp.example` | `alice+billing@corp.example` | `alice+billing@corp.example` | accepted, tag preserved |
| `alice.smith@corp.example` | `alice.smith@corp.example` | `alice.smith@corp.example` | accepted, interior dot preserved |
| `alice@Example.com.` | `alice@example.com` | `alice@example.com` | accepted, trailing DNS dot dropped from both forms |
| `alice.@corp.example` | — | — | **rejected**: trailing dot in local part |
| `alice..smith@corp.example` | — | — | **rejected**: consecutive dots in local part |
| `"john smith"@corp.example` | — | — | **rejected**: quoted local part unsupported in v1 |
| `Ａlice@corp.example` (full-width `Ａ`) | — | — | **rejected**: non-ASCII local-part codepoint |
| `alice@café.example` | `alice@xn--caf-dma.example` | `alice@café.example` | accepted; `Display` keeps the human-readable Unicode label, `Normalized` carries the IDNA-encoded form |
| `alice@xn--caf-dma.example` | `alice@xn--caf-dma.example` | `alice@xn--caf-dma.example` | accepted; **must** equal the previous row's `Normalized` — IDNA guarantees the Unicode and punycode spellings of the same domain collapse |
| `alice@apple.com` vs `alice@аpple.com` (Cyrillic а) | two different values | — | both accepted as **distinct** identities — confirms IDN-6's homoglyph decision |
| `alice@localhost` | — | — | **rejected**: fewer than two domain labels |

## 4. Server-scoped vs. project-scoped

### 4.1 Definitions

- **Server-scoped** (`Scope() == ScopeServer`, `HomeProject()` absent, `Realm() == SystemRealm`): an
  identity with no owning Project. In the committed model this population exists to hold standing in
  the Super Project — it is who a Super Admin *is*.
- **Project-scoped** (`Scope() == ScopeProject`, `HomeProject()` present): an identity anchored to
  exactly one Project. Its email is unique only within that Project's own realm — the same literal
  address can independently exist as an unrelated row in a different Project, by design (the schema
  comment on `identity_realm` says this explicitly).

### 4.2 What a project-scoped user may never do

- **May never hold standing outside its home Project.** A project-scoped user's only legitimate
  `project_memberships` row is in `Project = HomeProject()`. It must never hold a membership — admin,
  ordinary, or link-minted — in any other Project, including the Super Project. Reach into another
  Project's *data* is exclusively the `project_link` mechanism operating between Projects
  (`authz.linkGrants`, already committed); it is never achieved by the same identity acquiring a second
  membership elsewhere. This is exactly the invariant the brief names as the worst possible failure, and
  it is **not** enforced anywhere in the schema (§4.3, §8) — only the write path that creates a
  `project_memberships` row can enforce it, because the read path (`MembershipResolver`, §6.3) is
  already safe by construction: a query scoped to `(project_id = ?, principal_id = ?)` simply returns no
  row when none was ever (wrongly) written there.
- **May never hold `super_admin = 1`.** This already follows from the point above once membership
  containment holds: `super_admin` requires a membership in the Super Project (`project_kind = 'super'`,
  schema CHECK), and a project-scoped user is never a member of any Project but its own.
- **May never change its own `HomeProject`** — see IDN-4. There is no supported operation that moves an
  identity from one realm to another; an identity that changes Project is a new identity, created fresh.
- **May never be looked up by `ByEmail` without naming its realm.** `UserStore.ByEmail` takes
  `(realm, normalized)`, never `normalized` alone (§6.1) — there is no lookup surface, anywhere, that
  treats an email address as globally unique across the whole install.

A server-scoped user is bound by the mirror-image rule: its only legitimate `project_memberships` rows
are in the Super Project. **(synthesis)** — nothing in the committed schema names this population's
scope this narrowly, but it is the only interpretation consistent with the brief's framing and with
`identity_realm` partitioning identity by Project in the first place; a server-scoped identity that could
also hold an ordinary membership in an arbitrary standard Project would reintroduce exactly the
cross-Project identity blending the realm design exists to prevent.

### 4.3 Why this needs Go, not SQL, today

`project_memberships.user_id REFERENCES users(id)` only guarantees the referenced row *exists* — it says
nothing about whether that row's `home_project_id` matches the membership's own `project_id`. The schema
already shows the pattern that *would* close this: `project_kind` is denormalized onto
`project_memberships` specifically so the `super_admin = 0 OR project_kind = 'super'` CHECK can see it
without a subquery. The identical fix here would denormalize the referenced user's `scope` and
`home_project_id` onto `project_memberships` and add the matching CHECK/composite-FK pair. This document
does not make that schema change — see §8 for why, and why it is flagged rather than applied silently
per this task's own constraint on editing `schema.sql`.

Until that lands, containment is a **write-time** invariant only: whatever code path creates a
`project_memberships` row (membership provisioning, invite acceptance, bootstrap's `firstSuperAdmin`)
must read the target user's `scope`/`home_project_id` and refuse to write a row that would violate §4.2,
denying rather than defaulting to "allow." The **read** path needs no equivalent guard — see §6.3.

## 5. State transitions

### 5.1 The state machine

```go
package identity

// State is an identity's lifecycle position.
type State string

const (
	StateInvited  State = "invited"
	StateActive   State = "active"
	StateDisabled State = "disabled"
)

// Valid reports whether the state is one this server recognises.
func (s State) Valid() bool {
	switch s {
	case StateInvited, StateActive, StateDisabled:
		return true
	default:
		return false
	}
}

// CanTransitionTo reports whether the lifecycle allows this move. No state
// ever returns to StateInvited — resetting a forgotten password is a
// separate, token-based flow, never a raw state flip back to "not yet set up."
func (s State) CanTransitionTo(next State) bool {
	if s == next || !next.Valid() {
		return false
	}

	switch s {
	case StateInvited:
		return next == StateActive || next == StateDisabled
	case StateActive:
		return next == StateDisabled
	case StateDisabled:
		return next == StateActive
	default:
		return false
	}
}

// TransitionTo returns the next state, or refuses the move.
func (s State) TransitionTo(next State) (State, error) {
	if !s.Valid() {
		return "", fmt.Errorf("%w: %q", ErrUnknownState, string(s))
	}

	if !s.CanTransitionTo(next) {
		return "", fmt.Errorf("%w: %s to %s", ErrInvalidTransition, s, next)
	}

	return next, nil
}
```

```
invited ──accept invitation (sets password)──▶ active ◀──▶ disabled
   │
   └──revoke (before ever accepted)──────────▶ disabled
```

No state is terminal, unlike `project.State`'s `StateDeleting` or `project.MembershipState`'s
`MembershipRevoked`. `disabled` is fully reversible — an admin can re-enable an identity — because
disabling here means "this credential may not authenticate," not "this identity is being purged."

### 5.2 The coupling with `password_hash` (IDN-2)

- `StateInvited` **must** carry no credential (`HasCredential() == false`). This is the state an
  identity is created in when it is provisioned before the person has set anything.
- `StateActive` and `StateDisabled` **must** carry a credential. Once set, a credential survives a
  `active → disabled → active` round trip untouched — disabling does not erase it, and re-enabling does
  not require setting it again.
- The **only** transition that may move `password_hash` from unset to set is `invited → active`, and it
  must do so in the same atomic write as the state change (§6.2) — there must be no observable
  intermediate row where `state = 'active'` and `password_hash IS NULL`, or vice versa.
- `mfa_required` is an independent policy flag, settable in any state; it is not modeled as part of the
  state machine at all. Whether it is *enforced* (a login is refused without a second factor) is a login-
  handler concern, out of scope here — this document only guarantees the flag itself survives every
  transition unchanged.

## 6. `UserStore` and the three resolvers

### 6.1 `UserStore`

```go
package identity

// Version is the optimistic-concurrency counter the users.version column
// carries. It is unrelated to storage.VersionID, which names one immutable
// FHIR resource version, not one generation of a mutable identity row.
type Version int64

// UserStore persists identities. It exposes no method that can change Scope
// or HomeProject after Create (IDN-4) — an identity that must move to a
// different realm is a new identity, not an update to this one.
type UserStore interface {
	// ByID reads one identity by its id, in whatever realm it belongs to. An
	// absent row is (User{}, 0, false, nil) — never an error. A caller must
	// treat found=false as an outright denial, never proceed with the zero
	// value as if it authorized or identified anything.
	ByID(ctx context.Context, id UserID) (User, Version, bool, error)

	// ByEmail resolves one identity inside one realm, comparing the
	// Normalized form only (§3). It is the one lookup a login or invite flow
	// may use to test "does this address already exist here" — never a query
	// against Display, and never one that omits realm (§4.2).
	ByEmail(ctx context.Context, realm IdentityRealm, normalized string) (User, Version, bool, error)

	// Create inserts a new identity at Version 1. It fails with
	// ErrEmailTaken, never overwrites, when (realm, normalized email) is
	// already claimed — the same guarantee ux_users_realm_email enforces,
	// surfaced as a typed error instead of a raw constraint violation.
	Create(ctx context.Context, user User) (Version, error)

	// AcceptInvitation is the one path StateInvited -> StateActive may travel
	// through: it sets the credential and advances the state in one
	// transaction, so no reader can ever observe the two facts disagreeing
	// (§5.2). It fails if the current state is not StateInvited.
	AcceptInvitation(ctx context.Context, id UserID, hash PasswordHash, expect Version) (Version, error)

	// SetPassword resets the credential on an already-accepted identity
	// (State != StateInvited). It never changes State — "a credential was
	// replaced" and "the account is active" are two different facts an audit
	// must be able to tell apart.
	SetPassword(ctx context.Context, id UserID, hash PasswordHash, expect Version) (Version, error)

	// TransitionState applies invited->disabled, active->disabled or
	// disabled->active (State.CanTransitionTo, §5.1) — every move except the
	// one AcceptInvitation owns. It refuses StateActive as a target (that
	// path requires AcceptInvitation) and StateInvited as a target (no state
	// ever returns to it), before ever attempting a write.
	TransitionState(ctx context.Context, id UserID, next State, expect Version) (Version, error)
}
```

Every write method takes `expect Version`, the same optimistic-concurrency shape
`storage.ResourceRepository.Update`/`.Delete` already use: a version mismatch is a distinct, typed
`ErrVersionConflict`, never silently overwritten and never confused with "no such user."

### 6.2 `TransitionState` and `AcceptInvitation`, concretely

Both must re-read (or `UPDATE ... WHERE version = ?` and check rows-affected against) the current row
inside the same transaction as the write, for the same reason `pocketbase.ResourceStore.mutate` binds
its authorization predicate into the write statement itself rather than checking-then-writing as two
steps: a check-then-act gap is a race, not a guarantee.

```
AcceptInvitation(id, hash, expect):
    UPDATE users
    SET password_hash = ?, state = 'active', version = version + 1, updated_at = ?
    WHERE id = ? AND version = ? AND state = 'invited' AND password_hash IS NULL
    -- zero rows affected: distinguish "wrong version" from "wrong state" by a
    -- follow-up read under the same transaction, mirroring
    -- ResourceStore.explainMiss, so the caller learns which precondition failed.

TransitionState(id, next, expect):
    if next == StateActive:  return ErrInvalidTransition  -- AcceptInvitation's path only
    if next == StateInvited: return ErrInvalidTransition  -- no state returns to invited
    current, version, found, err := read row for update
    if !found: return ErrNotFound
    if version != expect: return ErrVersionConflict
    next, err := current.state.TransitionTo(next)   -- §5.1, reused verbatim
    if err != nil: return err
    UPDATE users SET state = ?, version = version + 1, updated_at = ? WHERE id = ? AND version = ?
```

### 6.3 `MembershipResolver` (pocketbase)

```go
package pocketbase

// MembershipResolver implements authz.MembershipResolver by joining
// project_memberships to users: a membership's standing depends on the
// identity behind it, which project_memberships.state alone cannot see.
type MembershipResolver struct{ db *sql.DB }

func (r *MembershipResolver) Membership(
	ctx context.Context, proj project.ID, principal project.PrincipalRef,
) (project.Membership, bool, error) {
	...
}
```

Behavior:

- The query is always scoped by `project_id = ?` (the passed `proj`) and `principal_id = ?`. A
  project-scoped user who was never (correctly) given a row in this Project therefore returns
  `(Membership{}, false, nil)` automatically — no separate containment check is needed on read (§4.3);
  the containment invariant only has to be enforced where the row gets written.
- When `principal.Kind == PrincipalUser`, the query **must** join `users` on `user_id` and require
  `users.state = 'active'`. A membership whose `project_memberships.state` is `'active'` but whose
  underlying `users.state` is `'disabled'` (or whose `users` row is somehow absent, which RESTRICT
  should make impossible but a resolver must not assume) reports `found = false`, **not** a `Membership`
  with a degraded state. Two reasons to hide it rather than degrade it: `project.MembershipState` has no
  value that means "the identity behind this is disabled," so degrading it would require picking a
  lie (e.g. `MembershipSuspended`) that misrepresents `project_memberships`'s own real column; and this
  is exactly the defense-in-depth case where a still-valid session token outlives an admin disabling the
  underlying identity — `BuildScope` must start returning empty scopes on the very next call, without
  waiting for the session to expire on its own.
- When `principal.Kind` is `PrincipalClientApplication` or `PrincipalBot`, no `users` join applies — those
  tables carry no identity of this shape (schema comment: "client_applications and bots are separate
  tables that do not exist yet"). Only `project_memberships.state` gates standing for those kinds.
- On any driver error, return the error, never a zero-value "not found" — `BuildScope` already
  distinguishes `err != nil` from `!found` and wraps the error with `"authz: resolve membership in %s"`
  (committed).

### 6.4 `ProjectResolver` (pocketbase)

```go
package pocketbase

type ProjectResolver struct{ db *sql.DB }

func (r *ProjectResolver) State(ctx context.Context, proj project.ID) (project.State, error) {
	...
}
```

The committed interface — `State(ctx, proj) (project.State, error)` — has **no `found bool`**, unlike
`MembershipResolver` and `PolicyResolver`. This is a real, load-bearing asymmetry worth stating
explicitly rather than papering over:

- **IDN-7.** A `proj` that names no row in `projects` **must** return a non-nil error (wrapping
  `sql.ErrNoRows`), never `(project.State(""), nil)`. `project.State`'s four values (`active`,
  `suspended`, `archived`, `deleting`) each describe a Project that genuinely exists; using any of them,
  or the empty string, to *also* mean "never existed" would corrupt every log line and audit record that
  reads this value, conflating "exists but suspended" with "was purged or never provisioned." Yes,
  `State("").Admits(action)` already happens to evaluate to `false` for every action (the `switch`'s
  default case), so a implementation that returned `("", nil)` would still *deny* correctly by accident —
  but `BuildScope` would then report success (`err == nil`) for a request naming a nonexistent Project,
  which is observably different from, and strictly worse than, the error `admits` is written to
  propagate (`fmt.Errorf("authz: resolve state of %s: %w", proj, err)`, already committed). An
  implementation must not rely on the accidental correctness of the empty string and skip returning the
  error.

### 6.5 `PolicyResolver` (pocketbase)

```go
package pocketbase

type PolicyResolver struct{ db *sql.DB }

func (r *PolicyResolver) Policy(ctx context.Context, ref project.PolicyRef) (authz.AccessPolicy, bool, error) {
	...
}
```

Behavior:

- Reads `access_policies` + `access_policy_parameters` + `access_policy_rules` for
  `(project_id, id) = (ref.Project(), ref.ID())`, and reconstructs the value through
  `authz.NewAccessPolicy` (never by populating `AccessPolicy`'s unexported fields directly — that
  constructor is the only place the "a rule may not name an undeclared parameter" check runs).
- An absent policy row returns `(AccessPolicy{}, false, nil)` — not an error. `authz.compile` (committed)
  already documents why: *"a policy that no longer exists says nothing, and nothing is not permission."*
- The reconstructed value's `Project()`/`ID()` **must** equal `ref.Project()`/`ref.ID()` exactly, so
  `AccessPolicy.Matches(ref)` (checked by `compile`) succeeds — a resolver that ever returned a policy
  keyed differently than what was asked would trip `authz.ErrPolicyMismatch`, which is meant to catch a
  resolver bug, never a legitimate answer.
- Rule reconstruction: an `access_policy_rules` row with `unrestricted = 1` becomes
  `authz.NewUnrestrictedRule(kind, res_type, action)`; one with `compartment_id` set becomes a
  `LiteralSubject` fed to `authz.NewRule`; one with `compartment_param` set becomes a `ParameterSubject`.
  This is exactly the three-way disjunction the schema's own CHECK on `access_policy_rules` already
  enforces (§ schema, "unrestricted = 1 ... OR unrestricted = 0 AND ... id ... OR ... param"), so the
  resolver's `switch` over those three shapes should never hit a fourth case — if it does, that is a
  schema/Go drift bug, and it must error rather than guess which of the three was intended.

## 7. Rules a test can be written against

**Domain type (§2)**
- `NewUser` refuses `Scope` paired with the wrong `HomeProject` nilness, an invalid `State`, an empty
  `Email`, and every `(State, HasCredential)` pairing except `(invited, false)`, `(active, true)`,
  `(disabled, true)` (IDN-2, IDN-3).
- `User.Realm()` and `identity.DeriveRealm` agree with each other and with
  `COALESCE(home_project_id, 'system')` for every `(scope, home_project_id)` pair the schema's CHECK
  allows.

**Email (§3)**
- Every row of the §3.5 table holds exactly as written, including that the Unicode and IDNA-encoded
  spellings of the same domain (`café.example` / `xn--caf-dma.example`) produce the identical
  `Normalized` value, and that a Cyrillic/Latin homoglyph pair produces two *different* ones.
  `ParseEmail` never strips a `+` suffix and never removes an interior `.` from the local part, for any
  input (IDN-6).
- `ParseEmail` rejects: empty input; more or fewer than one `@`; a local part over 64 octets; a domain
  over 255 octets (post trailing-dot strip); a total over 254 octets; a local part with a non-dot-atom
  ASCII character or any non-ASCII character; a local part starting/ending with `.` or containing `..`;
  a domain that fails IDNA `ToASCII`; a domain with fewer than two labels.
- `ParseEmail` never returns an error together with a non-empty `Email`, and never a non-error result
  with an empty `Normalized`.

**Server-scoped vs. project-scoped (§4)**
- No code path can write a `project_memberships` row for a project-scoped user outside its
  `HomeProject`, nor for a server-scoped user outside the Super Project (enforced at the write site
  named in §4.3, since the schema does not — §8).
- `MembershipResolver.Membership(ctx, otherProject, principalOfAUserHomedElsewhere)` returns
  `(Membership{}, false, nil)` given a correctly-written database (no row exists to find) — this holds
  even without the write-time guard, as a regression backstop.
- No exported `UserStore` or `identity` method can change an existing identity's `Scope` or
  `HomeProject` (IDN-4) — an architecture test enumerating `UserStore`'s method set should assert this
  directly, the same way CP-7 asserts `storage.NewScope`'s call-site closure.

**State transitions (§5)**
- `State("invited").CanTransitionTo("active")`, `("invited","disabled")`, `("active","disabled")`,
  `("disabled","active")` are the **only** four `true` results across all nine ordered pairs (three
  states × three states, minus the three no-op `s == next` cases already `false`).
- `AcceptInvitation` fails (no write) if the current state is not `invited`, or if `expect` does not
  match the current version — and on success, a row read back has `state = 'active'` and a non-empty
  `password_hash` with no intermediate state observable by a concurrent reader (§5.2, §6.2).
- `TransitionState` refuses `next = StateActive` and `next = StateInvited` unconditionally, before
  touching the database.

**Resolvers (§6)**
- `MembershipResolver.Membership` returns `found = false` for: no row at all; a row whose
  `project_memberships.state = 'revoked'`; a `PrincipalUser` row whose joined `users.state != 'active'`.
  It returns `found = true` with the row's real state for a `PrincipalClientApplication`/`PrincipalBot`
  row regardless of any `users` state (no join applies).
- `ProjectResolver.State` returns a non-nil error, never `("", nil)`, for a `proj` with no row in
  `projects` (IDN-7).
- `PolicyResolver.Policy` returns `found = false`, not an error, for a `ref` naming no row; the returned
  `AccessPolicy`'s `Project()`/`ID()` equal `ref.Project()`/`ref.ID()` in every case it returns
  `found = true`.

## 8. Schema gap observed — flagged, not fixed

Per this task's constraint, `packages/storage/pocketbase/schema.sql` is not edited here. One gap is
worth recording explicitly rather than leaving implicit in §4.3's prose, because it is the one place
this document's own containment rule (§4.2) has no database backstop at all:

**`project_memberships` carries no column tying its `project_id` to the referenced user's
`home_project_id`/`scope`.** The schema already solves an identically-shaped problem —
`super_admin` confined to the Super Project — by denormalizing `project_kind` onto
`project_memberships` and adding `CHECK (super_admin = 0 OR project_kind = 'super')`
plus the composite FK `(project_id, project_kind) REFERENCES projects(id, kind)`. The same
pattern would close this gap: add a denormalized column (e.g. `principal_home_project_id`,
nullable, populated only when `principal_kind = 'user'`) and a composite FK against
`users(id, home_project_id)`, plus a CHECK that `project_id` equals that column whenever it is
non-null (and, for a server-scoped principal, that `project_kind = 'super'`). This is a genuine
missing constraint, not a stylistic preference — it is the database-level version of exactly the
invariant the brief calls the worst possible failure — but adding it is a schema change with its
own migration and denormalization-consistency concerns, which this research task's own instructions
reserve for a deliberate, separate decision rather than a silent edit made here. Until it lands,
§4.2/§4.3's write-time Go guard is the *only* thing enforcing containment, and it should be treated
as a P0 gap by whoever implements membership provisioning next — not a nice-to-have hardening pass.
