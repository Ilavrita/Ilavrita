package project

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"
)

// maxAssertionLifetime is how far ahead of now a client assertion may expire.
//
// SMART Backend Services says an assertion's lifetime should be short, and five
// minutes is what its own examples use. What the ceiling buys is bounded replay
// memory: a jti is remembered until the assertion naming it would have expired
// anyway, so a client that asked for a year would be asking this server to
// remember one string for a year.
const maxAssertionLifetime = 5 * time.Minute

// maxAssertion bounds the token itself, so a verifier cannot be made to parse a
// megabyte before deciding it is not a token.
const maxAssertion = 8192

// The signature and digest sizes this build verifies.
const (
	es384Signature = 96
	sha384Size     = 48
)

// ClientAssertion is a verified `private_key_jwt`: proof that whoever sent it
// holds the private key of a registration, for this server, once.
//
// It exists only as the result of VerifyClientAssertion. There is no way to
// build one from parts, because a value a caller could construct without a
// signature is one a caller could construct.
type ClientAssertion struct {
	client  ClientApplicationID
	id      string
	expires time.Time
}

// Client returns the registration the assertion proves.
func (a ClientAssertion) Client() ClientApplicationID { return a.client }

// ID returns the jti the assertion carried, which is what a replay is
// recognised by.
func (a ClientAssertion) ID() string { return a.id }

// ExpiresAt returns when the assertion stops being presentable, and therefore
// how long its jti has to be remembered.
func (a ClientAssertion) ExpiresAt() time.Time { return a.expires }

// assertionHeader is the JOSE header, read only for what decides verification.
type assertionHeader struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
	KeyID     string `json:"kid"`
}

// assertionClaims are the claims SMART Backend Services requires.
type assertionClaims struct {
	Issuer   string `json:"iss"`
	Subject  string `json:"sub"`
	Audience any    `json:"aud"`
	Expires  int64  `json:"exp"`
	IssuedAt int64  `json:"iat"`
	ID       string `json:"jti"`
}

// VerifyClientAssertion proves that an assertion was signed by a registration's
// own key, for this server, and has not expired.
//
// What it does not do is decide whether the jti has been seen before. That needs
// storage, and it is the caller's to record — but the jti and the expiry come
// back here, so the caller has what it needs and cannot invent either.
//
// audience is the token endpoint's own URL. SMART requires the check, and the
// reason is the whole point of the flow: without it, an assertion a client made
// for one server is one that server can present to another as if it were the
// client.
func VerifyClientAssertion(
	token string, registered ClientApplication, audience string, now time.Time,
) (ClientAssertion, error) {
	if len(token) > maxAssertion {
		return ClientAssertion{}, fmt.Errorf(
			"%w: %d characters exceeds the %d an assertion carries",
			ErrInvalidAssertion, len(token), maxAssertion)
	}

	signed, signature, header, claims, err := splitAssertion(token)
	if err != nil {
		return ClientAssertion{}, err
	}

	// The algorithm is checked against what this server accepts, never taken
	// from the header as an instruction. A verifier that trusts the header is
	// one an attacker tells which algorithm to use: "none" verifies everything,
	// and an HMAC verifies against a public key anybody can read.
	if !slices.Contains(signingAlgorithms, header.Algorithm) {
		return ClientAssertion{}, fmt.Errorf(
			"%w: %q is not an algorithm this server verifies",
			ErrInvalidAssertion, header.Algorithm)
	}

	if header.Type != "" && !strings.EqualFold(header.Type, "JWT") {
		return ClientAssertion{}, fmt.Errorf("%w: %q is not a JWT", ErrInvalidAssertion, header.Type)
	}

	if err := claims.check(registered.ID(), audience, now); err != nil {
		return ClientAssertion{}, err
	}

	keys := registered.JWKS().KeysFor(header.KeyID)
	if len(keys) == 0 {
		return ClientAssertion{}, fmt.Errorf(
			"%w: %s registered no key this assertion names", ErrInvalidAssertion, registered.ID())
	}

	if !verifiedBy(keys, header.Algorithm, signed, signature) {
		return ClientAssertion{}, fmt.Errorf(
			"%w: the signature is not one %s's keys made", ErrInvalidAssertion, registered.ID())
	}

	return ClientAssertion{
		client:  registered.ID(),
		id:      claims.ID,
		expires: time.Unix(claims.Expires, 0).UTC(),
	}, nil
}

// splitAssertion reads the three parts of a compact JWS.
func splitAssertion(token string) ([]byte, []byte, assertionHeader, assertionClaims, error) {
	var (
		header assertionHeader
		claims assertionClaims
	)

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, nil, header, claims, fmt.Errorf(
			"%w: an assertion has three parts, and this has %d", ErrInvalidAssertion, len(parts))
	}

	rawHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, nil, header, claims, fmt.Errorf("%w: %w", ErrInvalidAssertion, err)
	}

	rawClaims, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, nil, header, claims, fmt.Errorf("%w: %w", ErrInvalidAssertion, err)
	}

	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, nil, header, claims, fmt.Errorf("%w: %w", ErrInvalidAssertion, err)
	}

	if err := json.Unmarshal(rawHeader, &header); err != nil {
		return nil, nil, header, claims, fmt.Errorf("%w: %w", ErrInvalidAssertion, err)
	}

	if err := json.Unmarshal(rawClaims, &claims); err != nil {
		return nil, nil, header, claims, fmt.Errorf("%w: %w", ErrInvalidAssertion, err)
	}

	// What was signed is the two encoded parts and the dot between them, as they
	// arrived — not a re-encoding of what they decoded to, which could differ.
	return []byte(parts[0] + "." + parts[1]), signature, header, claims, nil
}

// check holds the claims to what SMART Backend Services requires of them.
func (c assertionClaims) check(client ClientApplicationID, audience string, now time.Time) error {
	// iss and sub are both the client id. A client asserting somebody else's
	// identity in either is asserting something it cannot prove.
	if c.Issuer != string(client) || c.Subject != string(client) {
		return fmt.Errorf("%w: iss and sub name %q and %q, not %s",
			ErrInvalidAssertion, c.Issuer, c.Subject, client)
	}

	if c.ID == "" {
		return fmt.Errorf("%w: an assertion carries a jti, so it can be spent", ErrInvalidAssertion)
	}

	if !c.addresses(audience) {
		return fmt.Errorf("%w: aud does not name this server's token endpoint", ErrInvalidAssertion)
	}

	if c.Expires == 0 {
		return fmt.Errorf("%w: an assertion states when it expires", ErrInvalidAssertion)
	}

	expires := time.Unix(c.Expires, 0).UTC()

	if !now.Before(expires) {
		return fmt.Errorf("%w: the assertion expired at %s",
			ErrInvalidAssertion, expires.Format(time.RFC3339))
	}

	// A long-lived assertion is one whose jti this server would have to remember
	// for that long. The ceiling is what bounds that memory.
	if expires.Sub(now) > maxAssertionLifetime {
		return fmt.Errorf("%w: an assertion lives at most %s, and this one lives %s",
			ErrInvalidAssertion, maxAssertionLifetime, expires.Sub(now).Round(time.Second))
	}

	return nil
}

// addresses reports whether the audience claim names this server.
//
// RFC 7519 lets aud be a string or an array of them, so both are read; anything
// else is not an audience.
func (c assertionClaims) addresses(audience string) bool {
	switch held := c.Audience.(type) {
	case string:
		return held == audience
	case []any:
		for _, one := range held {
			if stated, text := one.(string); text && stated == audience {
				return true
			}
		}

		return false
	default:
		return false
	}
}

// verifiedBy reports whether any of the registered keys made this signature.
//
// Every key is tried, and the loop does not stop at the first success: stopping
// early makes the work depend on which key signed, and a registration holding
// two keys should not take measurably longer for one of them.
func verifiedBy(keys []crypto.PublicKey, algorithm string, signed, signature []byte) bool {
	digest := sha512.Sum384(signed)
	verified := false

	for _, key := range keys {
		switch algorithm {
		case "RS384":
			public, rsaKey := key.(*rsa.PublicKey)
			if rsaKey && rsa.VerifyPKCS1v15(public, crypto.SHA384, digest[:], signature) == nil {
				verified = true
			}

		case "ES384":
			public, ecKey := key.(*ecdsa.PublicKey)
			if ecKey && verifiedByECDSA(public, digest[:], signature) {
				verified = true
			}
		}
	}

	return verified
}

// verifiedByECDSA reads a JWS signature, which is r and s concatenated at fixed
// width rather than the ASN.1 encoding ecdsa.VerifyASN1 expects.
func verifiedByECDSA(public *ecdsa.PublicKey, digest, signature []byte) bool {
	if len(signature) != es384Signature || len(digest) != sha384Size {
		return false
	}

	r := new(big.Int).SetBytes(signature[:es384Signature/2])
	s := new(big.Int).SetBytes(signature[es384Signature/2:])

	return ecdsa.Verify(public, digest, r, s)
}
