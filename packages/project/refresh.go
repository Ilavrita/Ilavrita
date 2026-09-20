package project

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// The namespaces a refresh token's own identifiers live in.
const (
	refreshTokenPrefix = "rft_"
	refreshChainPrefix = "rch_"
)

// refreshTokenBytes is the entropy a refresh token carries, the same 32 bytes a
// session token draws.
const refreshTokenBytes = 32

// maxRefreshLifetime is how long one grant may be refreshed for without the
// person being asked again.
//
// A refresh token is the longest-lived credential this server issues, so it is
// the one whose ceiling matters most. Thirty days is long enough that an app a
// person uses weekly stays signed in, and short enough that an abandoned one
// stops working inside a billing cycle.
const maxRefreshLifetime = 30 * 24 * time.Hour

// refreshScopes are the SMART scopes that ask for a refresh token at all. An
// authorization granting neither is one the person agreed to for this session,
// and issuing a refresh token for it would extend a grant nobody extended.
var refreshScopes = []string{"offline_access", "online_access"}

// RefreshRequested reports whether an approval asked to outlive its session.
func RefreshRequested(launch LaunchContext) bool {
	for _, one := range strings.Fields(launch.Scopes()) {
		if slices.Contains(refreshScopes, one) {
			return true
		}
	}

	return false
}

// RefreshChainID names one grant across every rotation of it.
//
// It is what makes a replay detectable: each rotation is a new token, and they
// are only recognisable as the same grant because they carry this.
type RefreshChainID string

// MintRefreshChainID draws a chain identifier.
func MintRefreshChainID(random io.Reader) (RefreshChainID, error) {
	id, err := mintServiceID(random, refreshChainPrefix)

	return RefreshChainID(id), err
}

// RefreshTokenID identifies one issued refresh token.
type RefreshTokenID string

// MintRefreshTokenID draws a refresh token identifier.
func MintRefreshTokenID(random io.Reader) (RefreshTokenID, error) {
	id, err := mintServiceID(random, refreshTokenPrefix)

	return RefreshTokenID(id), err
}

// RefreshToken is the bearer a client presents to be issued a new session.
type RefreshToken struct {
	raw string
}

// Reveal returns the token in the clear, for the one handover in a token
// response.
func (t RefreshToken) Reveal() string { return t.raw }

// String redacts, so a token cannot reach a log line through ordinary
// formatting.
func (t RefreshToken) String() string {
	if t.raw == "" {
		return ""
	}

	return "[redacted refresh token]"
}

// GoString redacts too, because %#v prints the value rather than String.
func (t RefreshToken) GoString() string { return `"` + t.String() + `"` }

// MarshalJSON redacts, so a token cannot reach a JSON log handler through a
// struct that merely happens to carry one.
func (t RefreshToken) MarshalJSON() ([]byte, error) {
	return []byte(`"` + t.String() + `"`), nil
}

// IsZero reports whether this value carries a token at all.
func (t RefreshToken) IsZero() bool { return t.raw == "" }

// Digest is what the row keeps, and what a redemption compares against.
func (t RefreshToken) Digest() string {
	if t.raw == "" {
		return ""
	}

	sum := sha256.Sum256([]byte(t.raw))

	return hex.EncodeToString(sum[:])
}

// ParseRefreshToken reads a token a client presented. It is the one way an
// outside value becomes one, and it confers nothing by itself.
func ParseRefreshToken(raw string) (RefreshToken, error) {
	if raw == "" {
		return RefreshToken{}, fmt.Errorf("%w: no refresh token was presented", ErrInvalidRefreshToken)
	}

	return RefreshToken{raw: raw}, nil
}

// mintRefreshToken draws a token from the reader.
func mintRefreshToken(random io.Reader) (RefreshToken, error) {
	raw := make([]byte, refreshTokenBytes)
	if _, err := io.ReadFull(random, raw); err != nil {
		return RefreshToken{}, fmt.Errorf("project: cannot mint a refresh token: %w", err)
	}

	return RefreshToken{raw: base64.RawURLEncoding.EncodeToString(raw)}, nil
}

// RefreshState is one token's position in its chain.
type RefreshState string

// The states a refresh token holds.
const (
	// RefreshActive is the one token of a chain that may currently be redeemed.
	RefreshActive RefreshState = "active"

	// RefreshSpent is a token that was redeemed. Its row is kept, because a
	// spent token presented again is the signal that somebody other than the
	// client is holding one — and a row that had been deleted would answer
	// "unknown", which is what a guess answers too.
	RefreshSpent RefreshState = "spent"
)

// Valid reports whether the state is one this server recognises.
func (s RefreshState) Valid() bool {
	return s == RefreshActive || s == RefreshSpent
}

// RefreshGrant is one issued refresh token: what it may be exchanged for, and
// which chain it belongs to.
type RefreshGrant struct {
	id         RefreshTokenID
	chain      RefreshChainID
	project    ID
	client     ClientApplicationID
	user       UserID
	membership MembershipID
	launch     LaunchContext
	state      RefreshState
	digest     string
	createdAt  time.Time
	expiresAt  time.Time
}

// RefreshRecord is the shape a store rehydrates a persisted row through.
type RefreshRecord struct {
	ID            RefreshTokenID
	Chain         RefreshChainID
	Client        ClientApplicationID
	User          UserID
	Membership    MembershipID
	LaunchPatient string
	GrantedScopes string
	State         RefreshState
	Digest        string
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

// IssueRefreshToken mints the first token of a new chain.
func IssueRefreshToken(
	owner ID, id RefreshTokenID, chain RefreshChainID, client ClientApplicationID,
	user UserID, membership MembershipID, launch LaunchContext,
	issuedAt time.Time, lifetime time.Duration, random io.Reader,
) (RefreshGrant, RefreshToken, error) {
	if err := ValidateID(owner); err != nil {
		return RefreshGrant{}, RefreshToken{}, err
	}

	if err := validateServiceID(string(id), refreshTokenPrefix); err != nil {
		return RefreshGrant{}, RefreshToken{}, err
	}

	if err := validateServiceID(string(chain), refreshChainPrefix); err != nil {
		return RefreshGrant{}, RefreshToken{}, err
	}

	if err := ValidateClientApplicationID(client); err != nil {
		return RefreshGrant{}, RefreshToken{}, err
	}

	if user == "" || membership == "" {
		return RefreshGrant{}, RefreshToken{}, fmt.Errorf(
			"%w: a refresh token names an identity and a membership", ErrMissingID)
	}

	// The launch context is what a refresh mints a session from. Without it
	// there would be nothing to narrow the new session by, and the refresh would
	// widen what the original approval granted.
	if launch.IsZero() {
		return RefreshGrant{}, RefreshToken{}, fmt.Errorf(
			"%w: a refresh token states what it may be exchanged for", ErrInvalidLaunch)
	}

	if issuedAt.IsZero() {
		return RefreshGrant{}, RefreshToken{}, fmt.Errorf(
			"%w: issued at no instant", ErrInvalidRefreshToken)
	}

	if lifetime <= 0 || lifetime > maxRefreshLifetime {
		return RefreshGrant{}, RefreshToken{}, fmt.Errorf(
			"%w: %s is outside the permitted lifetime", ErrInvalidRefreshToken, lifetime)
	}

	token, err := mintRefreshToken(random)
	if err != nil {
		return RefreshGrant{}, RefreshToken{}, err
	}

	return RefreshGrant{
		id: id, chain: chain, project: owner, client: client,
		user: user, membership: membership, launch: launch,
		state: RefreshActive, digest: token.Digest(),
		createdAt: issuedAt.UTC(), expiresAt: issuedAt.Add(lifetime).UTC(),
	}, token, nil
}

// NewRefreshGrant rebuilds a persisted token, refusing every row the table
// refuses.
func NewRefreshGrant(owner ID, rec RefreshRecord) (RefreshGrant, error) {
	if err := ValidateID(owner); err != nil {
		return RefreshGrant{}, err
	}

	if err := validateServiceID(string(rec.ID), refreshTokenPrefix); err != nil {
		return RefreshGrant{}, err
	}

	if err := validateServiceID(string(rec.Chain), refreshChainPrefix); err != nil {
		return RefreshGrant{}, err
	}

	if !rec.State.Valid() {
		return RefreshGrant{}, fmt.Errorf("%w: %q", ErrUnknownState, string(rec.State))
	}

	// A spent token keeps its digest, which is what makes a replay of it
	// recognisable rather than merely unknown.
	if rec.Digest == "" {
		return RefreshGrant{}, fmt.Errorf(
			"%w: %s holds nothing to compare against", ErrInvalidRefreshToken, rec.ID)
	}

	if !rec.ExpiresAt.After(rec.CreatedAt) ||
		rec.ExpiresAt.Sub(rec.CreatedAt) > maxRefreshLifetime {
		return RefreshGrant{}, fmt.Errorf(
			"%w: %s is outside the permitted lifetime", ErrInvalidRefreshToken, rec.ID)
	}

	launch, err := NewLaunchContext(rec.LaunchPatient, rec.GrantedScopes)
	if err != nil {
		return RefreshGrant{}, fmt.Errorf("%s: %w", rec.ID, err)
	}

	return RefreshGrant{
		id: rec.ID, chain: rec.Chain, project: owner, client: rec.Client,
		user: rec.User, membership: rec.Membership, launch: launch,
		state: rec.State, digest: rec.Digest,
		createdAt: rec.CreatedAt.UTC(), expiresAt: rec.ExpiresAt.UTC(),
	}, nil
}

// ID returns this token's identifier.
func (g RefreshGrant) ID() RefreshTokenID { return g.id }

// Chain returns the grant this token is one rotation of.
func (g RefreshGrant) Chain() RefreshChainID { return g.chain }

// Project returns the Project the grant lives in.
func (g RefreshGrant) Project() ID { return g.project }

// Client returns the registration the grant was issued to.
func (g RefreshGrant) Client() ClientApplicationID { return g.client }

// User returns the identity that approved.
func (g RefreshGrant) User() UserID { return g.user }

// Membership returns the standing pinned at approval.
func (g RefreshGrant) Membership() MembershipID { return g.membership }

// Launch returns what the grant may be exchanged for.
func (g RefreshGrant) Launch() LaunchContext { return g.launch }

// State returns whether this token is the live one or a spent rotation.
func (g RefreshGrant) State() RefreshState { return g.state }

// Digest returns the material the insert binds.
func (g RefreshGrant) Digest() string { return g.digest }

// CreatedAt returns the instant this rotation was issued.
func (g RefreshGrant) CreatedAt() time.Time { return g.createdAt }

// ExpiresAt returns the instant the grant stops answering.
func (g RefreshGrant) ExpiresAt() time.Time { return g.expiresAt }

// Spent reports whether this token was already redeemed.
//
// It is asked separately from Redeemable because the two demand opposite
// responses: a token nobody recognises is a guess and is merely refused, while a
// token this server issued and already spent is a copy somebody else is holding,
// and the whole chain must die.
func (g RefreshGrant) Spent() bool { return g.state == RefreshSpent }

// Redeemable reports whether this token may be exchanged, by the client
// presenting it, at this instant.
//
// The client is compared here for the same reason an authorization code compares
// it: a refresh token redeemed by another registration is a stolen one.
func (g RefreshGrant) Redeemable(
	token RefreshToken, client ClientApplicationID, now time.Time,
) bool {
	if g.state != RefreshActive || g.digest == "" || token.IsZero() {
		return false
	}

	if !now.Before(g.expiresAt) {
		return false
	}

	if g.client != client {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(g.digest), []byte(token.Digest())) == 1
}

// Rotate returns the successor this token is exchanged for: a new token in the
// same chain, expiring when the chain does.
//
// The ceiling travels rather than restarting, so refreshing does not extend a
// grant indefinitely. A person who approved an app thirty days ago is asked
// again, however often the app refreshed in between.
func (g RefreshGrant) Rotate(
	id RefreshTokenID, at time.Time, random io.Reader,
) (RefreshGrant, RefreshToken, error) {
	if g.state != RefreshActive {
		return RefreshGrant{}, RefreshToken{}, fmt.Errorf(
			"%w: %s is %s", ErrInvalidRefreshToken, g.id, g.state)
	}

	if err := validateServiceID(string(id), refreshTokenPrefix); err != nil {
		return RefreshGrant{}, RefreshToken{}, err
	}

	if !at.Before(g.expiresAt) {
		return RefreshGrant{}, RefreshToken{}, fmt.Errorf(
			"%w: %s has expired", ErrInvalidRefreshToken, g.id)
	}

	token, err := mintRefreshToken(random)
	if err != nil {
		return RefreshGrant{}, RefreshToken{}, err
	}

	next := g
	next.id, next.digest, next.createdAt = id, token.Digest(), at.UTC()

	return next, token, nil
}

// String renders a grant without its material.
func (g RefreshGrant) String() string {
	return fmt.Sprintf("refresh token %s of %s for %s to %s in %s (%s, expires %s)",
		g.id, g.chain, g.user, g.client, g.project, g.state, g.expiresAt.Format(time.RFC3339))
}

// GoString redacts too, because %#v reaches the unexported fields directly.
func (g RefreshGrant) GoString() string { return g.String() }
