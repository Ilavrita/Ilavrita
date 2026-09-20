package project

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"time"
)

// sessionPrefix keeps a session id out of every other identifier namespace.
const sessionPrefix = "ses_"

// sessionTokenBytes is the entropy a session token carries. It is the same 32
// bytes a client secret draws, and for the same reason: the single SHA-256 below
// leaves a guesser nothing to shorten.
const sessionTokenBytes = 32

// maxSessionLifetime is how long a session may live. A session that outlives the
// day it was issued on is one nobody re-proved a credential for, and the schema
// states the same ceiling so neither side can drift.
const maxSessionLifetime = 12 * time.Hour

// SessionID identifies one issued session inside one Project.
type SessionID string

// SessionToken is the bearer a caller presents on a later request. Its field is
// unexported and only this file sets it, so a token a caller invented carries
// nothing: what it names is decided by the row its digest finds, or by nothing.
type SessionToken struct {
	raw string
}

// Reveal returns the token in the clear, for the one handover at login.
func (t SessionToken) Reveal() string {
	return t.raw
}

// String redacts, so a token cannot reach a log line through ordinary formatting
// and Reveal stays the only way to read one.
func (t SessionToken) String() string {
	if t.raw == "" {
		return ""
	}

	return "[redacted session token]"
}

// GoString redacts too, because %#v prints the value rather than String.
func (t SessionToken) GoString() string {
	return `"` + t.String() + `"`
}

// MarshalJSON redacts, so a token cannot reach a JSON log handler through a
// struct that merely happens to carry one.
func (t SessionToken) MarshalJSON() ([]byte, error) {
	return []byte(`"` + t.String() + `"`), nil
}

// IsZero reports whether this value carries a token at all.
func (t SessionToken) IsZero() bool {
	return t.raw == ""
}

// Digest is what the session row keeps, and what a lookup compares against. A
// single SHA-256 is enough because a minted token is 32 bytes of entropy.
func (t SessionToken) Digest() string {
	if t.raw == "" {
		return ""
	}

	sum := sha256.Sum256([]byte(t.raw))

	return hex.EncodeToString(sum[:])
}

// ParseSessionToken reads a token a caller presented. It is the one way an
// outside value becomes a SessionToken, and it confers nothing by itself.
func ParseSessionToken(raw string) (SessionToken, error) {
	if raw == "" {
		return SessionToken{}, fmt.Errorf("%w: a request presents no session token", ErrInvalidSession)
	}

	return SessionToken{raw: raw}, nil
}

// mintSessionToken draws a token from the reader. A short or failed read mints
// nothing rather than a token an attacker could predict.
func mintSessionToken(random io.Reader) (SessionToken, error) {
	raw := make([]byte, sessionTokenBytes)
	if _, err := io.ReadFull(random, raw); err != nil {
		return SessionToken{}, fmt.Errorf("project: cannot mint a session token: %w", err)
	}

	return SessionToken{raw: base64.RawURLEncoding.EncodeToString(raw)}, nil
}

// MintSessionID draws a session identifier in its own namespace.
func MintSessionID(random io.Reader) (SessionID, error) {
	id, err := mintServiceID(random, sessionPrefix)

	return SessionID(id), err
}

// SessionState is an issued session's position in its life. There is no expired
// state: an expiry passes without anyone writing anything, which is what makes it
// hold when nothing is watching.
type SessionState string

// The lifecycle states of a session.
const (
	SessionActive  SessionState = "active"
	SessionRevoked SessionState = "revoked"
)

// Valid reports whether the state is one this server recognises.
func (s SessionState) Valid() bool {
	return s == SessionActive || s == SessionRevoked
}

// Session is one authenticated identity's standing to make later requests. It
// pins the Project and the membership at issuance, so a request never has to
// resolve which membership was meant.
type Session struct {
	id         SessionID
	project    ID
	user       UserID
	membership MembershipID
	digest     string
	state      SessionState
	launch     LaunchContext
	createdAt  time.Time
	expiresAt  time.Time
	revokedAt  time.Time
}

// SessionRecord is the shape a store rehydrates a persisted row through.
type SessionRecord struct {
	ID         SessionID
	User       UserID
	Membership MembershipID
	Digest     string
	State      SessionState

	// LaunchPatient and GrantedScopes are what an app's session was launched
	// with, both empty for an ordinary login. They travel in the session's own
	// row rather than a table beside it, so a session an app holds cannot be
	// read without reading what that app was granted.
	LaunchPatient string
	GrantedScopes string

	CreatedAt time.Time
	ExpiresAt time.Time
	RevokedAt time.Time
}

// IssueSession mints one session for an authenticated identity and returns the
// token beside it. The membership is an argument rather than something resolved
// later, because the instant a credential was presented is the only honest moment
// to decide which standing a token carries (FR-048).
func IssueSession(
	owner ID, id SessionID, user UserID, membership MembershipID,
	issuedAt time.Time, lifetime time.Duration, random io.Reader,
) (Session, SessionToken, error) {
	return issueSession(owner, id, user, membership, LaunchContext{}, issuedAt, lifetime, random)
}

// IssueAppSession mints one session for an app acting for an authenticated
// identity. It takes the launch context rather than accepting one afterwards,
// because a session that is an app's and does not say what the app was granted
// is one nothing narrows: it would reach everything the person reaches, which is
// the whole of what a SMART scope exists to prevent.
func IssueAppSession(
	owner ID, id SessionID, user UserID, membership MembershipID, launch LaunchContext,
	issuedAt time.Time, lifetime time.Duration, random io.Reader,
) (Session, SessionToken, error) {
	if launch.IsZero() {
		return Session{}, SessionToken{}, fmt.Errorf(
			"%w: an app's session states what the app was granted", ErrInvalidLaunch)
	}

	return issueSession(owner, id, user, membership, launch, issuedAt, lifetime, random)
}

// issueSession is what both minting paths come to, so neither can validate less
// than the other.
func issueSession(
	owner ID, id SessionID, user UserID, membership MembershipID, launch LaunchContext,
	issuedAt time.Time, lifetime time.Duration, random io.Reader,
) (Session, SessionToken, error) {
	if err := ValidateID(owner); err != nil {
		return Session{}, SessionToken{}, err
	}

	if err := validateServiceID(string(id), sessionPrefix); err != nil {
		return Session{}, SessionToken{}, err
	}

	if user == "" || membership == "" {
		return Session{}, SessionToken{}, fmt.Errorf("%w: a session names an identity and a membership", ErrMissingID)
	}

	if issuedAt.IsZero() {
		return Session{}, SessionToken{}, fmt.Errorf("%w: issued at no instant", ErrInvalidSession)
	}

	if lifetime <= 0 || lifetime > maxSessionLifetime {
		return Session{}, SessionToken{}, fmt.Errorf(
			"%w: %s is outside the permitted lifetime", ErrInvalidSession, lifetime)
	}

	token, err := mintSessionToken(random)
	if err != nil {
		return Session{}, SessionToken{}, err
	}

	return Session{
		id: id, project: owner, user: user, membership: membership,
		digest: token.Digest(), state: SessionActive, launch: launch,
		createdAt: issuedAt.UTC(), expiresAt: issuedAt.Add(lifetime).UTC(),
	}, token, nil
}

// NewSession rebuilds a persisted session and refuses every row the table
// refuses: a live one holding no token, a revoked one still holding material, and
// a lifetime that never ends or ends before it began.
func NewSession(owner ID, rec SessionRecord) (Session, error) {
	if err := ValidateID(owner); err != nil {
		return Session{}, err
	}

	if err := validateServiceID(string(rec.ID), sessionPrefix); err != nil {
		return Session{}, err
	}

	if rec.User == "" || rec.Membership == "" {
		return Session{}, fmt.Errorf("%w: a session names an identity and a membership", ErrMissingID)
	}

	if !rec.State.Valid() {
		return Session{}, fmt.Errorf("%w: %q", ErrUnknownState, string(rec.State))
	}

	if err := rec.validateMaterial(); err != nil {
		return Session{}, err
	}

	launch, err := rec.launchContext()
	if err != nil {
		return Session{}, err
	}

	return Session{
		id: rec.ID, project: owner, user: rec.User, membership: rec.Membership,
		digest: rec.Digest, state: rec.State, launch: launch,
		createdAt: rec.CreatedAt.UTC(), expiresAt: rec.ExpiresAt.UTC(), revokedAt: rec.RevokedAt.UTC(),
	}, nil
}

// launchContext rebuilds what an app's session was launched with, through the
// same constructor a fresh one goes through, so a row nothing could have written
// is refused rather than served.
//
// A row naming a patient but granting nothing is the dangerous shape: it would
// rebuild as an ordinary login, which is narrowed by nothing, while looking like
// an app's session to anyone reading the table. It is refused.
func (r SessionRecord) launchContext() (LaunchContext, error) {
	if r.GrantedScopes == "" {
		if r.LaunchPatient != "" {
			return LaunchContext{}, fmt.Errorf(
				"%w: %s names a launch patient and no granted scopes", ErrInvalidLaunch, r.ID)
		}

		return LaunchContext{}, nil
	}

	launch, err := NewLaunchContext(r.LaunchPatient, r.GrantedScopes)
	if err != nil {
		return LaunchContext{}, fmt.Errorf("%s: %w", r.ID, err)
	}

	return launch, nil
}

// validateMaterial pairs the state against the token, which is what the row's two
// mirrored CHECKs state and no column states alone.
func (r SessionRecord) validateMaterial() error {
	if r.State == SessionRevoked {
		if r.Digest != "" || r.RevokedAt.IsZero() {
			return fmt.Errorf("%w: %s is revoked", ErrInvalidSession, r.ID)
		}

		return nil
	}

	if r.Digest == "" || !r.RevokedAt.IsZero() {
		return fmt.Errorf("%w: %s is %s", ErrInvalidSession, r.ID, r.State)
	}

	if !r.ExpiresAt.After(r.CreatedAt) || r.ExpiresAt.Sub(r.CreatedAt) > maxSessionLifetime {
		return fmt.Errorf("%w: %s is outside the permitted lifetime", ErrInvalidSession, r.ID)
	}

	return nil
}

// ID returns the session's identifier.
func (s Session) ID() SessionID {
	return s.id
}

// Project returns the Project this session's standing lives in.
func (s Session) Project() ID {
	return s.project
}

// User returns the identity that authenticated.
func (s Session) User() UserID {
	return s.user
}

// Membership returns the standing pinned at issuance.
func (s Session) Membership() MembershipID {
	return s.membership
}

// State returns the session's position in its life.
func (s Session) State() SessionState {
	return s.state
}

// CreatedAt returns the instant the session was issued.
func (s Session) CreatedAt() time.Time {
	return s.createdAt
}

// ExpiresAt returns the instant the session stops answering.
func (s Session) ExpiresAt() time.Time {
	return s.expiresAt
}

// Digest returns the material the insert binds.
func (s Session) Digest() string {
	return s.digest
}

// Launch returns what an app's session was launched with. The zero value means
// an ordinary login, which no app asked to narrow and which therefore reaches
// exactly what the person's own standing reaches.
func (s Session) Launch() LaunchContext {
	return s.launch
}

// Principal names the identity a request carrying this session is served as.
func (s Session) Principal() PrincipalRef {
	return PrincipalRef{Kind: PrincipalUser, ID: PrincipalID(s.user)}
}

// Live reports whether the session authorizes anything at this instant. An
// expired session dies without anyone writing anything, which is what makes the
// ceiling hold when nothing is watching.
func (s Session) Live(now time.Time) bool {
	return s.state == SessionActive && s.digest != "" && now.Before(s.expiresAt)
}

// Matches reports whether this is the token on file and still live, compared in
// constant time. Liveness travels with the comparison, so no caller can check the
// token and forget the clock.
func (s Session) Matches(token SessionToken, now time.Time) bool {
	if !s.Live(now) || token.IsZero() {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(s.digest), []byte(token.Digest())) == 1
}

// Revoke returns the session with its material gone, so it matches nothing
// because it holds nothing.
func (s Session) Revoke(at time.Time) (Session, error) {
	if s.state == SessionRevoked {
		return Session{}, fmt.Errorf("%w: %s is already revoked", ErrInvalidSession, s.id)
	}

	if at.IsZero() {
		return Session{}, fmt.Errorf("%w: %s revoked at no instant", ErrInvalidSession, s.id)
	}

	s.state, s.digest, s.revokedAt = SessionRevoked, "", at.UTC()

	return s, nil
}

// String renders a session without its material. The launch context is named
// because whether a session is an app's decides what it reaches, and a log line
// that omitted it would read the same for a narrowed session and an open one.
func (s Session) String() string {
	return fmt.Sprintf("session %s for %s in %s (%s, expires %s, %s)",
		s.id, s.user, s.project, s.state, s.expiresAt.Format(time.RFC3339), s.launch)
}

// GoString redacts too, because %#v reaches the unexported fields directly.
func (s Session) GoString() string {
	return s.String()
}
