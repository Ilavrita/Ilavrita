package project

import (
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/net/idna"
)

// UserID identifies one row in the identity registry. It is unique across every
// realm, and it is never the value an email lookup is keyed on.
type UserID string

// UserScope separates an identity anchored to no Project from one anchored to
// exactly one. It is fixed when the identity is created and never changes.
type UserScope string

// The scopes an identity can hold.
const (
	ScopeServer  UserScope = "server"
	ScopeProject UserScope = "project"
)

// Valid reports whether the scope is one this server recognises.
func (s UserScope) Valid() bool {
	return s == ScopeServer || s == ScopeProject
}

// IdentityRealm is the scope an email address is unique within: a Project's own
// id for a project-scoped identity, the system realm for a server-scoped one.
type IdentityRealm string

// SystemRealm is the realm every server-scoped identity shares, mirroring the
// literal the generated column coalesces to.
const SystemRealm IdentityRealm = "system"

// DeriveRealm computes the realm the way the generated column does, so Go can
// never disagree with the row it is about to write. The zero id is SQL NULL.
func DeriveRealm(home ID) IdentityRealm {
	if home == "" {
		return SystemRealm
	}

	return IdentityRealm(home)
}

// UserState is an identity's lifecycle position.
type UserState string

// The lifecycle states of an identity.
const (
	UserInvited  UserState = "invited"
	UserActive   UserState = "active"
	UserDisabled UserState = "disabled"
)

// Valid reports whether the state is one this server recognises.
func (s UserState) Valid() bool {
	switch s {
	case UserInvited, UserActive, UserDisabled:
		return true
	default:
		return false
	}
}

// CanTransitionTo reports whether the lifecycle allows this move. Nothing
// returns to invited: a forgotten password is a token-based reset, never a flip
// back to "not yet set up". Disabled is reversible, because it is not a purge.
func (s UserState) CanTransitionTo(next UserState) bool {
	if s == next || !next.Valid() {
		return false
	}

	switch s {
	case UserInvited:
		return next == UserActive || next == UserDisabled
	case UserActive:
		return next == UserDisabled
	case UserDisabled:
		return next == UserActive
	default:
		return false
	}
}

// TransitionTo returns the next state, or refuses the move. An unrecognised
// current state fails closed rather than being read as the nearest known one.
func (s UserState) TransitionTo(next UserState) (UserState, error) {
	if !s.Valid() {
		return "", fmt.Errorf("%w: %q", ErrUnknownState, string(s))
	}

	if !s.CanTransitionTo(next) {
		return "", fmt.Errorf("%w: %s to %s", ErrInvalidTransition, s, next)
	}

	return next, nil
}

// PasswordHash is an opaque credential hash. Its zero value means no credential
// has ever been set, which only an invited identity may carry.
type PasswordHash string

// String redacts, mirroring ClaimToken, so a hash cannot reach a log line
// through ordinary formatting.
func (h PasswordHash) String() string {
	if h == "" {
		return ""
	}

	return "[redacted password hash]"
}

// GoString redacts too, because %#v prints the value rather than String and
// would otherwise spell the hash out in a debug line.
func (h PasswordHash) GoString() string {
	return `"` + h.String() + `"`
}

// MarshalJSON redacts, so a hash cannot reach a JSON log handler or a response
// body through a struct that merely happens to carry one.
func (h PasswordHash) MarshalJSON() ([]byte, error) {
	return []byte(`"` + h.String() + `"`), nil
}

// isSet reports whether a credential has been set. A real hash is never empty.
func (h PasswordHash) isSet() bool {
	return h != ""
}

// Octet limits from RFC 5321 section 4.5.3.1. They bound the normalised form,
// which is both what the unique index compares and what goes on the wire.
const (
	maxLocalOctets   = 64
	maxDomainOctets  = 255
	maxAddressOctets = 254
	maxLabelOctets   = 63
)

// localAtomSpecials is the RFC 5322 dot-atom punctuation, the only characters
// besides letters, digits and the separating dot a local part may hold.
const localAtomSpecials = "!#$%&'*+-/=?^_`{|}~"

// emailDomain is the one profile every domain passes through, ASCII or not, so
// an ASCII lookalike can never skip the validation a Unicode domain gets.
var emailDomain = idna.New(
	idna.MapForLookup(),
	idna.BidiRule(),
	idna.Transitional(false),
	idna.VerifyDNSLength(true),
)

// Email is a validated address: the value the realm-scoped unique index
// compares, and the spelling a person should see. NormaliseEmail is the only
// code path that produces one, so the two forms can never disagree.
type Email struct {
	normalized string
	display    string
}

// NormaliseEmail validates raw input and computes both stored forms. Where it
// cannot be sure two spellings name the same mailbox it keeps them apart: a
// duplicate account is a nuisance, a collapsed one is a cross-identity leak.
func NormaliseEmail(raw string) (Email, error) {
	address := strings.TrimSpace(raw)
	if address == "" {
		return Email{}, fmt.Errorf("%w: empty address", ErrInvalidEmail)
	}

	if err := rejectControlAndSpace(address); err != nil {
		return Email{}, err
	}

	at := strings.IndexByte(address, '@')
	if at < 0 || at != strings.LastIndexByte(address, '@') {
		return Email{}, fmt.Errorf("%w: an address holds exactly one @", ErrInvalidEmail)
	}

	local, err := normaliseLocalPart(address[:at])
	if err != nil {
		return Email{}, err
	}

	domain, ascii, err := normaliseDomain(address[at+1:])
	if err != nil {
		return Email{}, err
	}

	normalized := local + "@" + ascii
	if len(normalized) > maxAddressOctets {
		return Email{}, fmt.Errorf("%w: address exceeds %d octets", ErrInvalidEmail, maxAddressOctets)
	}

	return Email{normalized: normalized, display: address[:at] + "@" + domain}, nil
}

// rejectControlAndSpace refuses a control or whitespace codepoint anywhere in
// the trimmed address. Interior spacing is never repaired: stripping it could
// fold a quoted local part onto an unrelated unquoted one.
func rejectControlAndSpace(address string) error {
	for _, r := range address {
		if r < 0x20 || r == 0x7F || unicode.IsSpace(r) {
			return fmt.Errorf("%w: control or whitespace character", ErrInvalidEmail)
		}
	}

	return nil
}

// normaliseLocalPart lowercases pure ASCII dot-atom input. A plus tag and an
// interior dot survive untouched: which of them names a distinct mailbox is
// provider-specific, and this server cannot know it from the string alone.
func normaliseLocalPart(local string) (string, error) {
	if local == "" {
		return "", fmt.Errorf("%w: empty local part", ErrInvalidEmail)
	}

	if len(local) > maxLocalOctets {
		return "", fmt.Errorf("%w: local part exceeds %d octets", ErrInvalidEmail, maxLocalOctets)
	}

	if strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") || strings.Contains(local, "..") {
		return "", fmt.Errorf("%w: misplaced dot in local part", ErrInvalidEmail)
	}

	lowered := make([]rune, 0, len(local))
	for _, char := range local {
		if !isLocalAtomRune(char) {
			return "", fmt.Errorf("%w: %q is not a dot-atom character", ErrInvalidEmail, char)
		}

		if char >= 'A' && char <= 'Z' {
			char += 'a' - 'A'
		}

		lowered = append(lowered, char)
	}

	return string(lowered), nil
}

// isLocalAtomRune reports whether the rune is dot-atom content. The set is pure
// ASCII, which is what makes plain case folding safe for the local part and
// keeps every compatibility-folding question out of it.
func isLocalAtomRune(char rune) bool {
	switch {
	case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z':
		return true
	case char >= '0' && char <= '9', char == '.':
		return true
	default:
		return strings.ContainsRune(localAtomSpecials, char)
	}
}

// normaliseDomain drops the DNS root dot, checks the label shape and encodes
// the domain through IDNA. It returns the spelling as entered and the ASCII
// form the unique index compares.
func normaliseDomain(raw string) (string, string, error) {
	domain := strings.TrimSuffix(raw, ".")
	if domain == "" {
		return "", "", fmt.Errorf("%w: empty domain", ErrInvalidEmail)
	}

	if len(domain) > maxDomainOctets {
		return "", "", fmt.Errorf("%w: domain exceeds %d octets", ErrInvalidEmail, maxDomainOctets)
	}

	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return "", "", fmt.Errorf("%w: %q names no parent domain", ErrInvalidEmail, domain)
	}

	for _, label := range labels {
		if label == "" || len(label) > maxLabelOctets {
			return "", "", fmt.Errorf("%w: malformed domain label", ErrInvalidEmail)
		}
	}

	ascii, err := emailDomain.ToASCII(domain)
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", ErrInvalidEmail, err)
	}

	return domain, ascii, nil
}

// Normalized is the exact value the realm-scoped unique index compares.
func (e Email) Normalized() string {
	return e.normalized
}

// Display is the address as entered, minus surrounding whitespace and a bare
// trailing domain dot. It is never case-folded, so a person always sees their
// own address spelled the way they typed it.
func (e Email) Display() string {
	return e.display
}

// IsZero reports whether the value names no address.
func (e Email) IsZero() bool {
	return e.normalized == ""
}

// User is one identity: server-scoped, administering the install and owning no
// Project, or project-scoped, belonging to exactly one. Every field is
// unexported, so the two constructors stay the only way a value is built.
type User struct {
	id           UserID
	scope        UserScope
	homeProject  ID
	email        Email
	passwordHash PasswordHash
	state        UserState
	mfaRequired  bool
}

// UserConfig is what both constructors take, and the shape a store rehydrates a
// persisted row through. It names neither scope nor home Project: those come
// from the constructor, so a caller cannot pair them wrongly.
type UserConfig struct {
	ID           UserID
	Email        Email
	PasswordHash PasswordHash
	State        UserState
	MFARequired  bool
}

// NewServerUser builds an identity that owns no Project and is unique within
// the system realm. It accepts no Project at all, so it cannot be given one.
func NewServerUser(cfg UserConfig) (User, error) {
	return newUser(ScopeServer, "", cfg)
}

// NewProjectUser builds an identity anchored to exactly one Project. The home
// Project is an argument and not a field, so a project-scoped identity without
// one is not expressible.
func NewProjectUser(home ID, cfg UserConfig) (User, error) {
	if err := ValidateID(home); err != nil {
		return User{}, err
	}

	return newUser(ScopeProject, home, cfg)
}

// newUser applies the invariants the users table cannot: an email only
// NormaliseEmail can have produced, and a state that agrees with the credential.
func newUser(scope UserScope, home ID, cfg UserConfig) (User, error) {
	if cfg.ID == "" {
		return User{}, fmt.Errorf("%w: user", ErrMissingID)
	}

	if cfg.Email.IsZero() {
		return User{}, fmt.Errorf("%w: %s", ErrMissingEmail, cfg.ID)
	}

	if !cfg.State.Valid() {
		return User{}, fmt.Errorf("%w: %q", ErrUnknownState, string(cfg.State))
	}

	// The schema accepts any state beside any credential. This is the one place
	// the pairing is checked: an invitation has never set one, an active identity
	// cannot authenticate without one, and a revoked invitation still holds none.
	credentialled := cfg.PasswordHash.isSet()
	if (cfg.State == UserInvited && credentialled) || (cfg.State == UserActive && !credentialled) {
		return User{}, fmt.Errorf("%w: %s is %s", ErrInvalidCredentialState, cfg.ID, cfg.State)
	}

	return User{
		id: cfg.ID, scope: scope, homeProject: home,
		email: cfg.Email, passwordHash: cfg.PasswordHash,
		state: cfg.State, mfaRequired: cfg.MFARequired,
	}, nil
}

// String renders an identity without its credential. fmt cannot call String on
// an unexported field, so the hash would otherwise be spelled out by %v on a
// User however carefully PasswordHash redacts itself.
func (u User) String() string {
	return fmt.Sprintf("user %s in %s (%s, credentialled %t)",
		u.id, u.IdentityRealm(), u.state, u.passwordHash.isSet())
}

// GoString redacts too, because %#v reaches the unexported field directly and
// prints what it finds there.
func (u User) GoString() string {
	return u.String()
}

// ID returns the identity's identifier.
func (u User) ID() UserID {
	return u.id
}

// Scope returns whether this identity is anchored to a Project.
func (u User) Scope() UserScope {
	return u.scope
}

// HomeProject returns the Project this identity belongs to, if any. A
// server-scoped identity reports none, never the empty id as a usable Project.
func (u User) HomeProject() (ID, bool) {
	return u.homeProject, u.scope == ScopeProject
}

// IdentityRealm returns the scope this identity's email is unique within,
// computed the way the generated column is.
func (u User) IdentityRealm() IdentityRealm {
	return DeriveRealm(u.homeProject)
}

// Email returns the identity's validated address.
func (u User) Email() Email {
	return u.email
}

// HasCredential reports whether a password has ever been set. The hash itself
// is never handed out: whether one exists is the only domain question.
func (u User) HasCredential() bool {
	return u.passwordHash.isSet()
}

// State returns the identity's lifecycle position.
func (u User) State() UserState {
	return u.state
}

// MFARequired reports whether this identity's policy demands a second factor.
// It is independent of the lifecycle and survives every transition unchanged.
func (u User) MFARequired() bool {
	return u.mfaRequired
}

// AcceptInvitation returns the identity active and credentialled. It is the one
// way out of invited into active, and it sets the credential in the same move,
// so no value exists that is active without one.
func (u User) AcceptInvitation(hash PasswordHash) (User, error) {
	if u.state != UserInvited {
		return User{}, fmt.Errorf("%w: %s to %s", ErrInvalidTransition, u.state, UserActive)
	}

	if !hash.isSet() {
		return User{}, fmt.Errorf("%w: %s accepted with no credential", ErrInvalidCredentialState, u.id)
	}

	u.state, u.passwordHash = UserActive, hash

	return u, nil
}

// TransitionTo returns the identity in its next state. Activating an invitation
// is refused here because that move sets a credential: it travels through
// AcceptInvitation or not at all.
func (u User) TransitionTo(next UserState) (User, error) {
	if u.state == UserInvited && next == UserActive {
		return User{}, fmt.Errorf("%w: %s activates by accepting its invitation", ErrInvalidTransition, u.id)
	}

	state, err := u.state.TransitionTo(next)
	if err != nil {
		return User{}, err
	}

	u.state = state

	return u, nil
}
