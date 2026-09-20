package project

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"strings"
	"time"
)

// identityModulus is the size of the key this server signs identity tokens
// with. 2048 is what RS256 is universally deployed at, and the floor ParseJWKS
// already refuses a registration for going under.
const identityModulus = 2048

// identityAlgorithm is the only algorithm an identity token is signed with.
//
// SMART App Launch requires RSA SHA-256 for identity tokens specifically, which
// is narrower than the RS384/ES384 this server verifies a client assertion
// with. The two are separate choices about separate tokens, and a client
// reading an identity token is entitled to assume this one.
const identityAlgorithm = "RS256"

// maxSubject is the length OpenID Connect gives a subject identifier.
const maxSubject = 255

// SigningKey is the private key this server signs identity tokens with.
//
// It is the counterpart of JWKS, which holds the public keys a *client*
// registered so this server can verify what that client signed. This is the
// other direction: material this server holds, and whose public half it
// publishes so a client can verify what this server signed.
//
// The zero value signs nothing, which is what a deployment that has minted none
// has. Sign says so rather than producing a token nobody can check.
type SigningKey struct {
	id  string
	key *rsa.PrivateKey
}

// NewSigningKey mints one.
//
// The identifier is the `kid` a token names and a reader resolves against the
// published set, so it has to survive into storage alongside the key itself.
func NewSigningKey(id string, random io.Reader) (SigningKey, error) {
	if strings.TrimSpace(id) == "" {
		return SigningKey{}, fmt.Errorf("%w: a signing key needs an identifier", ErrInvalidJWKS)
	}

	key, err := rsa.GenerateKey(random, identityModulus)
	if err != nil {
		return SigningKey{}, fmt.Errorf("project: mint a signing key: %w", err)
	}

	return SigningKey{id: id, key: key}, nil
}

// ParseSigningKey restores one from the PKCS#8 bytes Marshal wrote.
func ParseSigningKey(id string, encoded []byte) (SigningKey, error) {
	if strings.TrimSpace(id) == "" {
		return SigningKey{}, fmt.Errorf("%w: a signing key needs an identifier", ErrInvalidJWKS)
	}

	parsed, err := x509.ParsePKCS8PrivateKey(encoded)
	if err != nil {
		return SigningKey{}, fmt.Errorf("%w: %w", ErrInvalidJWKS, err)
	}

	key, held := parsed.(*rsa.PrivateKey)
	if !held {
		return SigningKey{}, fmt.Errorf(
			"%w: an identity token is signed with RSA, and this is %T", ErrInvalidJWKS, parsed)
	}

	// A key too short to sign with is one a reader should refuse, and refusing
	// it here means it never reaches a token.
	if key.N.BitLen() < identityModulus {
		return SigningKey{}, fmt.Errorf(
			"%w: a %d-bit key is under the %d an identity token is signed with",
			ErrInvalidJWKS, key.N.BitLen(), identityModulus)
	}

	return SigningKey{id: id, key: key}, nil
}

// ID returns the `kid` a token names.
func (k SigningKey) ID() string { return k.id }

// IsZero reports whether this is a deployment that has minted no key.
func (k SigningKey) IsZero() bool { return k.key == nil }

// Marshal returns the private key as PKCS#8, for a caller to seal and store.
//
// It hands back private material deliberately, and the only caller is the one
// writing the key to storage. Sealing these bytes rather than writing them is
// that caller's to do.
func (k SigningKey) Marshal() ([]byte, error) {
	if k.IsZero() {
		return nil, fmt.Errorf("%w: there is no key to marshal", ErrInvalidJWKS)
	}

	return x509.MarshalPKCS8PrivateKey(k.key)
}

// PublishedJWKS returns the public half as a JWK Set, for the jwks_uri.
//
// Only public members are written, and there is no path here that could emit
// `d`, `p` or `q`: the document is built from the modulus and the exponent
// rather than from the key.
func (k SigningKey) PublishedJWKS() (string, error) {
	if k.IsZero() {
		return "", fmt.Errorf("%w: there is no key to publish", ErrInvalidJWKS)
	}

	document, err := json.Marshal(map[string]any{
		"keys": []map[string]string{{
			"kty": "RSA",
			"use": "sig",
			"alg": identityAlgorithm,
			"kid": k.id,
			"n":   base64.RawURLEncoding.EncodeToString(k.key.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.key.E)).Bytes()),
		}},
	})
	if err != nil {
		return "", fmt.Errorf("project: publish the signing key: %w", err)
	}

	return string(document), nil
}

// IdentityClaims are what an identity token says about who signed in.
//
// Every field is required except FHIRUser, which is present when the approval
// granted `fhirUser` and absent otherwise: a claim naming a resource nobody
// approved reading is one this server would be volunteering.
type IdentityClaims struct {
	// Issuer is this server, and it must be the string the OpenID Connect
	// discovery document publishes: a reader fetches that document by appending
	// to the issuer it read here, and checks the two agree.
	Issuer string

	// Subject identifies the person to this client, stably and opaquely.
	Subject string

	// Audience is the client the token is for. A client that accepted one minted
	// for a different client would accept whatever another client handed it.
	Audience string

	// FHIRUser is the absolute URL of the Patient, Practitioner, RelatedPerson
	// or Person the subject acts as.
	FHIRUser string

	IssuedAt  time.Time
	ExpiresAt time.Time
}

// identityHeader is the JOSE header of an identity token.
type identityHeader struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
	KeyID     string `json:"kid"`
}

// identityBody is the claim set OpenID Connect requires, and the one claim
// SMART adds to it.
type identityBody struct {
	Issuer   string `json:"iss"`
	Subject  string `json:"sub"`
	Audience string `json:"aud"`
	Expires  int64  `json:"exp"`
	IssuedAt int64  `json:"iat"`
	FHIRUser string `json:"fhirUser,omitempty"`
}

// check refuses a claim set that would make a token nobody should accept.
func (c IdentityClaims) check() error {
	for name, value := range map[string]string{
		"iss": c.Issuer, "sub": c.Subject, "aud": c.Audience,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: an identity token needs %s", ErrInvalidJWKS, name)
		}
	}

	if len(c.Subject) > maxSubject {
		return fmt.Errorf("%w: a subject of %d characters exceeds the %d OpenID Connect allows",
			ErrInvalidJWKS, len(c.Subject), maxSubject)
	}

	if !c.ExpiresAt.After(c.IssuedAt) {
		return fmt.Errorf("%w: an identity token expiring at or before it was issued is one"+
			" no reader will accept", ErrInvalidJWKS)
	}

	return nil
}

// Sign mints an identity token: a compact JWS over the claims, RS256.
//
// The algorithm is fixed here rather than chosen by a caller. A signer that
// took one as a parameter is a signer somebody eventually passes "none" to.
func (k SigningKey) Sign(claims IdentityClaims) (string, error) {
	if k.IsZero() {
		return "", fmt.Errorf("%w: this deployment has minted no signing key", ErrInvalidJWKS)
	}

	if err := claims.check(); err != nil {
		return "", err
	}

	header, err := json.Marshal(identityHeader{
		Algorithm: identityAlgorithm, Type: "JWT", KeyID: k.id,
	})
	if err != nil {
		return "", fmt.Errorf("project: sign an identity token: %w", err)
	}

	body, err := json.Marshal(identityBody{
		Issuer: claims.Issuer, Subject: claims.Subject, Audience: claims.Audience,
		Expires: claims.ExpiresAt.Unix(), IssuedAt: claims.IssuedAt.Unix(),
		FHIRUser: claims.FHIRUser,
	})
	if err != nil {
		return "", fmt.Errorf("project: sign an identity token: %w", err)
	}

	signed := base64.RawURLEncoding.EncodeToString(header) +
		"." + base64.RawURLEncoding.EncodeToString(body)

	digest := sha256.Sum256([]byte(signed))

	signature, err := rsa.SignPKCS1v15(rand.Reader, k.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("project: sign an identity token: %w", err)
	}

	return signed + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}
