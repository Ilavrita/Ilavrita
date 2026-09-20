package project

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"
)

// authorizationCodePrefix keeps a code's identifier out of every other namespace.
const authorizationCodePrefix = "acd_"

// authorizationCodeBytes is the entropy a code carries, the same 32 bytes a
// session token draws.
const authorizationCodeBytes = 32

// maxAuthorizationCodeLifetime is how long a code may live.
//
// A code is redeemed by the client that just received it, on the round trip it
// is already making. Sixty seconds is generous for that and short enough that a
// code read out of a proxy log or a browser history is almost always already
// dead. RFC 6749 recommends a maximum of ten minutes; this is shorter because
// nothing here needs the other nine.
const maxAuthorizationCodeLifetime = 60 * time.Second

// The PKCE verifier length RFC 7636 fixes. A shorter one is guessable and a
// longer one is a client sending something that is not a verifier.
const (
	minCodeVerifier = 43
	maxCodeVerifier = 128
)

// AuthorizationCodeID identifies one issued code.
type AuthorizationCodeID string

// MintAuthorizationCodeID draws a code identifier in its own namespace.
func MintAuthorizationCodeID(random io.Reader) (AuthorizationCodeID, error) {
	id, err := mintServiceID(random, authorizationCodePrefix)

	return AuthorizationCodeID(id), err
}

// AuthorizationCodeToken is what a client carries back to the token endpoint.
//
// It redacts for the same reason a session token does: it is a bearer credential
// for the seconds it lives, and a credential that can reach a log line through
// ordinary formatting is one that outlives its own lifetime.
type AuthorizationCodeToken struct {
	raw string
}

// Reveal returns the code in the clear, for the one handover in the redirect.
func (t AuthorizationCodeToken) Reveal() string { return t.raw }

// String redacts.
func (t AuthorizationCodeToken) String() string {
	if t.raw == "" {
		return ""
	}

	return "[redacted authorization code]"
}

// GoString redacts too, because %#v prints the value rather than String.
func (t AuthorizationCodeToken) GoString() string { return `"` + t.String() + `"` }

// MarshalJSON redacts, so a code cannot reach a JSON log handler through a
// struct that merely happens to carry one.
func (t AuthorizationCodeToken) MarshalJSON() ([]byte, error) {
	return []byte(`"` + t.String() + `"`), nil
}

// IsZero reports whether this value carries a code at all.
func (t AuthorizationCodeToken) IsZero() bool { return t.raw == "" }

// Digest is what the row keeps, and what a redemption compares against.
func (t AuthorizationCodeToken) Digest() string {
	if t.raw == "" {
		return ""
	}

	sum := sha256.Sum256([]byte(t.raw))

	return hex.EncodeToString(sum[:])
}

// ParseAuthorizationCode reads a code a client presented. It is the one way an
// outside value becomes one, and it confers nothing by itself.
func ParseAuthorizationCode(raw string) (AuthorizationCodeToken, error) {
	if raw == "" {
		return AuthorizationCodeToken{}, fmt.Errorf("%w: no code was presented", ErrInvalidAuthorizationCode)
	}

	return AuthorizationCodeToken{raw: raw}, nil
}

// mintAuthorizationCode draws a code from the reader. A short or failed read
// mints nothing rather than a code an attacker could predict.
func mintAuthorizationCode(random io.Reader) (AuthorizationCodeToken, error) {
	raw := make([]byte, authorizationCodeBytes)
	if _, err := io.ReadFull(random, raw); err != nil {
		return AuthorizationCodeToken{}, fmt.Errorf("project: cannot mint an authorization code: %w", err)
	}

	return AuthorizationCodeToken{raw: base64.RawURLEncoding.EncodeToString(raw)}, nil
}

// CodeChallenge is the PKCE challenge a client bound its authorization to.
//
// Only S256 is representable. RFC 7636 also defines "plain", where the challenge
// is the verifier — which protects against nothing, because anybody who
// intercepted the code intercepted the challenge with it. It exists for devices
// that cannot SHA-256, and a client that cannot SHA-256 cannot do TLS either.
type CodeChallenge struct {
	value string
}

// ParseCodeChallenge reads the challenge and the method a client stated.
func ParseCodeChallenge(value, method string) (CodeChallenge, error) {
	if method != "S256" {
		return CodeChallenge{}, fmt.Errorf(
			"%w: %q is not a challenge method this server accepts", ErrInvalidCodeChallenge, method)
	}

	value = strings.TrimSpace(value)

	// A challenge is the base64url of a SHA-256, so its length is not a matter
	// of taste: anything else is not one.
	if len(value) != base64.RawURLEncoding.EncodedLen(sha256.Size) {
		return CodeChallenge{}, fmt.Errorf(
			"%w: a challenge is %d characters, not %d",
			ErrInvalidCodeChallenge, base64.RawURLEncoding.EncodedLen(sha256.Size), len(value))
	}

	if _, err := base64.RawURLEncoding.DecodeString(value); err != nil {
		return CodeChallenge{}, fmt.Errorf("%w: %w", ErrInvalidCodeChallenge, err)
	}

	return CodeChallenge{value: value}, nil
}

// Value returns the challenge as stated, for a row being written.
func (c CodeChallenge) Value() string { return c.value }

// IsZero reports whether a challenge was stated at all.
func (c CodeChallenge) IsZero() bool { return c.value == "" }

// Verifies reports whether a verifier is the one this challenge was derived
// from.
//
// The zero challenge verifies nothing, which matters because the zero value is
// what a row missing its challenge would rebuild as. The comparison below
// already refuses it — an empty value and a 43-character digest differ in
// length, and ConstantTimeCompare answers zero for that — so the guard states
// the intent rather than carrying it, and would keep holding if the comparison
// were ever changed to one that did not.
func (c CodeChallenge) Verifies(verifier string) bool {
	if c.value == "" {
		return false
	}

	if len(verifier) < minCodeVerifier || len(verifier) > maxCodeVerifier {
		return false
	}

	if !unreservedVerifier(verifier) {
		return false
	}

	sum := sha256.Sum256([]byte(verifier))
	derived := base64.RawURLEncoding.EncodeToString(sum[:])

	return subtle.ConstantTimeCompare([]byte(c.value), []byte(derived)) == 1
}

// unreservedVerifier reports whether every character is one RFC 7636 permits.
// A verifier outside that set is one some hop will re-encode, and a re-encoded
// verifier hashes to something else.
func unreservedVerifier(verifier string) bool {
	for _, held := range verifier {
		switch {
		case held >= 'A' && held <= 'Z',
			held >= 'a' && held <= 'z',
			held >= '0' && held <= '9',
			held == '-', held == '.', held == '_', held == '~':
		default:
			return false
		}
	}

	return true
}

// AuthorizationCode is one person's approval, waiting to be redeemed once.
//
// Everything the token endpoint must re-check is bound here rather than re-read
// from the request that redeems it: the client it was issued to, the address it
// is returned at, the challenge it was bound to, and what was approved. A code
// that carried less would be a code the token endpoint had to trust its
// presenter about.
type AuthorizationCode struct {
	id         AuthorizationCodeID
	project    ID
	client     ClientApplicationID
	user       UserID
	membership MembershipID
	redirect   RedirectURI
	challenge  CodeChallenge
	launch     LaunchContext
	digest     string
	createdAt  time.Time
	expiresAt  time.Time
}

// AuthorizationCodeRecord is the shape a store rehydrates a persisted row
// through.
type AuthorizationCodeRecord struct {
	ID            AuthorizationCodeID
	Client        ClientApplicationID
	User          UserID
	Membership    MembershipID
	RedirectURI   string
	CodeChallenge string
	LaunchPatient string
	GrantedScopes string
	Digest        string
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

// IssueAuthorizationCode mints one code for an approval that has just been
// given, and returns the token beside it.
//
// The launch context is required, because a code granting nothing is a code that
// would mint a session narrowed by nothing. It is the same value the session
// will carry, decided here — at the moment the person approved — rather than at
// redemption, when only the client is present.
func IssueAuthorizationCode(
	owner ID, id AuthorizationCodeID, client ClientApplicationID,
	user UserID, membership MembershipID, redirect RedirectURI,
	challenge CodeChallenge, launch LaunchContext,
	issuedAt time.Time, lifetime time.Duration, random io.Reader,
) (AuthorizationCode, AuthorizationCodeToken, error) {
	if err := ValidateID(owner); err != nil {
		return AuthorizationCode{}, AuthorizationCodeToken{}, err
	}

	if err := validateServiceID(string(id), authorizationCodePrefix); err != nil {
		return AuthorizationCode{}, AuthorizationCodeToken{}, err
	}

	if err := ValidateClientApplicationID(client); err != nil {
		return AuthorizationCode{}, AuthorizationCodeToken{}, err
	}

	if user == "" || membership == "" {
		return AuthorizationCode{}, AuthorizationCodeToken{}, fmt.Errorf(
			"%w: a code names an identity and a membership", ErrMissingID)
	}

	if redirect == "" {
		return AuthorizationCode{}, AuthorizationCodeToken{}, fmt.Errorf(
			"%w: a code names the address it is returned at", ErrInvalidRedirectURI)
	}

	// PKCE is required of every client, so a code with no challenge is one
	// nothing binds to the client that asked for it.
	if challenge.IsZero() {
		return AuthorizationCode{}, AuthorizationCodeToken{}, fmt.Errorf(
			"%w: a code is bound to a challenge", ErrInvalidCodeChallenge)
	}

	if launch.IsZero() {
		return AuthorizationCode{}, AuthorizationCodeToken{}, fmt.Errorf(
			"%w: a code states what was approved", ErrInvalidLaunch)
	}

	if issuedAt.IsZero() {
		return AuthorizationCode{}, AuthorizationCodeToken{}, fmt.Errorf(
			"%w: issued at no instant", ErrInvalidAuthorizationCode)
	}

	if lifetime <= 0 || lifetime > maxAuthorizationCodeLifetime {
		return AuthorizationCode{}, AuthorizationCodeToken{}, fmt.Errorf(
			"%w: %s is outside the permitted lifetime", ErrInvalidAuthorizationCode, lifetime)
	}

	token, err := mintAuthorizationCode(random)
	if err != nil {
		return AuthorizationCode{}, AuthorizationCodeToken{}, err
	}

	return AuthorizationCode{
		id: id, project: owner, client: client, user: user, membership: membership,
		redirect: redirect, challenge: challenge, launch: launch,
		digest:    token.Digest(),
		createdAt: issuedAt.UTC(), expiresAt: issuedAt.Add(lifetime).UTC(),
	}, token, nil
}

// NewAuthorizationCode rebuilds a persisted code, refusing every row the table
// refuses.
func NewAuthorizationCode(owner ID, rec AuthorizationCodeRecord) (AuthorizationCode, error) {
	if err := ValidateID(owner); err != nil {
		return AuthorizationCode{}, err
	}

	if err := validateServiceID(string(rec.ID), authorizationCodePrefix); err != nil {
		return AuthorizationCode{}, err
	}

	if rec.Digest == "" {
		return AuthorizationCode{}, fmt.Errorf(
			"%w: %s holds nothing to compare against", ErrInvalidAuthorizationCode, rec.ID)
	}

	if !rec.ExpiresAt.After(rec.CreatedAt) ||
		rec.ExpiresAt.Sub(rec.CreatedAt) > maxAuthorizationCodeLifetime {
		return AuthorizationCode{}, fmt.Errorf(
			"%w: %s is outside the permitted lifetime", ErrInvalidAuthorizationCode, rec.ID)
	}

	// Rebuilt through the same constructors a fresh code goes through, so a row
	// nothing could have written is refused rather than redeemed.
	challenge, err := ParseCodeChallenge(rec.CodeChallenge, "S256")
	if err != nil {
		return AuthorizationCode{}, fmt.Errorf("%s: %w", rec.ID, err)
	}

	redirect, err := ParseRedirectURI(rec.RedirectURI)
	if err != nil {
		return AuthorizationCode{}, fmt.Errorf("%s: %w", rec.ID, err)
	}

	launch, err := NewLaunchContext(rec.LaunchPatient, rec.GrantedScopes)
	if err != nil {
		return AuthorizationCode{}, fmt.Errorf("%s: %w", rec.ID, err)
	}

	return AuthorizationCode{
		id: rec.ID, project: owner, client: rec.Client, user: rec.User,
		membership: rec.Membership, redirect: redirect, challenge: challenge,
		launch: launch, digest: rec.Digest,
		createdAt: rec.CreatedAt.UTC(), expiresAt: rec.ExpiresAt.UTC(),
	}, nil
}

// ID returns the code's identifier.
func (c AuthorizationCode) ID() AuthorizationCodeID { return c.id }

// Project returns the Project the approval was given in.
func (c AuthorizationCode) Project() ID { return c.project }

// Client returns the registration the code was issued to.
func (c AuthorizationCode) Client() ClientApplicationID { return c.client }

// User returns the identity that approved.
func (c AuthorizationCode) User() UserID { return c.user }

// Membership returns the standing pinned at approval.
func (c AuthorizationCode) Membership() MembershipID { return c.membership }

// RedirectURI returns the address the code was returned at.
func (c AuthorizationCode) RedirectURI() RedirectURI { return c.redirect }

// Challenge returns the PKCE challenge the code is bound to.
func (c AuthorizationCode) Challenge() CodeChallenge { return c.challenge }

// Launch returns what was approved: the scopes, and the patient it was approved
// for.
func (c AuthorizationCode) Launch() LaunchContext { return c.launch }

// Digest returns the material the insert binds.
func (c AuthorizationCode) Digest() string { return c.digest }

// CreatedAt returns the instant the approval was given.
func (c AuthorizationCode) CreatedAt() time.Time { return c.createdAt }

// ExpiresAt returns the instant the code stops answering.
func (c AuthorizationCode) ExpiresAt() time.Time { return c.expiresAt }

// Redeemable reports whether this code may be exchanged, by the caller
// presenting it, at this instant.
//
// Every check travels together, so no caller can compare the code and forget the
// clock, or check the clock and forget which client asked. A redemption that got
// any one of them wrong would be a code exchanged by somebody it was not issued
// to, and there is no safe order in which to check them separately.
func (c AuthorizationCode) Redeemable(
	token AuthorizationCodeToken, client ClientApplicationID,
	redirect, verifier string, now time.Time,
) bool {
	if c.digest == "" || token.IsZero() || !now.Before(c.expiresAt) {
		return false
	}

	if c.client != client {
		return false
	}

	// The address is compared as the string it was registered and presented as,
	// for the same reason RedirectURIs.Allows is exact.
	if string(c.redirect) != strings.TrimSpace(redirect) {
		return false
	}

	if !c.challenge.Verifies(verifier) {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(c.digest), []byte(token.Digest())) == 1
}

// String renders a code without its material.
func (c AuthorizationCode) String() string {
	return fmt.Sprintf("authorization code %s for %s to %s in %s (expires %s, %s)",
		c.id, c.user, c.client, c.project, c.expiresAt.Format(time.RFC3339), c.launch)
}

// GoString redacts too, because %#v reaches the unexported fields directly.
func (c AuthorizationCode) GoString() string { return c.String() }
