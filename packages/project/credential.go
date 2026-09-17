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

// ClientSecret is the plaintext a client application presents once. Its field is
// unexported and mintClientSecret is the only code that sets it, so an
// operator-chosen secret is not expressible and the entropy the single SHA-256
// below rests on holds by construction rather than by convention.
type ClientSecret struct {
	raw string
}

// Reveal returns the secret in the clear, for the one handover at issuance. It
// is the only plaintext path, so every such read is one grep away.
func (s ClientSecret) Reveal() string {
	return s.raw
}

// String redacts, so a secret cannot reach a log line through ordinary
// formatting and Reveal stays the only way to read one.
func (s ClientSecret) String() string {
	if s.raw == "" {
		return ""
	}

	return "[redacted client secret]"
}

// GoString redacts too, because %#v prints the value rather than String and
// would otherwise spell the secret out in a debug line.
func (s ClientSecret) GoString() string {
	return `"` + s.String() + `"`
}

// MarshalJSON redacts, so a secret cannot reach a JSON log handler or a response
// body through a struct that merely happens to carry one.
func (s ClientSecret) MarshalJSON() ([]byte, error) {
	return []byte(`"` + s.String() + `"`), nil
}

// isSet reports whether this value carries a secret at all.
func (s ClientSecret) isSet() bool {
	return s.raw != ""
}

// hash is what the credential row keeps. A single SHA-256 is enough because the
// type above admits nothing but the bytes mintClientSecret drew.
func (s ClientSecret) hash() CredentialHash {
	sum := sha256.Sum256([]byte(s.raw))

	return CredentialHash(hex.EncodeToString(sum[:]))
}

// mintClientSecret draws a secret from the reader. A short or failed read mints
// nothing rather than a secret an attacker could predict.
func mintClientSecret(random io.Reader) (ClientSecret, error) {
	raw := make([]byte, clientSecretBytes)
	if _, err := io.ReadFull(random, raw); err != nil {
		return ClientSecret{}, fmt.Errorf("project: cannot mint a client secret: %w", err)
	}

	return ClientSecret{raw: base64.RawURLEncoding.EncodeToString(raw)}, nil
}

// CredentialHash is the stored digest. Its zero value means no live secret,
// which only a revoked credential may carry.
type CredentialHash string

// String redacts, mirroring ClientSecret, so a digest cannot reach a log line
// through ordinary formatting.
func (h CredentialHash) String() string {
	if h == "" {
		return ""
	}

	return "[redacted credential hash]"
}

// GoString redacts too, because %#v prints the value rather than String.
func (h CredentialHash) GoString() string {
	return `"` + h.String() + `"`
}

// MarshalJSON redacts, so a digest cannot reach a JSON log handler through a
// struct that merely happens to carry one.
func (h CredentialHash) MarshalJSON() ([]byte, error) {
	return []byte(`"` + h.String() + `"`), nil
}

// isSet reports whether live material is on file. A real digest is never empty.
func (h CredentialHash) isSet() bool {
	return h != ""
}

// CredentialID identifies one issued secret in one application's history.
type CredentialID string

// CredentialState is an issued secret's position in its life. Superseded is a
// rotation's outgoing half, so an authentication against it is a different fact
// from one against the current secret. Revoked is terminal and holds nothing.
type CredentialState string

// The lifecycle states of an issued credential.
const (
	CredentialActive     CredentialState = "active"
	CredentialSuperseded CredentialState = "superseded"
	CredentialRevoked    CredentialState = "revoked"
)

// Valid reports whether the state is one this server recognises.
func (s CredentialState) Valid() bool {
	switch s {
	case CredentialActive, CredentialSuperseded, CredentialRevoked:
		return true
	default:
		return false
	}
}

// CanTransitionTo reports whether the lifecycle allows this move. A superseded
// credential never returns to active: a rotation runs one way.
func (s CredentialState) CanTransitionTo(next CredentialState) bool {
	if s == next || !next.Valid() {
		return false
	}

	switch s {
	case CredentialActive:
		return next == CredentialSuperseded || next == CredentialRevoked
	case CredentialSuperseded:
		return next == CredentialRevoked
	default:
		return false
	}
}

// CredentialConfig asks for a credential. It carries no hash and no state: both
// come from IssueCredential, so a caller cannot ask for one that already holds a
// secret it chose or is already revoked.
type CredentialConfig struct {
	ID        CredentialID
	CreatedAt time.Time
	ExpiresAt time.Time
}

// validate refuses a lifetime the table would refuse: one that never begins, one
// that ends before it began, and one that outlives the ceiling.
func (c CredentialConfig) validate() error {
	if c.CreatedAt.IsZero() {
		return fmt.Errorf("%w: %s is issued at no instant", ErrInvalidCredentialExpiry, c.ID)
	}

	if !c.ExpiresAt.After(c.CreatedAt) {
		return fmt.Errorf("%w: %s expires before it is issued", ErrInvalidCredentialExpiry, c.ID)
	}

	if c.ExpiresAt.Sub(c.CreatedAt) > maxCredentialLifetime {
		return fmt.Errorf("%w: %s outlives %s", ErrInvalidCredentialExpiry, c.ID, maxCredentialLifetime)
	}

	return nil
}

// Credential is one issued secret's record. It holds the hash and never the
// plaintext, which exists only as a return value at issuance.
type Credential struct {
	id        CredentialID
	project   ID
	client    ClientApplicationID
	hash      CredentialHash
	state     CredentialState
	createdAt time.Time
	expiresAt time.Time
	revokedAt time.Time
}

// CredentialRecord is the shape a store rehydrates a persisted row through, and
// the only way a Credential is built outside an issuance.
type CredentialRecord struct {
	ID        CredentialID
	Client    ClientApplicationID
	Hash      CredentialHash
	State     CredentialState
	CreatedAt time.Time
	ExpiresAt time.Time
	RevokedAt time.Time
}

// NewCredential rebuilds a persisted credential and refuses every row the table
// refuses: a live one holding no secret, a revoked one still holding material,
// and a lifetime that never ends or ends before it began.
func NewCredential(owner ID, rec CredentialRecord) (Credential, error) {
	if err := ValidateID(owner); err != nil {
		return Credential{}, err
	}

	if err := validateServiceID(string(rec.ID), credentialPrefix); err != nil {
		return Credential{}, err
	}

	if err := ValidateClientApplicationID(rec.Client); err != nil {
		return Credential{}, err
	}

	if !rec.State.Valid() {
		return Credential{}, fmt.Errorf("%w: %q", ErrUnknownState, string(rec.State))
	}

	if err := rec.validateMaterial(); err != nil {
		return Credential{}, err
	}

	// A revoked row keeps the expiry it died with, which may already have passed,
	// so the lifetime is re-checked only while the credential can still answer.
	if rec.State != CredentialRevoked {
		cfg := CredentialConfig{ID: rec.ID, CreatedAt: rec.CreatedAt, ExpiresAt: rec.ExpiresAt}
		if err := cfg.validate(); err != nil {
			return Credential{}, err
		}
	}

	return Credential{
		id: rec.ID, project: owner, client: rec.Client, hash: rec.Hash, state: rec.State,
		createdAt: rec.CreatedAt.UTC(), expiresAt: rec.ExpiresAt.UTC(), revokedAt: rec.RevokedAt.UTC(),
	}, nil
}

// validateMaterial pairs the state against the secret, which is what the row's
// two mirrored CHECKs state and no column states alone.
func (r CredentialRecord) validateMaterial() error {
	if r.State == CredentialRevoked {
		if r.Hash.isSet() || r.RevokedAt.IsZero() {
			return fmt.Errorf("%w: %s is revoked", ErrInvalidSecretState, r.ID)
		}

		return nil
	}

	if !r.Hash.isSet() || !r.RevokedAt.IsZero() {
		return fmt.Errorf("%w: %s is %s", ErrInvalidSecretState, r.ID, r.State)
	}

	return nil
}

// ID returns the credential's identifier.
func (c Credential) ID() CredentialID {
	return c.id
}

// Project returns the Project the issuing registration belongs to.
func (c Credential) Project() ID {
	return c.project
}

// ClientApplication returns the registration this secret was issued for.
func (c Credential) ClientApplication() ClientApplicationID {
	return c.client
}

// State returns the credential's position in its life.
func (c Credential) State() CredentialState {
	return c.state
}

// CreatedAt returns the instant the secret was issued.
func (c Credential) CreatedAt() time.Time {
	return c.createdAt
}

// ExpiresAt returns the instant the secret stops answering.
func (c Credential) ExpiresAt() time.Time {
	return c.expiresAt
}

// RevokedAt returns the instant the secret was destroyed, if it was.
func (c Credential) RevokedAt() (time.Time, bool) {
	return c.revokedAt, !c.revokedAt.IsZero()
}

// HasSecret reports whether live material is on file. The hash itself is handed
// out only to the one statement that writes it.
func (c Credential) HasSecret() bool {
	return c.hash.isSet()
}

// StoredHash returns the material the insert binds. A credential a read rebuilt
// carries the store's sentinel rather than a hash, and the store refuses to
// write that, so this cannot persist a value nothing could ever match.
func (c Credential) StoredHash() CredentialHash {
	return c.hash
}

// Matches reports whether this is the secret on file and still live, compared in
// constant time so a wrong guess cannot be narrowed by how long the answer took.
// Liveness travels with the comparison, so no caller can compare without it.
func (c Credential) Matches(secret ClientSecret, now time.Time) bool {
	if !c.hash.isSet() || !secret.isSet() {
		return false
	}

	if c.state == CredentialRevoked || !now.Before(c.expiresAt) {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(c.hash), []byte(secret.hash())) == 1
}

// Supersede returns the credential as a rotation's outgoing half, dying at the
// instant given. The window is chosen here and can only shorten the life the
// credential already had, so a rotation cannot extend the secret it retires.
func (c Credential) Supersede(expiresAt time.Time) (Credential, error) {
	if !c.state.CanTransitionTo(CredentialSuperseded) {
		return Credential{}, fmt.Errorf("%w: %s to %s", ErrInvalidTransition, c.state, CredentialSuperseded)
	}

	if expiresAt.IsZero() || expiresAt.After(c.expiresAt) {
		return Credential{}, fmt.Errorf("%w: %s outlives its own expiry", ErrInvalidCredentialExpiry, c.id)
	}

	c.state, c.expiresAt = CredentialSuperseded, expiresAt.UTC()

	return c, nil
}

// Revoke returns the credential with its material gone. The hash goes with it,
// so a revoked credential matches nothing because it holds nothing.
func (c Credential) Revoke(at time.Time) (Credential, error) {
	if c.state == CredentialRevoked {
		return Credential{}, fmt.Errorf("%w: %s", ErrCredentialRevoked, c.id)
	}

	if at.IsZero() {
		return Credential{}, fmt.Errorf("%w: %s revoked at no instant", ErrInvalidSecretState, c.id)
	}

	c.state, c.hash, c.revokedAt = CredentialRevoked, "", at.UTC()

	return c, nil
}

// String renders a credential without its material. fmt cannot call String on an
// unexported field, so the hash would otherwise be spelled out by %v however
// carefully CredentialHash redacts itself.
func (c Credential) String() string {
	return fmt.Sprintf("credential %s for %s in %s (%s, secret on file %t)",
		c.id, c.client, c.project, c.state, c.hash.isSet())
}

// GoString redacts too, because %#v reaches the unexported field directly and
// prints what it finds there.
func (c Credential) GoString() string {
	return c.String()
}
